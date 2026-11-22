package onnx

// DefaultLibraryPath is where the image puts the ONNX Runtime library (the file from the npm package
// onnxruntime-node@1.30.0). The image task owns the real location; this is the agreed default.
// It is a variable only so that a development build can point it elsewhere without a new setting:
// go build -tags onnx -ldflags "-X github.com/ai-pipes/mcp-registry/internal/modules/search/infra/onnx.DefaultLibraryPath=/path/to/libonnxruntime.dylib".
// Operators have no setting for it.
var DefaultLibraryPath = "/usr/local/lib/libonnxruntime.so.1"

// Options says where the native pieces are. Plain values: the composition reads the environment.
type Options struct {
	// ModelDir is the model root, the MODEL_DIR of the TypeScript server (default /app/models there):
	// the files are read from <ModelDir>/Xenova/multilingual-e5-small/tokenizer.json and
	// <ModelDir>/Xenova/multilingual-e5-small/onnx/model_quantized.onnx.
	ModelDir string
	// LibraryPath is the ONNX Runtime shared library; empty means DefaultLibraryPath.
	LibraryPath string
}

func (o Options) withDefaults() Options {
	if o.LibraryPath == "" {
		o.LibraryPath = DefaultLibraryPath
	}
	return o
}
