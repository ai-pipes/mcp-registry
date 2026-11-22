package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
)

// tripReader fails the test when it is read: a mode without a secret must not touch stdin.
type tripReader struct{ t *testing.T }

func (r tripReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("stdin was read")
	return 0, io.EOF
}

type fakeTerminal struct {
	line  string
	err   error
	read  int
	block bool // waits for the context, as a terminal read with nothing typed does
}

func (f *fakeTerminal) ReadSecretLine(ctx context.Context) (string, error) {
	f.read++
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.line, f.err
}

func cioOf(stdin io.Reader, env map[string]string) (CLIIO, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return CLIIO{Stdin: stdin, Stdout: &out, Stderr: &errOut, Env: env}, &out, &errOut
}

var (
	sharedSecretMode = catalog.AuthMode{Kind: catalog.AuthSharedSecret}
	noneMode         = catalog.AuthMode{Kind: catalog.AuthNone}
)

func strp(s string) *string { return &s }

func TestReadSecretFromAPipe(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     *string
	}{
		{"one trailing newline is dropped", "s3cret\n", strp("s3cret")},
		{"CRLF is dropped as one", "s3cret\r\n", strp("s3cret")},
		{"only one newline is dropped", "s3cret\n\n", strp("s3cret\n")},
		{"no newline", "s3cret", strp("s3cret")},
		{"inner and leading space is kept", "  a b \n", strp("  a b ")},
		{"empty is no secret", "", nil},
		{"a lone newline is no secret", "\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cio, _, errOut := cioOf(strings.NewReader(tc.in), nil)
			got, err := readSecret(context.Background(), cio, nil, sharedSecretMode)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil && got != nil, tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if errOut.Len() != 0 {
				t.Errorf("a pipe prints no prompt: %q", errOut.String())
			}
		})
	}
}

func TestReadSecretSkipsStdinForAModeWithoutOne(t *testing.T) {
	cio, _, _ := cioOf(tripReader{t}, nil)
	if got, err := readSecret(context.Background(), cio, nil, noneMode); err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	// An OAuth server holds the client secret, a PAT server the person's own token.
	cio, _, _ = cioOf(strings.NewReader("c\n"), nil)
	if got, _ := readSecret(context.Background(), cio, nil, catalog.AuthMode{Kind: catalog.AuthOAuthUser}); got == nil || *got != "c" {
		t.Errorf("oauth_user holds a secret: %v", got)
	}
	cio, _, _ = cioOf(tripReader{t}, nil)
	if got, err := readSecret(context.Background(), cio, nil, catalog.AuthMode{Kind: catalog.AuthPATUser}); err != nil || got != nil {
		t.Errorf("pat_user holds none: %v %v", got, err)
	}
}

func TestReadSecretFromTheEnvironment(t *testing.T) {
	name := "K"
	cio, _, _ := cioOf(tripReader{t}, map[string]string{"K": "from-env\n"})
	got, err := readSecret(context.Background(), cio, &name, sharedSecretMode)
	if err != nil || got == nil || *got != "from-env\n" {
		t.Fatalf("the variable is taken as it is (no newline stripping): %v %v", got, err)
	}
	// The variable wins even for a mode that holds no secret? TypeScript reads it first: yes.
	cio, _, _ = cioOf(tripReader{t}, map[string]string{"K": "x"})
	if got, _ := readSecret(context.Background(), cio, &name, noneMode); got == nil || *got != "x" {
		t.Errorf("--secret-env is read before the mode is asked: %v", got)
	}
	for _, env := range []map[string]string{{}, {"K": ""}} {
		cio, _, _ := cioOf(tripReader{t}, env)
		_, err := readSecret(context.Background(), cio, &name, sharedSecretMode)
		var u *UsageErr
		if !errors.As(err, &u) || u.Msg != "K is unset or empty" {
			t.Errorf("env %v: %v", env, err)
		}
	}
}

func TestReadSecretOnATerminal(t *testing.T) {
	term := &fakeTerminal{line: "typed"}
	cio, _, errOut := cioOf(tripReader{t}, nil)
	cio.Terminal = term
	got, err := readSecret(context.Background(), cio, nil, sharedSecretMode)
	if err != nil || got == nil || *got != "typed" {
		t.Fatalf("%v %v", got, err)
	}
	if errOut.String() != "Secret: \n" {
		t.Errorf("the prompt and the newline that the hidden Enter did not echo go to stderr: %q", errOut.String())
	}
	term = &fakeTerminal{}
	cio.Terminal = term
	if got, _ := readSecret(context.Background(), cio, nil, sharedSecretMode); got != nil {
		t.Errorf("an empty line is no secret: %v", got)
	}
	// A mode without a secret never prompts.
	term = &fakeTerminal{line: "x"}
	cio, _, errOut = cioOf(tripReader{t}, nil)
	cio.Terminal = term
	if got, _ := readSecret(context.Background(), cio, nil, noneMode); got != nil || term.read != 0 || errOut.Len() != 0 {
		t.Errorf("none prompted: %v %d %q", got, term.read, errOut.String())
	}
}

