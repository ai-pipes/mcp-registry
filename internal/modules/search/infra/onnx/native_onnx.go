//go:build onnx

package onnx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// Built reports whether this binary contains the local embedder (the onnx build tag).
const Built = true

// NewLocal returns the local embedder. Nothing is loaded until Ready, Load or an Embed call asks
// for it; a missing model directory or library is a failed load (Ready stays false, Embed returns
// the error), never a panic. Close it to release the native resources.
func NewLocal(opts Options) *Embedder {
	opts = opts.withDefaults()
	return newEmbedder(func(context.Context) (Backend, error) { return loadNative(opts) })
}

func loadNative(opts Options) (Backend, error) {
	dir := filepath.Join(opts.ModelDir, ModelID)
	tokenizerPath := filepath.Join(dir, "tokenizer.json")
	modelPath := filepath.Join(dir, "onnx", "model_quantized.onnx")
	// Plain files first: a clear error beats whatever the native libraries say about a bad path.
	for _, p := range []string{tokenizerPath, modelPath} {
		info, err := os.Stat(p)
		if err != nil {
			return Backend{}, fmt.Errorf("the model file %s is not readable: %w", p, err)
		}
		if info.IsDir() {
			return Backend{}, fmt.Errorf("the model file %s is a directory", p)
		}
	}
	data, err := os.ReadFile(tokenizerPath) //nolint:gosec // G304: the path is the operator's MODEL_DIR, not request input
	if err != nil {
		return Backend{}, fmt.Errorf("reading %s: %w", tokenizerPath, err)
	}
	tk, err := tokenizers.FromBytes(data)
	if err != nil {
		return Backend{}, fmt.Errorf("loading the tokenizer: %w", err)
	}
	if err := acquireRuntime(opts.LibraryPath); err != nil {
		_ = tk.Close()
		return Backend{}, err
	}
	session, err := newSession(modelPath)
	if err != nil {
		releaseRuntime()
		_ = tk.Close()
		return Backend{}, err
	}
	return Backend{Tokenizer: &nativeTokenizer{tk: tk}, Session: session}, nil
}

// nativeTokenizer adds the special tokens and returns the ids; the Embedder cuts them.
type nativeTokenizer struct{ tk *tokenizers.Tokenizer }

func (t *nativeTokenizer) Encode(text string) ([]int64, error) {
	ids, _, err := t.tk.EncodeErr(text, true)
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out, nil
}

func (t *nativeTokenizer) Close() error { return t.tk.Close() }

// nativeSession is one ONNX Runtime session with default options (no graph optimisation level, no
// thread settings: the vectors of the reference depend on it).
type nativeSession struct {
	mu      sync.Mutex
	session *ort.DynamicAdvancedSession
}

func newSession(modelPath string) (*nativeSession, error) {
	options, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("creating the session options: %w", err)
	}
	defer func() { _ = options.Destroy() }()
	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"}, options)
	if err != nil {
		return nil, fmt.Errorf("creating the ONNX Runtime session: %w", err)
	}
	return &nativeSession{session: session}, nil
}

// Run feeds one text: attention_mask all ones, token_type_ids all zeros, shape [1, n]. The Embedder
// already runs one call at a time; the mutex also keeps Close from racing a run.
func (s *nativeSession) Run(ids []int64) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run(ids)
}

// run is Run without the lock (the concurrency measurement in the tests calls it from several
// goroutines at once).
func (s *nativeSession) run(ids []int64) ([]float32, error) {
	if s.session == nil {
		return nil, errors.New("the session is closed")
	}
	n := int64(len(ids))
	shape := ort.NewShape(1, n)
	mask := make([]int64, n)
	for i := range mask {
		mask[i] = 1
	}
	inputIDs, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inputIDs.Destroy() }()
	attention, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer func() { _ = attention.Destroy() }()
	types, err := ort.NewTensor(shape, make([]int64, n))
	if err != nil {
		return nil, err
	}
	defer func() { _ = types.Destroy() }()
	outputs := []ort.Value{nil}
	if err := s.session.Run([]ort.Value{inputIDs, attention, types}, outputs); err != nil {
		return nil, err
	}
	defer func() { _ = outputs[0].Destroy() }()
	hidden, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, errors.New("last_hidden_state is not a float32 tensor")
	}
	return append([]float32(nil), hidden.GetData()...), nil
}

func (s *nativeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil
	}
	err := s.session.Destroy()
	s.session = nil
	releaseRuntime()
	return err
}

// ONNX Runtime's environment is global to the process: the first session initialises it with a
// library path, the last one destroys it. A second library path while it is up is refused.
var ortRuntime struct {
	mu   sync.Mutex
	path string
	refs int
}

func acquireRuntime(path string) error {
	ortRuntime.mu.Lock()
	defer ortRuntime.mu.Unlock()
	if ortRuntime.refs > 0 {
		if ortRuntime.path != path {
			return fmt.Errorf("ONNX Runtime is already loaded from %s, not %s", ortRuntime.path, path)
		}
		ortRuntime.refs++
		return nil
	}
	ort.SetSharedLibraryPath(path)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("initialising ONNX Runtime from %s: %w", path, err)
	}
	ortRuntime.path, ortRuntime.refs = path, 1
	return nil
}

func releaseRuntime() {
	ortRuntime.mu.Lock()
	defer ortRuntime.mu.Unlock()
	if ortRuntime.refs == 0 {
		return
	}
	ortRuntime.refs--
	if ortRuntime.refs == 0 {
		_ = ort.DestroyEnvironment()
	}
}
