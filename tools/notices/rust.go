package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const rustNotices = "third_party/tokenizers/NOTICES"

// rustTargets are the Rust targets of the published libtokenizers.a assets the image links.
var rustTargets = []string{"x86_64-unknown-linux-gnu", "aarch64-unknown-linux-gnu"}

// rustExtra names licence files below a crate's root that the crate redistributes: onig_sys
// compiles the bundled Oniguruma C library into the archive.
var rustExtra = map[string][]string{"onig_sys": {"oniguruma/COPYING"}}

type crate struct {
	Name, Version, License, Dir string
}

func run(dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // G204: the callers name git, cargo or pnpm with fixed arguments
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// linkedCrates lists the crates compiled into the tokenizers-ffi static library for the image's
// targets: normal dependencies only, without proc macros and their own dependencies (build scripts
// and macros run at compile time and are not linked), the FFI crate itself excluded (it is the Go
// module github.com/daulet/tokenizers).
func linkedCrates(checkout string) ([]crate, error) {
	ids := map[string]bool{}
	for _, target := range rustTargets {
		// Without dedupe and with depths, so that everything below a proc macro (its own
		// dependencies, used only by the compiler) is left out, wherever else it appears.
		out, err := run(checkout, "cargo", "tree", "--locked", "-p", "tokenizers-ffi", "-e", "normal",
			"--target", target, "--no-dedupe", "--prefix", "depth", "--format", "{p}")
		if err != nil {
			return nil, err
		}
		macroDepth := -1 // depth of the proc macro whose subtree is being skipped
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			i := strings.IndexFunc(line, func(r rune) bool { return r < '0' || r > '9' })
			if line == "" || i <= 0 {
				continue
			}
			depth, err := strconv.Atoi(line[:i])
			if err != nil {
				return nil, err
			}
			if macroDepth >= 0 && depth > macroDepth {
				continue
			}
			macroDepth = -1
			entry := line[i:]
			if strings.Contains(entry, "(proc-macro)") {
				macroDepth = depth
				continue
			}
			if strings.HasPrefix(entry, "tokenizers-ffi ") {
				continue
			}
			f := strings.Fields(entry) // name vX.Y.Z [(source)]
			ids[f[0]+" "+strings.TrimPrefix(f[1], "v")] = true
		}
	}
	out, err := run(checkout, "cargo", "metadata", "--locked", "--format-version", "1")
	if err != nil {
		return nil, err
	}
	var meta struct {
		Packages []struct {
			Name         string  `json:"name"`
			Version      string  `json:"version"`
			ManifestPath string  `json:"manifest_path"`
			License      *string `json:"license"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return nil, err
	}
	var crates []crate
	for _, p := range meta.Packages {
		if !ids[p.Name+" "+p.Version] {
			continue
		}
		delete(ids, p.Name+" "+p.Version)
		if !filepath.IsAbs(p.ManifestPath) {
			return nil, fmt.Errorf("cargo metadata: %s %s has no manifest path", p.Name, p.Version)
		}
		lic := "(not declared)"
		if p.License != nil {
			lic = *p.License
		}
		crates = append(crates, crate{Name: p.Name, Version: p.Version, License: lic, Dir: filepath.Dir(p.ManifestPath)})
	}
	if len(ids) > 0 {
		return nil, fmt.Errorf("cargo metadata does not know %v", ids)
	}
	slices.SortFunc(crates, func(a, b crate) int { return strings.Compare(a.Name+" "+a.Version, b.Name+" "+b.Version) })
	return crates, nil
}

func renderRust(root, checkout string) ([]byte, error) {
	p, err := readPins(root)
	if err != nil {
		return nil, err
	}
	tag := "v" + versionIn(p["TOK_BASE_URL"])
	got, err := run(checkout, "git", "describe", "--tags", "--exact-match")
	if err != nil || strings.TrimSpace(string(got)) != tag {
		return nil, fmt.Errorf("%s is not a checkout of the pinned tag %s (pins.env TOK_BASE_URL)", checkout, tag)
	}
	commit, err := run(checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	crates, err := linkedCrates(checkout)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	seen := texts{}
	b.WriteString("Rust crates compiled into libtokenizers.a (statically linked into /usr/local/bin/mcp-registry)\n")
	fmt.Fprintf(&b, gen, "rust <checkout>")
	fmt.Fprintf(&b, "\nSource: github.com/daulet/tokenizers %s (commit %s), crate tokenizers-ffi, normal\n"+
		"dependencies for %s, from its Cargo.lock. Each crate is listed with the licence its manifest\n"+
		"declares and the licence files it ships (for a choice such as \"MIT OR Apache-2.0\" the\n"+
		"recipient may use either). The FFI crate itself is the Go module github.com/daulet/tokenizers\n"+
		"(third_party/go/NOTICES).\n\n", tag, strings.TrimSpace(string(commit)), strings.Join(rustTargets, " and "))
	for _, c := range crates {
		files, err := licenceFiles(c.Dir)
		if err != nil {
			return nil, err
		}
		dir := c.Dir
		if len(files) == 0 {
			// A crate of a git workspace (tiktoken-rs) keeps its licence at the workspace root.
			if up, err := licenceFiles(filepath.Dir(c.Dir)); err == nil && len(up) > 0 {
				dir, files = filepath.Dir(c.Dir), up
			}
		}
		files = append(files, rustExtra[c.Name]...)
		if err := section(&b, seen, c.Name+" "+c.Version+" - "+c.License, dir, files); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

func writeRust(root, checkout string) error {
	data, err := renderRust(root, checkout)
	if err != nil {
		return err
	}
	return writeFile(root, filepath.FromSlash(rustNotices), data)
}
