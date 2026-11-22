//go:build !onnx

package onnx

import (
	"errors"
	"testing"
)

// The default build has no native code: the embedder is never ready and says why, once and the same
// way every time, without touching any file.
func TestTheBuildWithoutTheTagHasAnEmbedderThatIsNeverReady(t *testing.T) {
	if Built {
		t.Fatal("Built is true without the onnx tag")
	}
	e := NewLocal(Options{ModelDir: "/does/not/exist"})
	defer func() { _ = e.Close() }()
	if e.Ready(t.Context()) {
		t.Fatal("ready")
	}
	for range 3 {
		if _, err := e.EmbedPassage(t.Context(), "x"); !errors.Is(err, ErrNotBuilt) {
			t.Fatalf("err = %v, want ErrNotBuilt", err)
		}
	}
	if err := e.Load(t.Context()); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Load = %v, want ErrNotBuilt", err)
	}
}
