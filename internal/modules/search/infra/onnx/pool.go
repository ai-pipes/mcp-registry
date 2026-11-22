package onnx

import "math"

// MaxTokens is the longest input: model_max_length of the tokenizer.
const MaxTokens = 512

// truncate cuts ids to MaxTokens AFTER the special tokens were added, as transformers.js does
// (truncateHelper sets array.length), so a text longer than the limit loses its closing </s>.
func truncate(ids []int64) []int64 {
	if len(ids) > MaxTokens {
		return ids[:MaxTokens]
	}
	return ids
}

// meanPool averages the n token rows of hidden (row-major, dim wide), every token attended (the
// mask is all ones for a single text). Arithmetic as transformers.js mean_pooling: per component
// the sum runs in float64 in token order, the division is in float64, the result is stored as
// float32. A float32 sum would differ in the last bits.
func meanPool(hidden []float32, n, dim int) []float32 {
	out := make([]float32, dim)
	count := float64(n)
	for d := range dim {
		var sum float64
		for k := range n {
			sum += float64(hidden[k*dim+d]) * 1
		}
		out[d] = float32(sum / count)
	}
	return out
}

// l2Normalize scales v to unit length in place, as Tensor.normalize of transformers.js: the sum of
// squares accumulates in a float32 (each step rounds to float32: acc = float32(acc + x*x), the
// square taken in float64), the norm is acc ** 0.5 stored as float32, and every component is
// divided by it in float64 and stored as float32.
func l2Normalize(v []float32) {
	var acc float32
	for _, x := range v {
		acc = float32(float64(acc) + float64(x)*float64(x))
	}
	nn := float32(math.Pow(float64(acc), 0.5))
	for i, x := range v {
		v[i] = float32(float64(x) / float64(nn))
	}
}
