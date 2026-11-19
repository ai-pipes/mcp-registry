//go:build compat

package harness

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func sessionObjects(t *testing.T) string {
	t.Helper()
	c, err := dockerErr("ps", "-aq", "--filter", "label=compat.session="+session)
	if err != nil {
		t.Fatal(err)
	}
	n, err := dockerErr("network", "ls", "-q", "--filter", "label=compat.session="+session)
	if err != nil {
		t.Fatal(err)
	}
	return c + "|" + n
}

func TestFailedBootCleansUpAndExplains(t *testing.T) {
	const badKey = "bad-key-that-must-not-leak"
	before := sessionObjects(t)
	start := time.Now()
	s, err := boot(Options{Env: map[string]string{"SECRETS_KEY": badKey}}, false)
	if err == nil {
		s.teardown()
		t.Fatal("boot with an invalid SECRETS_KEY succeeded")
	}
	if time.Since(start) > 60*time.Second {
		t.Fatalf("the boot failure was not detected fast: %s", time.Since(start))
	}
	if !strings.Contains(err.Error(), "the app container is not running") || !strings.Contains(err.Error(), "SECRETS_KEY: must be base64") {
		t.Fatalf("error lacks the reason and the app log: %v", err)
	}
	if strings.Contains(err.Error(), badKey) {
		t.Fatalf("error quotes the key: %v", err)
	}
	if after := sessionObjects(t); after != before {
		t.Fatalf("leftovers after a failed boot: before %q after %q", before, after)
	}
}

func TestBootWithAMissingImageCleansUp(t *testing.T) {
	t.Setenv("COMPAT_IMAGE", "mcp-registry:does-not-exist")
	before := sessionObjects(t)
	if _, err := boot(Options{}, false); err == nil {
		t.Fatal("boot with a missing image succeeded")
	}
	if after := sessionObjects(t); after != before {
		t.Fatalf("leftovers: before %q after %q", before, after)
	}
}

// TestDoExpectingEarlyRefusalReturnsTheAnswerWhateverHappenedToTheWrite: a raw server reads the
// headers, answers 413 and closes at once without reading the 8 MiB body; the client's write
// breaks, the answer still comes back.
func TestDoExpectingEarlyRefusalReturnsTheAnswerWhateverHappenedToTheWrite(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	cookies := make(chan string, 16)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				n, _ := conn.Read(buf)
				for _, line := range strings.Split(string(buf[:n]), "\r\n") {
					if v, ok := strings.CutPrefix(line, "Cookie: "); ok {
						cookies <- v
					}
				}
				_, _ = conn.Write([]byte("HTTP/1.1 413 Payload Too Large\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"))
			}()
		}
	}()
	jar, _ := cookiejar.New(nil)
	base := "http://" + l.Addr().String()
	u, _ := url.Parse(base)
	jar.SetCookies(u, []*http.Cookie{{Name: "mcp_session", Value: "abc"}}) //nolint:gosec // G124: a cookie of a plain-http test listener, Secure would keep the jar from sending it
	c := &Client{base: base, http: newHTTPClient(jar)}
	for range 10 {
		r := c.DoExpectingEarlyRefusal(t, "POST", "/x", make([]byte, 8<<20), "Content-Type: application/json")
		if r.Status != 413 || string(r.Body) != "{}" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("status %d body %q header %v", r.Status, r.Body, r.Header)
		}
		// The cookie of the client's jar went out with the raw request, as http.Client.Do would send it.
		if got := <-cookies; got != "mcp_session=abc" || r.Sent.Get("Cookie") != "mcp_session=abc" {
			t.Fatalf("the raw listener saw the cookie %q, Sent says %q", got, r.Sent.Get("Cookie"))
		}
	}
	// A client without a jar sends no cookie.
	anon := &Client{base: base, http: newHTTPClient(nil)}
	if r := anon.DoExpectingEarlyRefusal(t, "POST", "/x", make([]byte, 1<<20)); r.Sent.Get("Cookie") != "" {
		t.Fatalf("a client without a jar sent %q", r.Sent.Get("Cookie"))
	}
}

func TestPortTakenRecognisesDockersMessages(t *testing.T) {
	for _, msg := range []string{
		"docker run: exit status 125: docker: Error response from daemon: driver failed programming external connectivity: Bind for 127.0.0.1:5000 failed: port is already allocated",
		"listen tcp4 127.0.0.1:5000: bind: address already in use",
	} {
		if !portTaken(errors.New(msg)) {
			t.Errorf("not recognised: %s", msg)
		}
	}
	if portTaken(errors.New("docker run: exit status 125: no such image: nope")) {
		t.Error("an unrelated docker failure is taken for a busy port")
	}
}

