package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
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
	var b bytes.Buffer
	seen := texts{}
	b.WriteString("npm packages bundled into the web UI (/app/web)\n")
	fmt.Fprintf(&b, gen, "web")
	b.WriteString("\nThe production dependencies of apps/web (pnpm licenses list --prod --filter @mcp/web), each\n" +
		"with the licence its package.json declares and the licence files it ships. The bundle contains\n" +
		"what the UI uses of them.\n\n")
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
