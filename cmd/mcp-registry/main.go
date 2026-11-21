// Command mcp-registry is the Go implementation of the MCP registry gateway.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: mcp-registry --version")
		return 2
	}
	if args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
