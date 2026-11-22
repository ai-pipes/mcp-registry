//go:build !onnx

package onnx

import "context"

// Built reports whether this binary contains the local embedder (the onnx build tag).
const Built = false

// NewLocal returns an embedder that is never ready: every call answers ErrNotBuilt. The binary
// works without native libraries, with text search only.
func NewLocal(Options) *Embedder {
	return newEmbedder(func(context.Context) (Backend, error) { return Backend{}, ErrNotBuilt })
}
