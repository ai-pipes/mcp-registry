package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const webNotices = "third_party/web/NOTICES"

type npmPackage struct {
	Name, Version, License, Dir string
}

// webPackages lists the production npm packages of the web UI (what its bundle can contain), the
// workspace's own packages (@mcp/*, MIT, this repository) excluded.
func webPackages(root string) ([]npmPackage, error) {
	out, err := run(root, "pnpm", "licenses", "list", "--prod", "--filter", "@mcp/web", "--json")
	if err != nil {
		return nil, err
	}
	var byLicence map[string][]struct {
		Name     string   `json:"name"`
		Versions []string `json:"versions"`
		Paths    []string `json:"paths"`
		License  string   `json:"license"`
	}
	if err := json.Unmarshal(out, &byLicence); err != nil {
		return nil, err
	}
	var pkgs []npmPackage
	for _, list := range byLicence {
		for _, p := range list {
			if strings.HasPrefix(p.Name, "@mcp/") {
				continue
			}
			if len(p.Versions) != len(p.Paths) {
				return nil, fmt.Errorf("%s: %d versions, %d paths", p.Name, len(p.Versions), len(p.Paths))
			}
			for i, v := range p.Versions {
				pkgs = append(pkgs, npmPackage{Name: p.Name, Version: v, License: p.License, Dir: p.Paths[i]})
			}
		}
	}
	slices.SortFunc(pkgs, func(a, b npmPackage) int { return strings.Compare(a.Name+"@"+a.Version, b.Name+"@"+b.Version) })
	return pkgs, nil
}

func renderWeb(root string) ([]byte, error) {
	pkgs, err := webPackages(root)
	if err != nil {
		return nil, err
	}
	listed := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		listed = append(listed, p.Name+"@"+p.Version)
	}
	lock, err := os.ReadFile(filepath.Join(root, "pnpm-lock.yaml")) //nolint:gosec // G304: a fixed path in the repository
	if err != nil {
		return nil, err
	}
	inputs, err := webLockHash(lock, listed)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	seen := texts{}
	b.WriteString("npm packages bundled into the web UI (/app/web)\n")
	fmt.Fprintf(&b, gen, "web")
	b.WriteString("Inputs: pnpm-lock.yaml entries sha256:" + inputs + "\n")
	b.WriteString("\nThe production dependencies of apps/web (pnpm licenses list --prod --filter @mcp/web), each\n" +
		"with the licence its package.json declares and the licence files it ships. The bundle contains\n" +
		"what the UI uses of them. The Inputs line is a hash of the lockfile entries of apps/web,\n" +
		"packages/contracts and the packages listed; a test fails when the lockfile no longer gives it.\n\n")
	for _, p := range pkgs {
		files, err := licenceFiles(p.Dir)
		if err != nil {
			return nil, err
		}
		if err := section(&b, seen, p.Name+"@"+p.Version+" - "+p.License, p.Dir, files); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

func writeWeb(root string) error {
	data, err := renderWeb(root)
	if err != nil {
		return err
	}
	return writeFile(root, filepath.FromSlash(webNotices), data)
}

// webInputsRe finds the recorded lockfile hash in the web notices.
var webInputsRe = regexp.MustCompile(`(?m)^Inputs: pnpm-lock\.yaml entries sha256:([0-9a-f]{64})$`)

// webHeadingRe finds the listed packages (name@version) in the web notices.
var webHeadingRe = regexp.MustCompile(`(?m)^={80}\n(\S+@[^\s@]+) - `)

// webListed is the name@version of every package the web notices list.
func webListed(text string) []string {
	var out []string
	for _, m := range webHeadingRe.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// lockEntries splits a pnpm v9 lockfile into its top-level sections and their entries (keys at two
// spaces of indentation, unquoted), each entry with its key line and body as written.
func lockEntries(lock []byte) map[string]map[string]string {
	out := map[string]map[string]string{}
	section, key := "", ""
	var body strings.Builder
	flush := func() {
		if section != "" && key != "" {
			out[section][key] = body.String()
		}
		key = ""
		body.Reset()
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(lock), "\r\n", "\n"), "\n") {
		switch {
		case line != "" && line[0] != ' ':
			flush()
			section = strings.TrimSuffix(strings.Fields(line)[0], ":")
			if out[section] == nil {
				out[section] = map[string]string{}
			}
		case strings.HasPrefix(line, "  ") && len(line) > 2 && line[2] != ' ':
			flush()
			k := strings.TrimSpace(line)
			if i := strings.LastIndex(k, ":"); i > 0 {
				k = k[:i]
			}
			key = strings.Trim(k, `'"`)
			body.WriteString(line + "\n")
		case key != "":
			body.WriteString(line + "\n")
		}
	}
	flush()
	return out
}

// webLockHash is the SHA-256 of the lockfile entries the web notices depend on: the importers
// apps/web and packages/contracts, then for each listed package (sorted) its `packages` entry and
// every `snapshots` entry of that version (with any peer suffix). A listed package missing from
// the lockfile is an error.
func webLockHash(lock []byte, listed []string) (string, error) {
	e := lockEntries(lock)
	h := sha256.New()
	for _, imp := range []string{"apps/web", "packages/contracts"} {
		b, ok := e["importers"][imp]
		if !ok {
			return "", fmt.Errorf("pnpm-lock.yaml has no importer %s", imp)
		}
		h.Write([]byte(b))
	}
	pkgs := append([]string(nil), listed...)
	slices.Sort(pkgs)
	for _, p := range pkgs {
		b, ok := e["packages"][p]
		if !ok {
			return "", fmt.Errorf("pnpm-lock.yaml has no package %s", p)
		}
		h.Write([]byte(b))
		var snaps []string
		for k := range e["snapshots"] {
			if k == p || strings.HasPrefix(k, p+"(") {
				snaps = append(snaps, k)
			}
		}
		slices.Sort(snaps)
		for _, k := range snaps {
			h.Write([]byte(e["snapshots"][k]))
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
