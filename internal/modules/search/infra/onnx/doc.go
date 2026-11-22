// Package onnx is the local embedder of the search module: multilingual-e5-small (q8) run by ONNX
// Runtime, reproducing the vectors of the TypeScript server (transformers.js) bit for bit.
//
// The package is split by build tag so that the default build needs no native library:
//
//   - no tag: the normaliser (a port of the JavaScript one), truncation, mean pooling, L2
//     normalisation and the Embedder's lifecycle behind two small interfaces (Tokenizer, Session).
//     It is pure Go and always built and tested.
//   - tag onnx (cgo): the real Tokenizer (github.com/daulet/tokenizers) and Session
//     (github.com/yalue/onnxruntime_go), and NewLocal.
//   - tag !onnx: NewLocal returns an Embedder that is never ready and answers ErrNotBuilt.
//
// What makes the vectors identical to the reference (measured in the embedder spike, each is
// mandatory): the ONNX Runtime library is the exact 1.30.0 build the reference loads; default
// session options; one text per run, never a batch; mean pooling summed in float64 in token
// order; L2 norm with a float32 accumulator; the 512-token cut after the special tokens were
// added; the JavaScript normaliser run before the tokenizer.
package onnx