func TestOneOffCLIContainerNeverPutsAValueOnTheCommandLine(t *testing.T) {
	s := &Stack{
		network: "compat-x", image: "img", cli: "node dist/cli.js", id: "x",
		env: map[string]string{"SECRETS_KEY": "stack-key-value", "DATABASE_URL": "postgres://u:" + "stack-password" + "@db/x", "LOG_LEVEL": "debug"},
	}
	create, extraEnv := s.oneOffCreateArgs(map[string]string{"SECRETS_KEY": "other-key-value", "EXTRA_SECRET": "extra-value", "LOG_LEVEL": ""}, true, []string{"create-server", "--secret-env", "EXTRA_SECRET"})
	line := strings.Join(create, " ")
	for _, value := range []string{"stack-key-value", "other-key-value", "stack-password", "extra-value"} {
		if strings.Contains(line, value) {
			t.Fatalf("a value is on the command line: %q", line)
		}
	}
	for _, want := range []string{"-e DATABASE_URL", "-e EXTRA_SECRET", "-e SECRETS_KEY", "-i", "img node dist/cli.js create-server --secret-env EXTRA_SECRET"} {
		if !strings.Contains(line, want) {
			t.Errorf("the arguments lack %q: %q", want, line)
		}
	}
	if strings.Contains(line, "LOG_LEVEL") {
		t.Errorf("a variable removed with an empty value is still passed: %q", line)
	}
	got := strings.Join(extraEnv, "|")
	for _, want := range []string{"SECRETS_KEY=other-key-value", "EXTRA_SECRET=extra-value", "DATABASE_URL=postgres://u:stack-password@db/x"} {
		if !strings.Contains(got, want) {
			t.Errorf("the docker environment lacks %q: %q", want, got)
		}
	}
	if strings.Contains(got, "stack-key-value") || strings.Contains(got, "LOG_LEVEL") {
		t.Errorf("an overridden or removed value is in the docker environment: %q", got)
	}
}

func TestDefaultImageAndCLIFollowTheEnvironment(t *testing.T) {
	t.Setenv("COMPAT_IMAGE", "")
	t.Setenv("COMPAT_CLI", "")
	if DefaultImage() != defaultImage || DefaultCLI() != defaultCLI {
		t.Fatalf("defaults: %q %q", DefaultImage(), DefaultCLI())
	}
	t.Setenv("COMPAT_IMAGE", "img:1")
	t.Setenv("COMPAT_CLI", "mcp-registry")
	if DefaultImage() != "img:1" || DefaultCLI() != "mcp-registry" {
		t.Fatalf("from the environment: %q %q", DefaultImage(), DefaultCLI())
	}
}

func TestCLIResultStringMasksTokensAndLinkCodes(t *testing.T) {
	r := CLIResult{Code: 1, Stdout: "sk_abcDEF123-_x\nhttp://h/login#link=ll_QWERTY-_09", Stderr: "plain"}
	got := r.String()
	for _, leaked := range []string{"abcDEF123", "QWERTY"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("a credential is shown: %s", got)
		}
	}
	if !strings.Contains(got, "sk_<masked>") || !strings.Contains(got, "ll_<masked>") || !strings.Contains(got, "plain") {
		t.Fatalf("masked result: %s", got)
	}
}

func TestPublicHostAndExtraHostsReachTheContainerArguments(t *testing.T) {
	s := &Stack{
		network: "compat-x", image: "img", cli: "node dist/cli.js", id: "x", port: 4321, app: "compat-x-app",
		host: "gateway.compat.test", hosts: []string{"idp.compat.test:host-gateway"},
		env: map[string]string{"LOG_LEVEL": "debug"},
	}
	create, _ := s.oneOffCreateArgs(nil, false, []string{"sync", "x"})
	if line := strings.Join(create, " "); !strings.Contains(line, "--add-host idp.compat.test:host-gateway") || !strings.Contains(line, "--add-host host.docker.internal:host-gateway") {
		t.Fatalf("the one-off container lacks the hosts: %q", line)
	}
	if got := s.LoopbackURL(); got != "http://127.0.0.1:4321" {
		t.Fatalf("loopback URL %q", got)
	}
}

func TestLoopbackHostResolution(t *testing.T) {
	for host, want := range map[string]bool{
		"host.docker.internal": true, "gateway.compat.test": true, "idp.compat.test": true,
		"localhost": false, "example.com": false, "compat.test.evil.example": false, "notcompat.test": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestPublicHostBootsAStackThatAnswersOnItsPublicName(t *testing.T) {
	s, err := boot(Options{PublicHost: "gateway.compat.test", ExtraHosts: []string{"idp.compat.test:host-gateway"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.teardown)
	if !strings.HasPrefix(s.BaseURL, "http://gateway.compat.test:") || s.env["PUBLIC_URL"] != s.BaseURL {
		t.Fatalf("BaseURL %q, PUBLIC_URL %q", s.BaseURL, s.env["PUBLIC_URL"])
	}
	// The test process resolves *.compat.test itself, so a client of the stack works.
	c := &Client{base: s.BaseURL, http: newHTTPClient(nil)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz through the public name: %d", resp.StatusCode)
	}
	out, err := dockerErr("exec", s.app, "sh", "-c", "getent hosts idp.compat.test")
	if err != nil || !strings.Contains(out, "idp.compat.test") {
		t.Fatalf("the container does not resolve the extra host: %q %v", out, err)
	}
	// A failed CLI of a stopped app is an error, not a test failure.
	_, _ = dockerErr("rm", "-f", s.app)
	if _, err := s.TryCLI("", "sync", "x"); err == nil {
		t.Fatal("TryCLI on a stopped app returned no error")
	}
}
