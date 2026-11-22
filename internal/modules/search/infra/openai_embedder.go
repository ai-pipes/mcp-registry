package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

const (
	// embedTimeout bounds one request, response body included (AbortSignal.timeout(20_000) in
	// the TypeScript adapter).
	embedTimeout = 20 * time.Second
	// maxEmbedResponseBytes caps the response. The TypeScript adapter has no cap; a 384-number
	// answer is about 10 KB, so 4 MiB leaves room for any real server and stops a misbehaving one
	// from filling the memory.
	maxEmbedResponseBytes = 4 << 20
)

// OpenAIEmbedder is app.Embedder on an OpenAI-compatible `POST {url}/embeddings` endpoint serving
// a 384-dimension e5 model (EMBEDDER=openai-compatible).
//
// It sends what the TypeScript adapter sends: `Content-Type: application/json` and nothing that
// authenticates (there is no API key variable), a body {"model": ..., "input": ["<text>"]} with
// exactly one text per request (the q8 model of the local embedder is why one text per call is a
// rule; here it keeps every vector independent of its neighbours too), and the "passage: " /
// "query: " prefix in front of the text.
// Redirects are followed by net/http's rules, which match fetch's for the codes that matter
// (301, 302 and 303 turn the POST into a GET, 307 and 308 repeat it). Safe for concurrent use.
type OpenAIEmbedder struct {
	endpoint string
	model    string
	client   *http.Client
	timeout  time.Duration
}

var _ app.Embedder = (*OpenAIEmbedder)(nil)

// NewOpenAIEmbedder builds the adapter for EMBEDDER_URL and EMBEDDER_MODEL. client may be nil
// (a plain http.Client); the 20 s timeout is applied per request whatever client is given.
func NewOpenAIEmbedder(url, model string, client *http.Client) *OpenAIEmbedder {
	if client == nil {
		client = &http.Client{}
	}
	return &OpenAIEmbedder{
		endpoint: strings.TrimRight(url, "/") + "/embeddings",
		model:    model,
		client:   client,
		timeout:  embedTimeout,
	}
}

// Ready is always true: nothing is loaded here (an unreachable server shows up as an error of the
// call, as in TypeScript).
func (e *OpenAIEmbedder) Ready(context.Context) bool { return true }

// EmbedPassage embeds "passage: <text>".
func (e *OpenAIEmbedder) EmbedPassage(ctx context.Context, text string) ([]float32, error) {
	return e.embed(ctx, "passage: "+sendable(text))
}

// EmbedQuery embeds "query: <text>".
func (e *OpenAIEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return e.embed(ctx, "query: "+sendable(text))
}

// sendable replaces a trailing domain.SplitPairStandIn with U+FFFD. The stand-in marks the lone
// high surrogate JavaScript's slice leaves when a cut falls inside an emoji; the local embedder
// reads it as ordinary text, but TypeScript would put a lone-surrogate escape into the JSON,
// which many servers refuse, so this adapter sends the replacement character (a decided
// difference). Only the last rune is looked at: the stand-in appears nowhere else.
func sendable(text string) string {
	if body, ok := strings.CutSuffix(text, domain.SplitPairStandIn); ok {
		return body + "�"
	}
	return text
}

// embedError carries a fixed text (never a response body, the request text or the URL) and, for a
// timeout, the context error it came from.
type embedError struct {
	msg   string
	cause error
}

func (e *embedError) Error() string { return e.msg }
func (e *embedError) Unwrap() error { return e.cause }

var (
	errNotJSON         = &embedError{msg: "embedder response is not valid JSON"}
	errShape           = &embedError{msg: "embedder must return 1 vectors of " + strconv.Itoa(app.Dimensions) + " numbers"}
	errTooLarge        = &embedError{msg: "embedder response is too large"}
	errCredentialedURL = &embedError{msg: "embedder URL must not contain credentials"}
)

func (e *OpenAIEmbedder) embed(ctx context.Context, input string) ([]float32, error) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{e.model, []string{input}}); err != nil {
		return nil, &embedError{msg: "embedder request cannot be encoded"}
	}
	payload := bytes.TrimSuffix(body.Bytes(), []byte("\n")) // JSON.stringify has no trailing newline

	// Node's fetch refuses a URL with credentials, so the reference fails every call; Go's client
	// would send them as Basic authentication, which no code path of this adapter may do.
	if u, err := neturl.Parse(e.endpoint); err == nil && u.User != nil {
		return nil, errCredentialedURL
	}

	rctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &embedError{msg: "embedder URL is not valid"}
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := e.client.Do(req)
	if err != nil {
		return nil, e.transportError(ctx, rctx)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, &embedError{msg: "embedder responded with HTTP " + strconv.Itoa(res.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxEmbedResponseBytes+1))
	if err != nil {
		return nil, e.transportError(ctx, rctx)
	}
	if len(raw) > maxEmbedResponseBytes {
		return nil, errTooLarge
	}
	return parseEmbedding(raw)
}

// transportError classifies a failed round trip without the driver's text (it names the URL, which
// may carry credentials): the caller's own cancellation comes back as the context error, this
// adapter's timeout as an error that unwraps to context.DeadlineExceeded, the rest as a fixed text.
func (e *OpenAIEmbedder) transportError(caller, request context.Context) error {
	if err := caller.Err(); err != nil {
		return err
	}
	if errors.Is(request.Err(), context.DeadlineExceeded) {
		return &embedError{msg: "embedder request timed out", cause: context.DeadlineExceeded}
	}
	return &embedError{msg: "embedder request failed"}
}

// parseEmbedding accepts what the TypeScript schema accepts: {"data": [{"index"?: integer,
// "embedding": 384 numbers}]} with exactly one element (index only orders several); other keys are
// ignored. The vector is used as given (no normalisation).
func parseEmbedding(raw []byte) ([]float32, error) {
	if !json.Valid(raw) {
		return nil, errNotJSON
	}
	var res struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Data) != 1 {
		return nil, errShape
	}
	// A map keeps an explicit "index": null apart from an absent index (zod's optional() takes only
	// the latter).
	if idx, present := res.Data[0]["index"]; present {
		var n float64
		if err := json.Unmarshal(idx, &n); err != nil || idx[0] == 'n' || n != math.Trunc(n) {
			return nil, errShape
		}
	}
	var embedding []*float64
	if err := json.Unmarshal(res.Data[0]["embedding"], &embedding); err != nil || len(embedding) != app.Dimensions {
		return nil, errShape
	}
	out := make([]float32, app.Dimensions)
	for i, p := range embedding {
		if p == nil {
			return nil, errShape
		}
		f, ok := asFloat4(*p)
		if !ok {
			return nil, errShape
		}
		out[i] = f
	}
	return out, nil
}

// asFloat4 converts a JSON number the way the reference stores it: TypeScript prints the double's
// shortest decimal text and pgvector parses that text as float4. Rounding the double directly
// differs for a double exactly halfway between two float32 values (1.0000000596046448 is read as
// 1.0000001 from its text, as 1 when rounded to even). A value beyond float32 is refused; one that
// underflows becomes zero (a negative one is stored as 0, not -0, by the vector literal).
func asFloat4(v float64) (float32, bool) {
	f, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', -1, 64), 32)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return float32(f), true
}
