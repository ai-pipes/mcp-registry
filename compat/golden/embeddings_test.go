//go:build compat

// Package golden_test checks the shape of the recorded reference files that need no Docker, so a
// corrupted or truncated golden is caught before a milestone depends on it.
package golden_test

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

const (
	embeddingTexts = 110 // 61 Gitea + 23 YouTrack passages, 24 queries, 2 long texts
	embeddingDims  = 384 // Xenova/multilingual-e5-small
)

type embedding struct {
	Text string    `json:"text"`
	IDs  []int     `json:"ids"`
	Vec  []float64 `json:"vec"`
}

func TestEmbeddingGoldensAreWellFormed(t *testing.T) {
	texts := map[string][]string{}
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			raw, err := os.ReadFile("embeddings-" + arch + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var rows []embedding
			if err := json.Unmarshal(raw, &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != embeddingTexts {
				t.Fatalf("%d entries, want %d", len(rows), embeddingTexts)
			}
			for i, r := range rows {
				if r.Text == "" || len(r.IDs) == 0 {
					t.Fatalf("entry %d: empty text %v or no token ids (%d)", i, r.Text == "", len(r.IDs))
				}
				if len(r.Vec) != embeddingDims {
					t.Fatalf("entry %d (%.40q): vector of %d numbers, want %d", i, r.Text, len(r.Vec), embeddingDims)
				}
				var sum float64
				for _, x := range r.Vec {
					if math.IsNaN(x) || math.IsInf(x, 0) {
						t.Fatalf("entry %d (%.40q): a vector value is not finite", i, r.Text)
					}
					sum += x * x
				}
				if norm := math.Sqrt(sum); math.Abs(norm-1) > 1e-3 {
					t.Fatalf("entry %d (%.40q): vector length %.6f, want 1 within 1e-3", i, r.Text, norm)
				}
				texts[arch] = append(texts[arch], r.Text)
			}
		})
	}
	// Both architectures embed the same texts in the same order and tokenize them identically;
	// only the vectors differ a little.
	if len(texts["arm64"]) == embeddingTexts && len(texts["amd64"]) == embeddingTexts {
		for i := range texts["arm64"] {
			if texts["arm64"][i] != texts["amd64"][i] {
				t.Fatalf("text %d differs between the architectures", i)
			}
		}
	}
}
