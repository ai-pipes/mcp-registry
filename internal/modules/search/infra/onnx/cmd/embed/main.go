//go:build onnx

// Command embed prints the vectors the local embedder makes, for comparing with the reference
// (testdata/gen/compare.mjs). It is a developer tool: it reads texts, embeds each one exactly as
// the server does (one per run) and writes [{"i":0,"ids":[...],"vec":[...]}] to standard output.
//
//	go run -tags onnx ./internal/modules/search/infra/onnx/cmd/embed \
//	    -lib <libonnxruntime> -model-dir <model root> [-mode raw|passage|query] [-texts file] < texts.json
//
// The texts are a JSON array of strings, or testdata/reference.json (its "cases"); from standard
// input unless -texts names a file. -mode raw (the default) embeds a text as given; passage and
// query add the prefix, as EmbedPassage and EmbedQuery do.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/infra/onnx"
)

type row struct {
	I   int       `json:"i"`
	IDs []int64   `json:"ids"`
	Vec []float32 `json:"vec"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embed:", err)
		os.Exit(1)
	}
}

func run() error {
	lib := flag.String("lib", onnx.DefaultLibraryPath, "ONNX Runtime shared library")
	modelDir := flag.String("model-dir", "/app/models", "model root (holds Xenova/multilingual-e5-small)")
	mode := flag.String("mode", "raw", "raw | passage | query")
	textsFile := flag.String("texts", "", "file with the texts (default: standard input)")
	flag.Parse()

	var in io.Reader = os.Stdin
	if *textsFile != "" {
		f, err := os.Open(*textsFile)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	texts, err := readTexts(in)
	if err != nil {
		return err
	}
	prefix := ""
	switch *mode {
	case "raw":
	case "passage":
		prefix = onnx.PassagePrefix
	case "query":
		prefix = onnx.QueryPrefix
	default:
		return fmt.Errorf("unknown -mode %q", *mode)
	}

	e := onnx.NewLocal(onnx.Options{ModelDir: *modelDir, LibraryPath: *lib})
	defer func() { _ = e.Close() }()
	ctx := context.Background()
	if err := e.Load(ctx); err != nil {
		return err
	}
	rows := make([]row, len(texts))
	for i, text := range texts {
		res, err := e.EmbedDetailed(ctx, prefix+text)
		if err != nil {
			return fmt.Errorf("text %d: %w", i, err)
		}
		rows[i] = row{I: i, IDs: res.IDs, Vec: res.Vector}
	}
	return json.NewEncoder(os.Stdout).Encode(rows)
}

func readTexts(r io.Reader) ([]string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var list []string
	if json.Unmarshal(b, &list) == nil {
		return list, nil
	}
	var ref struct {
		Cases []struct {
			Text string `json:"text"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &ref); err != nil || len(ref.Cases) == 0 {
		return nil, fmt.Errorf("the input is neither a JSON array of strings nor a reference.json")
	}
	texts := make([]string, len(ref.Cases))
	for i, c := range ref.Cases {
		texts[i] = c.Text
	}
	return texts, nil
}
