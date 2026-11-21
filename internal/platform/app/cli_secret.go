package app

import (
	"io"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
)

// Terminal is a person typing at stdin: a line is read without echo. The implementation (the one
// place that imports golang.org/x/term) is in cli_terminal.go; tests use a fake.
type Terminal interface {
	// ReadSecretLine reads one line, without its newline, and echoes nothing. End of input is an
	// empty line.
	ReadSecretLine() (string, error)
}

// secretPrompt is what readSecretInput of cli-input.ts writes before a typed secret.
const secretPrompt = "Secret: "

// readSecret is the server secret of create-server (readSecret in cli-commands.ts): from the
// variable named by --secret-env when given (unset or empty is a bad invocation), else, when the
// mode holds a secret, from stdin: typed without echo on a terminal, otherwise the whole stream
// with ONE trailing newline (`\n` or `\r\n`) removed. Nothing means no secret (nil). A mode that
// holds none never touches stdin. The secret is never part of an error.
func readSecret(cio CLIIO, secretEnv *string, auth catalog.AuthMode) (*string, error) {
	if secretEnv != nil {
		value := cio.Env[*secretEnv]
		if value == "" {
			return nil, UsageError(*secretEnv + " is unset or empty")
		}
		return &value, nil
	}
	if !auth.HoldsSecret() {
		return nil, nil
	}
	typed, err := readSecretInput(cio)
	if err != nil {
		return nil, err
	}
	typed = stripNewline(typed)
	if typed == "" {
		return nil, nil
	}
	return &typed, nil
}

// readSecretInput is readSecretInput of cli-input.ts: on a terminal one line typed without echo
// (the prompt and the newline that the hidden Enter did not show go to stderr), otherwise the
// whole stream.
func readSecretInput(cio CLIIO) (string, error) {
	if cio.Terminal != nil {
		_, _ = io.WriteString(cio.Stderr, secretPrompt)
		line, err := cio.Terminal.ReadSecretLine()
		_, _ = io.WriteString(cio.Stderr, "\n")
		if err != nil {
			return "", CommandError("cannot read the secret from the terminal") // never the cause: it may quote input
		}
		return line, nil
	}
	raw, err := io.ReadAll(cio.Stdin)
	if err != nil {
		return "", CommandError("cannot read the secret from stdin")
	}
	return string(raw), nil
}

// stripNewline removes one trailing newline (what `echo` or a heredoc adds), nothing else.
func stripNewline(s string) string {
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		return strings.TrimSuffix(t, "\r")
	}
	return s
}
