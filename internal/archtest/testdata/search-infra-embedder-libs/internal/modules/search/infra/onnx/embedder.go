// Fixture: the local embedder's libraries are allowed in search/infra (and nowhere else).
package onnx

import (
	_ "github.com/daulet/tokenizers"
	_ "github.com/yalue/onnxruntime_go"
	_ "golang.org/x/text/unicode/norm"
)
