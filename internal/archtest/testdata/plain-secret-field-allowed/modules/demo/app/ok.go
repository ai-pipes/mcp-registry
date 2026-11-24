package app

import "github.com/ai-pipes/mcp-registry/internal/kernel"

// Command holds its secrets as kernel types: nothing to report.
type Command struct {
	AccessToken kernel.Secret
	Sealed      kernel.SecretBytes
}

// Problem has an error code, allowlisted by the test.
type Problem struct{ Code string }
