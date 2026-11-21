//go:build compat

package fakes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// EmbedderDimensions is the width of the vectors the application's OpenAI-compatible adapter
// accepts (a 384-dimension e5 model).
const EmbedderDimensions = 384

// EmbedRequest is a POST .../embeddings request the embedder received.
type EmbedRequest struct {
	Model  string      // "model" of the body
	Input  []string    // "input" of the body, exactly as sent (with the passage:/query: prefixes)
	Header http.Header // the HTTP request headers
	Status int         // what the embedder answered: 200, or 503 while it is down
}

// Embedder is an OpenAI-compatible embeddings endpoint: POST <URL>/embeddings with
// {"model": ..., "input": [...]} answers {"data": [{"index": i, "embedding": [...]}, ...]}, one
// 384-dimension unit vector per input (EmbeddingOf). It records every request.
type Embedder struct {
	URL string // http://host.docker.internal:<port>/v1: the value of EMBEDDER_URL

	mu       sync.Mutex
	down     bool
	requests []EmbedRequest
}

// StartEmbedder starts an embedder that stops with the test.
func StartEmbedder(t *testing.T) *Embedder {
	t.Helper()
	e := &Embedder{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/embeddings", e.embeddings)
	e.URL = baseURL(serve(t, mux)) + "/v1"
	return e
}

// SetDown makes the embedder answer 503 to every request (and ends it with false).
func (e *Embedder) SetDown(down bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.down = down
}

// Requests returns the requests received so far, in order.
func (e *Embedder) Requests() []EmbedRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]EmbedRequest, len(e.requests))
	copy(out, e.requests)
	return out
}

// embedInput is the "input" of an embeddings request: the OpenAI-compatible protocol allows one
// string or an array of strings; a single string is one input.
type embedInput []string

func (in *embedInput) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*in = embedInput{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*in = many
	return nil
}

func (e *Embedder) embeddings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string     `json:"model"`
		Input embedInput `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	status := http.StatusOK
	if e.down {
		status = http.StatusServiceUnavailable
	}
	e.requests = append(e.requests, EmbedRequest{Model: body.Model, Input: []string(body.Input), Header: r.Header.Clone(), Status: status})
	e.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != http.StatusOK {
		_, _ = w.Write([]byte(`{"error":"the embedder is down"}`))
		return
	}
	type item struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	data := make([]item, len(body.Input))
	for i, in := range body.Input {
		data[i] = item{Object: "embedding", Index: i, Embedding: EmbeddingOf(in)}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": body.Model, "data": data})
}

// EmbeddingOf is the vector the embedder returns for an input: a bag of words. Every word (letters
// and digits, lower case) adds +1 or -1 to one of the 384 dimensions chosen by its hash; the sum is
// scaled to length 1. The leading "passage: " or "query: " prefix is dropped first, so a query and a
// passage that share words are similar whatever their prefix. The same text always gives the same
// vector; an input without words gives the first unit vector.
func EmbeddingOf(input string) []float64 {
	text := strings.TrimPrefix(strings.TrimPrefix(input, "passage: "), "query: ")
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	v := make([]float64, EmbedderDimensions)
	for _, w := range words {
		sum := sha256.Sum256([]byte(w))
		dim := int(binary.BigEndian.Uint32(sum[:4]) % EmbedderDimensions)
		if sum[4]&1 == 0 {
			v[dim]++
		} else {
			v[dim]--
		}
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= norm
	}
	return v
}
