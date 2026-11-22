package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
)

// Terminal is a person typing at stdin: a line is read without echo. The implementation (the one
// place that imports golang.org/x/term) is in cli_terminal.go; tests use a fake.
type Terminal interface {
	// ReadSecretLine reads one line, without its newline, and echoes nothing. Ctrl-D on an empty
	// line is an empty line. An interrupt (Ctrl-C, or ctx cancelled: SIGINT and SIGTERM end ctx)
	// is an error, errInterrupted or ctx's, and the terminal is as it was before the call.
	ReadSecretLine(ctx context.Context) (string, error)
}

// errInterrupted is Ctrl-C typed at the secret prompt.
var errInterrupted = errors.New("interrupted")

// secretPrompt is what readSecretInput of cli-input.ts writes before a typed secret.
const secretPrompt = "Secret: "

// readSecret is the server secret of create-server (readSecret in cli-commands.ts): from the
// variable named by --secret-env when given (unset or empty is a bad invocation), else, when the
// mode holds a secret, from stdin: typed without echo on a terminal, otherwise the whole stream
// with ONE trailing newline (`\n` or `\r\n`) removed. Nothing means no secret (nil). A mode that
// holds none never touches stdin. The secret is never part of an error.
func readSecret(ctx context.Context, cio CLIIO, secretEnv *string, auth catalog.AuthMode) (*string, error) {
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
	typed, err := readSecretInput(ctx, cio)
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
// whole stream. An interrupt while reading, or a context that ends before the secret is in, aborts
// the command (the reference carries on without a secret after Ctrl-C; creating a server nobody
// meant to finish is the worse mistake).
func readSecretInput(ctx context.Context, cio CLIIO) (string, error) {
	if cio.Terminal != nil {
		_, _ = io.WriteString(cio.Stderr, secretPrompt)
		line, err := cio.Terminal.ReadSecretLine(ctx)
		_, _ = io.WriteString(cio.Stderr, "\n")
		switch {
		case errors.Is(err, errInterrupted), ctx.Err() != nil:
			return "", CommandError(msgInterrupted)
		case err != nil:
			return "", CommandError("cannot read the secret from the terminal") // never the cause: it may quote input
		}
		return line, nil
	}
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := io.ReadAll(cio.Stdin)
		done <- result{raw, err}
	}()
	select {
	case r := <-done:
		switch {
		case ctx.Err() != nil:
			return "", CommandError(msgInterrupted)
		case r.err != nil:
			return "", CommandError("cannot read the secret from stdin")
		}
		return decodeUTF8(r.raw), nil
	case <-ctx.Done():
		return "", CommandError(msgInterrupted) // the read is left behind; the process is about to end
	}
}

// msgInterrupted is the stderr text of a command ended by Ctrl-C or a signal at the secret prompt.
const msgInterrupted = "interrupted"

// decodeUTF8 is what the reference's stream decoding makes of bytes: invalid ones become U+FFFD,
// one for each maximal invalid subpart (the WHATWG rule: a cut multi-byte sequence is one
// replacement, every other bad byte its own). strings.ToValidUTF8 would merge a run into one.
func decodeUTF8(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	var b strings.Builder
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		if r != utf8.RuneError || size > 1 {
			b.WriteRune(r)
			raw = raw[size:]
			continue
		}
		b.WriteRune(utf8.RuneError)
		raw = raw[invalidPrefix(raw):]
	}
	return b.String()
}

// invalidPrefix is the length of the maximal subpart of a valid sequence at the start of raw (at
// least 1): the lead byte and the continuation bytes that fit its allowed ranges.
func invalidPrefix(raw []byte) int {
	lo, hi := byte(0x80), byte(0xBF)
	var need int
	switch b := raw[0]; {
	case b >= 0xC2 && b <= 0xDF:
		need = 1
	case b == 0xE0:
		need, lo = 2, 0xA0
	case b == 0xED:
		need, hi = 2, 0x9F
	case b >= 0xE1 && b <= 0xEF:
		need = 2
	case b == 0xF0:
		need, lo = 3, 0x90
	case b == 0xF4:
		need, hi = 3, 0x8F
	case b >= 0xF1 && b <= 0xF3:
		need = 3
	default:
		return 1
	}
	n := 1
	for ; n <= need && n < len(raw); n++ {
		if raw[n] < lo || raw[n] > hi {
			break
		}
		lo, hi = 0x80, 0xBF // only the first continuation byte has a special range
	}
	return n
}

// stripNewline removes one trailing newline (what `echo` or a heredoc adds), nothing else.
func stripNewline(s string) string {
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		return strings.TrimSuffix(t, "\r")
	}
	return s
}