func TestATerminalFailureIsNeverQuoted(t *testing.T) {
	cio, out, errOut := cioOf(tripReader{t}, nil)
	cio.Terminal = &fakeTerminal{line: "half-typed", err: errors.New("read /dev/tty: s3cret-leak")}
	got, err := readSecret(context.Background(), cio, nil, sharedSecretMode)
	if got != nil || err == nil {
		t.Fatalf("%v %v", got, err)
	}
	for _, text := range []string{err.Error(), out.String(), errOut.String()} {
		if strings.Contains(text, "s3cret-leak") || strings.Contains(text, "half-typed") {
			t.Errorf("the failure quotes what was typed or the cause: %q", text)
		}
	}
	var failed *CommandErr
	if !errors.As(err, &failed) || failed.Msg != "cannot read the secret from the terminal" {
		t.Errorf("%v", err)
	}
}

// An interrupt while the secret is read aborts the command (the reference carries on without a
// secret; this is the safer reading of Ctrl-C).
func TestAnInterruptAbortsTheSecretReadWithANewline(t *testing.T) {
	var failed *CommandErr
	for name, term := range map[string]*fakeTerminal{
		"Ctrl-C as a byte": {line: "half", err: errInterrupted},
	} {
		cio, _, errOut := cioOf(tripReader{t}, nil)
		cio.Terminal = term
		got, err := readSecret(context.Background(), cio, nil, sharedSecretMode)
		if got != nil || !errors.As(err, &failed) || failed.Msg != "interrupted" {
			t.Errorf("%s: %v %v", name, got, err)
		}
		if errOut.String() != "Secret: \n" {
			t.Errorf("%s: %q", name, errOut.String())
		}
	}
	// The context cancelled while the terminal waits (SIGINT, SIGTERM).
	ctx, cancel := context.WithCancel(context.Background())
	cio, _, errOut := cioOf(tripReader{t}, nil)
	cio.Terminal = &fakeTerminal{block: true}
	done := make(chan error, 1)
	go func() { _, err := readSecret(ctx, cio, nil, sharedSecretMode); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.As(err, &failed) || failed.Msg != "interrupted" {
			t.Errorf("%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read did not end")
	}
	if errOut.String() != "Secret: \n" {
		t.Errorf("%q", errOut.String())
	}
	// A line that arrives together with the cancellation is not used.
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	cio, _, _ = cioOf(tripReader{t}, nil)
	cio.Terminal = &fakeTerminal{line: "late"}
	if got, err := readSecret(ctx, cio, nil, sharedSecretMode); got != nil || err == nil {
		t.Errorf("a cancelled command used the secret: %v %v", got, err)
	}
}

func TestAPipeThatNeverClosesEndsOnCancellation(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cio, _, _ := cioOf(pr, nil)
	done := make(chan error, 1)
	go func() { _, err := readSecret(ctx, cio, nil, sharedSecretMode); done <- err }()
	cancel()
	select {
	case err := <-done:
		var failed *CommandErr
		if !errors.As(err, &failed) || failed.Msg != "interrupted" {
			t.Errorf("%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read did not end")
	}
}

func TestInvalidUTF8OnAPipeIsReplacedAsJavaScriptDoes(t *testing.T) {
	// One U+FFFD per maximal invalid subpart (the WHATWG decoder of TextDecoder and Node's
	// stream decoding), not one per run of bad bytes.
	for in, want := range map[string]string{
		"ok\n":               "ok",
		"a\xffb":             "a\ufffdb",
		"\xff\xfe":           "\ufffd\ufffd",
		"x\xe2\x82":          "x\ufffd", // a cut euro sign is one subpart
		"\xf0\x9f\x98":       "\ufffd",
		"\xc0\x80":           "\ufffd\ufffd",
		"\xed\xa0\x80":       "\ufffd\ufffd\ufffd", // a surrogate
		"\xe2\x82\xac\x80":   "\u20ac\ufffd",
		"\u00e9\U0001F600\n": "\u00e9\U0001F600",
	} {
		cio, _, _ := cioOf(strings.NewReader(in), nil)
		got, err := readSecret(context.Background(), cio, nil, sharedSecretMode)
		if err != nil || got == nil || *got != want {
			t.Errorf("%q: got %v, want %q", in, got, want)
		}
	}
}

// The steps before the person is looked up need no database: the wiring below has no identity
// deps, so a command that went on to look the person up would panic.
func serverCommand(t *testing.T, name string) CLICommand {
	t.Helper()
	for _, c := range (catalogWiring{}).commands() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no command %s", name)
	return CLICommand{}
}

func runCmd(t *testing.T, name string, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	cio, out, _ := cioOf(stdin, nil)
	err := serverCommand(t, name).Run(context.Background(), cio, args)
	return out.String(), err
}

func writeJSON(t *testing.T, name, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreateServerRefusesBeforeTheDatabaseIsTouched(t *testing.T) {
	var usage *UsageErr
	var failed *CommandErr
	_, err := runCmd(t, "create-server", tripReader{t}, "--as", "x")
	if !errors.As(err, &usage) || usage.Msg != "--json is required" {
		t.Errorf("no --json: %v", err)
	}
	_, err = runCmd(t, "create-server", tripReader{t}, "--json", "f.json")
	if !errors.As(err, &usage) || usage.Msg != "--as is required" {
		t.Errorf("no --as: %v", err)
	}
	_, err = runCmd(t, "create-server", tripReader{t}, "--json", "f", "--as", "x", "extra")
	if !errors.As(err, &usage) || usage.Msg != "expected 0 argument(s), got 1" {
		t.Errorf("positional: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	_, err = runCmd(t, "create-server", tripReader{t}, "--json", missing, "--as", "x")
	if !errors.As(err, &failed) || failed.Msg != "cannot read "+missing {
		t.Errorf("missing file: %v", err)
	}
	for _, text := range []string{"", "   ", "{", "nope"} {
		f := writeJSON(t, "bad.json", text)
		_, err = runCmd(t, "create-server", tripReader{t}, "--json", f, "--as", "x")
		if !errors.As(err, &failed) || failed.Msg != f+" is not valid JSON" {
			t.Errorf("%q: %v", text, err)
		}
	}
}

const goodServer = `{"slug":"fake","name":"Fake","description":"d","url":"http://127.0.0.1:1/mcp",
"auth":{"kind":"none"},"access":{"kind":"all"}}`

func TestCreateServerPrintsEveryIssueInSchemaOrder(t *testing.T) {
	var failed *CommandErr
	f := writeJSON(t, "bad.json", `{"slug":"x","name":"","url":"ftp://x","auth":{"kind":"none"}}`)
	_, err := runCmd(t, "create-server", tripReader{t}, "--json", f, "--as", "x")
	if !errors.As(err, &failed) {
		t.Fatalf("%v", err)
	}
	lines := strings.Split(strings.TrimPrefix(failed.Msg, f+" is not a valid server:\n"), "\n")
	var paths []string
	for _, l := range lines {
		path, _, ok := strings.Cut(strings.TrimPrefix(l, "  "), ": ")
		if !ok || !strings.HasPrefix(l, "  ") {
			t.Fatalf("line %q is not `  <path>: <message>`", l)
		}
		paths = append(paths, path)
	}
	if got := strings.Join(paths, ","); got != "name,description,url,access" {
		t.Errorf("issues %s (message %q)", got, failed.Msg)
	}
	if !strings.Contains(failed.Msg, "\n  url: must be an http(s) URL") {
		t.Errorf("the url issue has the reference's text: %q", failed.Msg)
	}
	// A root that is not an object names (root).
	f = writeJSON(t, "arr.json", `[]`)
	_, err = runCmd(t, "create-server", tripReader{t}, "--json", f, "--as", "x")
	if !errors.As(err, &failed) || !strings.HasPrefix(failed.Msg, f+" is not a valid server:\n  (root): ") {
		t.Errorf("%v", err)
	}
}

func TestSyncNeedsOneSlug(t *testing.T) {
	var usage *UsageErr
	_, err := runCmd(t, "sync", nil)
	if !errors.As(err, &usage) || usage.Msg != "expected 1 argument(s), got 0" {
		t.Errorf("%v", err)
	}
}

func TestTheServerCommandsAreRegisteredWithTheirGuards(t *testing.T) {
	guards := map[string]KeyGuard{}
	for _, c := range (catalogWiring{}).commands() {
		guards[c.Name] = c.Guard
		if c.Usage == "" || c.Run == nil {
			t.Errorf("%s: incomplete", c.Name)
		}
	}
	// create-server is the only command that seals a new secret: a key mismatch refuses it.
	if len(guards) != 2 || guards["create-server"] != GuardRefuse || guards["sync"] != GuardWarn {
		t.Errorf("%v", guards)
	}
	if _, left := pendingUsage["create-server"]; left {
		t.Error("create-server is implemented: its pending usage entry goes")
	}
	if _, left := pendingUsage["sync"]; left {
		t.Error("sync is implemented: its pending usage entry goes")
	}
}

func TestOnlyARealTerminalGetsTheNoEchoReader(t *testing.T) {
	if terminalOf(strings.NewReader("x")) != nil {
		t.Error("a reader is not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if terminalOf(r) != nil {
		t.Error("a pipe is not a terminal")
	}
}
