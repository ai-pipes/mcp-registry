// Command notices regenerates the licence files under third_party/, which the Go image ships in
// /usr/share/licenses/mcp-registry/ next to THIRD_PARTY_NOTICES (the hand-written overview).
// Run from the repository root:
//
//	go run ./tools/notices go                  third_party/go/NOTICES: every Go module linked into
//	                                           cmd/mcp-registry (build tag onnx, linux/amd64 and
//	                                           linux/arm64) with its licence files, and the Go
//	                                           standard library's licence; offline once the module
//	                                           cache holds the modules (go mod download)
//	go run ./tools/notices web                 third_party/web/NOTICES: the production npm packages
//	                                           of the web UI (pnpm licenses list --prod --filter
//	                                           @mcp/web) with their licence files; needs pnpm install
//	go run ./tools/notices rust <checkout>     third_party/tokenizers/NOTICES: the Rust crates linked
//	                                           into libtokenizers.a, from a checkout of
//	                                           github.com/daulet/tokenizers at the pinned tag; needs
//	                                           git and cargo (cargo fetches the crate sources)
//	go run ./tools/notices onnxruntime         third_party/onnxruntime/{LICENSE,ThirdPartyNotices.txt,
//	                                           SOURCE}: ONNX Runtime's licence and third-party notices
//	                                           from the PyPI wheel pinned in pins.env, SHA-256 verified
//
// Nothing here runs at image build time: the files are committed, and tools/notices tests keep them
// current (the Go list exactly, the others against the pins and the web client's dependencies).
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "go":
		err = writeGo(".")
	case "web":
		err = writeWeb(".")
	case "rust":
		if len(os.Args) != 3 {
			usage()
		}
		err = writeRust(".", os.Args[2])
	case "onnxruntime":
		err = writeONNXRuntime(".")
	default:
		usage()
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func usage() {
	_, _ = fmt.Fprintln(os.Stderr, "usage: go run ./tools/notices go | web | rust <tokenizers checkout> | onnxruntime")
	os.Exit(2)
}
