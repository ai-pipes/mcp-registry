//go:build compat

package harness

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
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

// TestDoRepeatsALargeUploadThatLostTheRaceToAReset: the first connection is reset by the server
// after the headers (as a server does when it refuses a large body without reading it), the second
// one is answered 413. A large upload is repeated and the 413 is seen; a small request is not
// repeated, its reset is an error.
func TestDoRepeatsALargeUploadThatLostTheRaceToAReset(t *testing.T) {
	serve := func(t *testing.T, resets int) (*Client, *int32) {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		var conns int32
		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				n := int(atomic.AddInt32(&conns, 1))
				go func() {
					defer func() { _ = conn.Close() }()
					buf := make([]byte, 4096)
					first, _ := conn.Read(buf)
					if n <= resets {
						if tcp, ok := conn.(*net.TCPConn); ok {
							_ = tcp.SetLinger(0) // close with a reset
						}
						return
					}
					// Read the whole upload first: closing with unread data would reset this one too.
					for got := first; got < largeBody+1; {
						k, err := conn.Read(make([]byte, 64<<10))
						if err != nil {
							break
						}
						got += k
					}
					_, _ = conn.Write([]byte("HTTP/1.1 413 Payload Too Large\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
				}()
			}
		}()
		return &Client{base: "http://" + l.Addr().String(), http: newHTTPClient(nil)}, &conns
	}
	t.Run("a large body is repeated", func(t *testing.T) {
		c, conns := serve(t, 1)
		r := c.Do(t, "POST", "/x", make([]byte, largeBody+1))
		if r.Status != 413 || atomic.LoadInt32(conns) != 2 {
			t.Fatalf("status %d after %d connections, want 413 after 2", r.Status, atomic.LoadInt32(conns))
		}
	})
	t.Run("a small body is not", func(t *testing.T) {
		c, conns := serve(t, 1)
		req, _ := http.NewRequestWithContext(context.Background(), "POST", c.base+"/x", strings.NewReader("small"))
		if resp, err := c.send(req); err == nil {
			_ = resp.Body.Close()
			t.Fatalf("a small request that was reset succeeded with %d", resp.StatusCode)
		}
		if atomic.LoadInt32(conns) != 1 {
			t.Fatalf("%d connections, want 1", atomic.LoadInt32(conns))
		}
	})
}
