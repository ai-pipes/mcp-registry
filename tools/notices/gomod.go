package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const goNotices = "third_party/go/NOTICES"

// goArches are the platforms the image is published for; the module set is their union.
var goArches = []string{"amd64", "arm64"}

type goModule struct{ Path, Version, Dir string }

// linkedModules lists the modules (other than this one) whose packages cmd/mcp-registry links when
// it is built as the image builds it: build tag onnx, cgo, linux, each published architecture.
func linkedModules(root string) ([]goModule, error) {
	seen := map[string]goModule{}
	for _, arch := range goArches {
		cmd := exec.Command("go", "list", "-deps", "-tags", "onnx",
			"-f", "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}", "./cmd/mcp-registry")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=1", "GOFLAGS=-mod=readonly")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list (linux/%s): %w", arch, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) != 3 {
				return nil, fmt.Errorf("go list: unexpected line %q", line)
			}
			seen[f[0]] = goModule{Path: f[0], Version: f[1], Dir: f[2]}
		}
	}
	mods := make([]goModule, 0, len(seen))
	for _, m := range seen {
		mods = append(mods, m)
	}
	slices.SortFunc(mods, func(a, b goModule) int { return strings.Compare(a.Path, b.Path) })
	return mods, nil
}

func goroot(root string) (string, error) {
	cmd := exec.Command("go", "env", "GOROOT")
	cmd.Dir = root
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// goHeading is the line that names a module in the file; the tests look for it.
func goHeading(m goModule) string { return m.Path + " " + m.Version }

// renderGo is the content of third_party/go/NOTICES.
func renderGo(root string) ([]byte, error) {
	mods, err := linkedModules(root)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	seen := texts{}
	b.WriteString("Go modules compiled into /usr/local/bin/mcp-registry\n")
	fmt.Fprintf(&b, gen, "go")
	b.WriteString("\nThe modules whose packages the binary links (build tag onnx; linux/amd64 and linux/arm64),\n" +
		"each with the licence files at the root of its module, then the licence of the Go standard\n" +
		"library and runtime, which every Go binary contains.\n\n")
	for _, m := range mods {
		files, err := licenceFiles(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Path, err)
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s %s has no licence file at its root: add its licence to THIRD_PARTY_NOTICES by hand and teach the tool", m.Path, m.Version)
		}
		if err := section(&b, seen, goHeading(m), m.Dir, files); err != nil {
			return nil, err
		}
	}
	gr, err := goroot(root)
	if err != nil {
		return nil, err
	}
	if err := section(&b, seen, "Go standard library and runtime (https://go.dev)", gr, []string{"LICENSE", "PATENTS"}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeGo(root string) error {
	data, err := renderGo(root)
	if err != nil {
		return err
	}
	return writeFile(root, filepath.FromSlash(goNotices), data)
}
