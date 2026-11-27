// Command covergate judges a `go test -coverprofile` file: every domain and app package of every
// module must cover at least archtest.CoverageThreshold percent of its statements. It is run by
// `make go-cover`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ai-pipes/mcp-registry/internal/archtest"
)

func main() {
	profile := flag.String("profile", "", "coverage profile written by go test -coverprofile")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if *profile == "" {
		fmt.Fprintln(os.Stderr, "covergate: -profile is required")
		os.Exit(2)
	}
	expected, err := archtest.ExpectedPackages(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "covergate:", err)
		os.Exit(1)
	}
	f, err := os.Open(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "covergate:", err)
		os.Exit(1)
	}
	cov, err := archtest.ParseCoverProfile(f)
	_ = f.Close() // read-only
	if err != nil {
		fmt.Fprintln(os.Stderr, "covergate:", err)
		os.Exit(1)
	}
	report, failures := archtest.CheckCoverage(cov, expected, archtest.CoverageThreshold)
	for _, l := range report {
		fmt.Println(l)
	}
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "covergate: %d package(s) under the %.0f%% gate:\n", len(failures), archtest.CoverageThreshold)
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "  "+f)
		}
		os.Exit(1)
	}
}
