//go:build compat

// Package fakes holds the external systems the application under test talks to: an upstream MCP
// server, an OpenID provider and an OAuth provider. They run inside the test process, listen on
// all interfaces and are reached by the application container as host.docker.internal.
package fakes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

// containerHost is how the application container reaches the test process.
const containerHost = "host.docker.internal"

// identityPath is reserved for the self-check of serve; no fake uses it.
const identityPath = "/__compat_identity"

// listenersFor binds what a fake listens on, by operating system.
//
// Linux: one wildcard listener 0.0.0.0:<free port>. The kernel refuses the wildcard bind when
// another process holds 127.0.0.1:P (and refuses 0.0.0.0:P followed by 127.0.0.1:P in one
// process), so a pair can never be made there and is not needed.
//
// Elsewhere (macOS, BSDs): Go sets SO_REUSEADDR, and binding 0.0.0.0:P then succeeds although
// another process (a host daemon, another stack's published port) holds 127.0.0.1:P; connections
// to 127.0.0.1:P reach THAT process, and the test client dials 127.0.0.1 for host.docker.internal.
// So the loopback address is bound too (listenPair): EADDRINUSE there means someone else holds it.
func listenersFor(goos string) ([]net.Listener, error) {
	if goos == "linux" {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "0.0.0.0:0")
		if err != nil {
			return nil, err
		}
		return []net.Listener{l}, nil
	}
	all, loopback, err := listenPair("0")
	if err != nil {
		return nil, err
	}
	return []net.Listener{all, loopback}, nil
}

// listenPair binds 0.0.0.0:port and then 127.0.0.1:port (port "0" picks a free one for the first).
// On macOS the second bind fails with EADDRINUSE when another process holds the loopback port.
func listenPair(port string) (all, loopback net.Listener, err error) {
	lc := &net.ListenConfig{}
	all, err = lc.Listen(context.Background(), "tcp", "0.0.0.0:"+port)
	if err != nil {
		return nil, nil, err
	}
	tcp, ok := all.Addr().(*net.TCPAddr)
	if !ok {
		_ = all.Close()
		return nil, nil, fmt.Errorf("unexpected address %v", all.Addr())
	}
	loopback, err = lc.Listen(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", tcp.Port))
	if err != nil {
		_ = all.Close()
		return nil, nil, err
	}
	return all, loopback, nil
}

// selfCheck dials 127.0.0.1:port and asks for the identity the fake serves on identityPath: the
// connection must have been accepted by the fake's own listener, on every operating system.
func selfCheck(port int, token string) error {
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, identityPath))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return err
	}
	if string(body) != token {
		return fmt.Errorf("127.0.0.1:%d answered %q, not this fake: another process holds the port", port, body)
	}
	return nil
}

// maxBindAttempts bounds the search for a port that really is the fake's own.
const maxBindAttempts = 20

// serve listens (see listenersFor) on a free port and serves h until the test ends. Before it
// returns, a request to 127.0.0.1:<port> must reach this fake (selfCheck); otherwise everything is
// closed and another port is tried, at most maxBindAttempts times. In-flight requests (a
// deliberately slow or hanging answer) are cancelled by the cleanup. It returns the port.
func serve(t *testing.T, h http.Handler) int {
	t.Helper()
	port, err := serveE(t.Cleanup, h)
	if err != nil {
		t.Fatalf("fake listen: %v", err)
	}
	return port
}

// serveE is serve for callers that must not fail a test from their own goroutine: the error is
// returned, and cleanup (t.Cleanup, which is safe to call from any goroutine) registers the stop.
func serveE(cleanup func(func()), h http.Handler) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= maxBindAttempts; attempt++ {
		port, stop, err := tryServe(h)
		if err == nil {
			cleanup(stop)
			return port, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("no usable port in %d attempts, last error: %w", maxBindAttempts, lastErr)
}

// tryServe makes one attempt: listeners, server, self-check. stop closes everything.
func tryServe(h http.Handler) (port int, stop func(), err error) {
	ls, err := listenersFor(runtime.GOOS)
	if err != nil {
		return 0, nil, err
	}
	tcp, ok := ls[0].Addr().(*net.TCPAddr)
	if !ok {
		for _, l := range ls {
			_ = l.Close()
		}
		return 0, nil, fmt.Errorf("unexpected address %v", ls[0].Addr())
	}
	token := randomToken("fake-")
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == identityPath {
				_, _ = w.Write([]byte(token))
				return
			}
			h.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = srv.Serve(l) // ErrServerClosed at the end; other errors surface in the self-check
		}()
	}
	stop = func() {
		cancel()
		_ = srv.Close()
		wg.Wait()
	}
	if err := selfCheck(tcp.Port, token); err != nil {
		stop()
		return 0, nil, err
	}
	return tcp.Port, stop, nil
}

func baseURL(port int) string { return advertisedURL(containerHost, port) }

// advertisedURL is the URL a fake puts in its own discovery data and in the URLs it hands out.
func advertisedURL(host string, port int) string { return fmt.Sprintf("http://%s:%d", host, port) }

// randomToken returns prefix and 16 random bytes in hex.
func randomToken(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system random source failing is not recoverable
	}
	return prefix + hex.EncodeToString(b)
}

// readBody reads a small form body; a failed read yields what was read.
func readBody(r *http.Request) string {
	b, _ := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	return string(b)
}
