//go:build onnx

package onnx

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The tests of this file need the native libraries and the model: `make go-test-onnx` fetches
// them and sets the variables below. Without them the tests that need them are skipped (the only
// reason to skip is that environment, as in the other integration tests of the repository).
//
//	ONNX_TEST_LIB             the ONNX Runtime 1.30.0 shared library (from onnxruntime-node@1.30.0)
//	ONNX_TEST_MODEL_DIR       the model root, holding Xenova/multilingual-e5-small/...
//	ONNX_TEST_REFERENCE_VECTORS  optional: vectors of the reference (testdata/gen/vectors.mjs) made
//	                          on the SAME machine or image architecture as this run
func nativeOptions(t testing.TB) Options {
	t.Helper()
	lib, dir := os.Getenv("ONNX_TEST_LIB"), os.Getenv("ONNX_TEST_MODEL_DIR")
	if lib == "" || dir == "" {
		t.Skip("set ONNX_TEST_LIB and ONNX_TEST_MODEL_DIR (make go-test-onnx does)")
	}
	return Options{ModelDir: dir, LibraryPath: lib}
}

func newLocal(t testing.TB) *Embedder {
	t.Helper()
	e := NewLocal(nativeOptions(t))
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Load(context.Background()); err != nil {
		t.Fatalf("loading the model: %v", err)
	}
	return e
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTokenIDsMatchTheReference runs every reference text through the production path (normaliser,
// NUL substitution, daulet tokenizer, 512 cut) and compares the ids with the reference's. Token ids
// do not depend on the CPU. The classes the port cannot match are lone surrogates (they cannot be a
// Go string; the generator leaves them out) and Unicode newer than the Go tables.
func TestTokenIDsMatchTheReference(t *testing.T) {
	e := newLocal(t)
	ref := loadReference(t)
	var bad int
	for i, c := range ref.Cases {
		res, err := e.EmbedDetailed(t.Context(), c.Text)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !sameIDs(res.IDs, c.IDs) {
			bad++
			if bad <= 5 {
				t.Errorf("case %d (%s) %q: ids %v, reference %v", i, c.Group, c.Text, head(res.IDs), head(c.IDs))
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d texts tokenise differently from the reference", bad, len(ref.Cases))
	}
	t.Logf("%d of %d texts: ids identical to the reference (%s, Unicode %s)", len(ref.Cases)-bad, len(ref.Cases), ref.Node, ref.Unicode)
}

func head(ids []int64) []int64 {
	if len(ids) > 12 {
		return ids[:12]
	}
	return ids
}

// TestVectorsMatchTheReferenceBitForBit compares with vectors Node made with the same ONNX Runtime
// build on the same machine (or image architecture): bit identical is the requirement, not 1e-6.
func TestVectorsMatchTheReferenceBitForBit(t *testing.T) {
	path := os.Getenv("ONNX_TEST_REFERENCE_VECTORS")
	if path == "" {
		t.Skip("set ONNX_TEST_REFERENCE_VECTORS to a file made by testdata/gen/vectors.mjs on this machine")
	}
	e := newLocal(t)
	ref := loadReference(t)
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file named by the developer through the environment
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		I   int       `json:"i"`
		Vec []float64 `json:"vec"`
	}
	if err := json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(ref.Cases) {
		t.Fatalf("%d reference vectors for %d texts", len(vectors), len(ref.Cases))
	}
	var identical int
	var worst float64
	for _, rv := range vectors {
		got, err := e.Embed(t.Context(), ref.Cases[rv.I].Text)
		if err != nil {
			t.Fatal(err)
		}
		var diff float64
		for k := range got {
			diff = math.Max(diff, math.Abs(float64(got[k])-float64(float32(rv.Vec[k]))))
		}
		if diff == 0 {
			identical++
		} else if worst < diff {
			worst = diff
			t.Logf("text %d differs by %g", rv.I, diff)
		}
	}
	t.Logf("%d of %d vectors bit identical, max difference %g", identical, len(vectors), worst)
	if identical != len(vectors) {
		t.Fatalf("%d of %d vectors differ from the reference (max %g)", len(vectors)-identical, len(vectors), worst)
	}
}

func TestLoadReadyAndCloseOnTheRealModel(t *testing.T) {
	e := NewLocal(nativeOptions(t))
	if e.Ready(t.Context()) {
		t.Fatal("ready before the load finished: Ready must start the load and not wait for it")
	}
	if err := e.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !e.Ready(t.Context()) {
		t.Fatal("not ready after Load")
	}
	v, err := e.EmbedQuery(t.Context(), "create an issue")
	if err != nil || len(v) != Dimensions {
		t.Fatalf("%d numbers, %v", len(v), err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The process-wide environment is torn down with the last embedder and comes back for the next.
	again := NewLocal(nativeOptions(t))
	defer func() { _ = again.Close() }()
	v2, err := again.EmbedQuery(t.Context(), "create an issue")
	if err != nil {
		t.Fatal(err)
	}
	for i := range v {
		if v[i] != v2[i] {
			t.Fatalf("the vector changed after a restart at %d: %v vs %v", i, v[i], v2[i])
		}
	}
}

func TestAMissingModelDirectoryOrLibraryIsAFailedLoadNotAPanic(t *testing.T) {
	good := nativeOptions(t)
	cases := []struct {
		name string
		opts Options
	}{
		{"model directory missing", Options{ModelDir: filepath.Join(t.TempDir(), "nope"), LibraryPath: good.LibraryPath}},
		{"model directory empty", Options{ModelDir: t.TempDir(), LibraryPath: good.LibraryPath}},
		{"library missing", Options{ModelDir: good.ModelDir, LibraryPath: filepath.Join(t.TempDir(), "libonnxruntime.so.1")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewLocal(c.opts)
			defer func() { _ = e.Close() }()
			if e.Ready(t.Context()) {
				t.Fatal("ready")
			}
			if err := e.Load(t.Context()); err == nil {
				t.Fatal("Load succeeded")
			}
			if _, err := e.EmbedPassage(t.Context(), "x"); err == nil {
				t.Fatal("Embed succeeded")
			}
			if e.Ready(t.Context()) {
				t.Fatal("ready after a failed load")
			}
		})
	}
	// a failed load leaves nothing behind: a good embedder still works
	e := newLocal(t)
	if _, err := e.EmbedQuery(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
}

func sample(t testing.TB, n int) []string {
	t.Helper()
	ref := loadReference(t)
	var texts []string
	for _, c := range ref.Cases {
		if c.Group == "curated" {
			texts = append(texts, c.Text)
		}
		if len(texts) == n {
			break
		}
	}
	return texts
}

// TestConcurrentCallsGiveTheSameVectorsAsSequentialOnes runs the production path from several
// goroutines (go test -race watches the Go side) and compares with a sequential pass.
func TestConcurrentCallsGiveTheSameVectorsAsSequentialOnes(t *testing.T) {
	e := newLocal(t)
	texts := sample(t, 100)
	want := make([][]float32, len(texts))
	for i, text := range texts {
		v, err := e.Embed(t.Context(), text)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = v
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range texts {
				i := (k*7 + g*13) % len(texts)
				v, err := e.Embed(t.Context(), texts[i])
				if err != nil {
					t.Error(err)
					return
				}
				for d := range v {
					if v[d] != want[i][d] {
						t.Errorf("goroutine %d text %d: vector differs at %d", g, i, d)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// TestUnserialisedRunsOnOneSessionAreBitIdentical is the measurement behind the decision to
// serialise anyway: concurrent Run calls on ONE session (no Embedder lock) with default options
// are safe and bit identical here. The Embedder still serialises (one text per run, and ORT already
// spreads a run over the cores), so this documents the finding rather than a requirement.
func TestUnserialisedRunsOnOneSessionAreBitIdentical(t *testing.T) {
	opts := nativeOptions(t)
	backend, err := loadNative(opts.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeBackend(backend) }()
	session := backend.Session.(*nativeSession)
	texts := sample(t, 60)
	idsOf := make([][]int64, len(texts))
	want := make([][]float32, len(texts))
	for i, text := range texts {
		ids, err := backend.Tokenizer.Encode(PrepareText(text))
		if err != nil {
			t.Fatal(err)
		}
		idsOf[i] = truncate(ids)
		if want[i], err = session.run(idsOf[i]); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range texts {
				i := (k*7 + g*13) % len(texts)
				// the tokenizer handle is shared too
				ids, err := backend.Tokenizer.Encode(PrepareText(texts[i]))
				if err != nil || !sameIDs(truncate(ids), idsOf[i]) {
					t.Errorf("goroutine %d text %d: tokenizing differs (%v)", g, i, err)
					return
				}
				h, err := session.run(idsOf[i])
				if err != nil {
					t.Error(err)
					return
				}
				for d := range h {
					if h[d] != want[i][d] {
						t.Errorf("goroutine %d text %d: hidden state differs at %d", g, i, d)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
