package onnx

import (
	"math"
	"testing"
)

func TestMeanPoolAveragesPerComponent(t *testing.T) {
	got := meanPool([]float32{1, 2, 3, 3, 4, 5}, 2, 3)
	want := []float32{2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("component %d = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
}

// The sum runs in float64 in token order (transformers.js mean_pooling); a float32 sum of
// 16777216, 1, 1 loses both ones (16777216 + 1 rounds back to 16777216) and gives 5592405.
func TestMeanPoolSumsInFloat64(t *testing.T) {
	got := meanPool([]float32{16777216, 1, 1}, 3, 1)
	if want := float32(5592406); got[0] != want {
		t.Fatalf("mean = %v, want %v", got[0], want)
	}
}

func TestL2NormalizeUnitLength(t *testing.T) {
	v := []float32{3, 4}
	l2Normalize(v)
	if v[0] != 0.6 || v[1] != 0.8 {
		t.Fatalf("normalised = %v, want [0.6 0.8]", v)
	}
	zero := []float32{0, 0}
	l2Normalize(zero)
	if !math.IsNaN(float64(zero[0])) {
		t.Logf("a zero vector divides by zero as the reference does: %v", zero)
	}
}

// TestPoolingMatchesTheReferenceArithmetic runs fixed hidden states through the port and compares
// bit for bit with what transformers.js (mean_pooling, then Tensor.normalize) makes of them. The
// fixture is CPU independent: it exercises the arithmetic only.
func TestPoolingMatchesTheReferenceArithmetic(t *testing.T) {
	cases := loadPooling(t)
	if len(cases) < 4 {
		t.Fatalf("pooling.json has %d cases", len(cases))
	}
	for _, c := range cases {
		hidden := decodeFloat32s(t, c.Hidden)
		want := decodeFloat32s(t, c.Vector)
		if len(hidden) != c.Length*c.Dim || len(want) != c.Dim {
			t.Fatalf("length %d: malformed fixture", c.Length)
		}
		got := meanPool(hidden, c.Length, c.Dim)
		l2Normalize(got)
		for i := range want {
			if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
				t.Fatalf("length %d component %d: %v (%#x), reference %v (%#x)", c.Length, i,
					got[i], math.Float32bits(got[i]), want[i], math.Float32bits(want[i]))
			}
		}
	}
}

func TestTruncateKeepsTheFirst512AndLosesTheClosingToken(t *testing.T) {
	ids := make([]int64, 600)
	for i := range ids {
		ids[i] = int64(i + 10)
	}
	ids[0], ids[599] = 0, 2 // <s> ... </s>
	got := truncate(ids)
	if len(got) != MaxTokens || got[0] != 0 {
		t.Fatalf("len %d, first %d", len(got), got[0])
	}
	for _, id := range got {
		if id == 2 {
			t.Fatal("</s> survived the cut: transformers.js cuts the array after the special tokens were added")
		}
	}
	short := []int64{0, 5, 2}
	if got := truncate(short); len(got) != 3 || got[2] != 2 {
		t.Fatalf("a short text must keep </s>: %v", got)
	}
	exact := make([]int64, MaxTokens)
	if got := truncate(exact); len(got) != MaxTokens {
		t.Fatalf("exactly 512 tokens must stay: %d", len(got))
	}
}
