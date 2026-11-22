package onnx

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"testing"
)

// referenceCase is one row of testdata/reference.json, produced by testdata/gen/reference.mjs from
// the reference implementation (transformers.js + @huggingface/tokenizers).
type referenceCase struct {
	Group      string  `json:"group"`
	Text       string  `json:"text"`
	Normalized string  `json:"normalized"`
	IDs        []int64 `json:"ids"`
}

type referenceFile struct {
	Transformers           string          `json:"transformers"`
	Tokenizers             string          `json:"tokenizers"`
	Node                   string          `json:"node"`
	Unicode                string          `json:"unicode"`
	ExcludedLoneSurrogates int             `json:"excludedLoneSurrogates"`
	Cases                  []referenceCase `json:"cases"`
}

func loadReference(t testing.TB) referenceFile {
	t.Helper()
	b, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var f referenceFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

type poolingCase struct {
	Length int    `json:"length"`
	Dim    int    `json:"dim"`
	Hidden string `json:"hidden"`
	Vector string `json:"vector"`
}

func decodeFloat32s(t testing.TB, s string) []float32 {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return out
}

func loadPooling(t testing.TB) []poolingCase {
	t.Helper()
	b, err := os.ReadFile("testdata/pooling.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []poolingCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Cases
}
