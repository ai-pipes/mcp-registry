package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const ortDir = "third_party/onnxruntime"

// ortMembers are the files taken from the wheel, by their path inside it and their name in ortDir.
var ortMembers = [][2]string{
	{"onnxruntime/LICENSE", "LICENSE"},
	{"onnxruntime/ThirdPartyNotices.txt", "ThirdPartyNotices.txt"},
}

// ortSource is the content of third_party/onnxruntime/SOURCE: the version, where the files come
// from (by pin name, not by value: pins.env stays the only copy) and the SHA-256 of each file, which
// the tests check against the files.
func ortSource(version string, sums map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ONNX Runtime %s: its licence and third-party notices, taken unchanged from the PyPI wheel\n", version)
	b.WriteString("pinned in internal/modules/search/infra/onnx/native/pins.env (ORT_NOTICES_WHEEL_URL, SHA-256\n")
	b.WriteString("ORT_NOTICES_WHEEL_SHA256) by `go run ./tools/notices onnxruntime`. The image's\n")
	b.WriteString("libonnxruntime.so.1 is the same release (ORT_NPM_URL, the npm package carries no licence file).\n\n")
	for _, m := range ortMembers {
		fmt.Fprintf(&b, "%s  %s\n", sums[m[1]], m[1])
	}
	return b.String()
}

func writeONNXRuntime(root string) error {
	p, err := readPins(root)
	if err != nil {
		return err
	}
	url, want := p["ORT_NOTICES_WHEEL_URL"], p["ORT_NOTICES_WHEEL_SHA256"]
	version := versionIn(p["ORT_NPM_URL"])
	if url == "" || want == "" || version == "" {
		return fmt.Errorf("pins.env lacks ORT_NOTICES_WHEEL_URL, ORT_NOTICES_WHEEL_SHA256 or ORT_NPM_URL")
	}
	if !strings.HasPrefix(url, "https://") || !strings.Contains(filepath.Base(url), "onnxruntime-"+version+"-") {
		return fmt.Errorf("ORT_NOTICES_WHEEL_URL is not an https URL of the onnxruntime %s wheel", version)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	wheel, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(wheel)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("%s: sha256 %s, expected %s", url, got, want)
	}
	zr, err := zip.NewReader(bytes.NewReader(wheel), int64(len(wheel)))
	if err != nil {
		return err
	}
	sums := map[string]string{}
	for _, m := range ortMembers {
		f, err := zr.Open(m[0])
		if err != nil {
			return fmt.Errorf("%s is not in the wheel: %w", m[0], err)
		}
		data, err := io.ReadAll(io.LimitReader(f, 16<<20))
		_ = f.Close()
		if err != nil {
			return err
		}
		s := sha256.Sum256(data)
		sums[m[1]] = hex.EncodeToString(s[:])
		if err := writeFile(root, filepath.Join(filepath.FromSlash(ortDir), m[1]), data); err != nil {
			return err
		}
	}
	return writeFile(root, filepath.Join(filepath.FromSlash(ortDir), "SOURCE"), []byte(ortSource(version, sums)))
}
