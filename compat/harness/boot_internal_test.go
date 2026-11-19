//go:build compat

package harness

import (
	"context"
	"errors"
	"net"
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
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = conn.Read(make([]byte, 4096))
				_, _ = conn.Write([]byte("HTTP/1.1 413 Payload Too Large\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"))
			}()
		}
	}()
	c := &Client{base: "http://" + l.Addr().String(), http: newHTTPClient(nil)}
	for range 10 {
		r := c.DoExpectingEarlyRefusal(t, "POST", "/x", make([]byte, 8<<20), "Content-Type: application/json")
		if r.Status != 413 || string(r.Body) != "{}" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("status %d body %q header %v", r.Status, r.Body, r.Header)
		}
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
