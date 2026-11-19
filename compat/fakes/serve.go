//go:build compat

// Package fakes holds the external systems the application under test talks to: an upstream MCP
// server, an OpenID provider and an OAuth provider. They run inside the test process, listen on
// all interfaces and are reached by the application container as host.docker.internal.
package fakes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"time"
)

// containerHost is how the application container reaches the test process.
const containerHost = "host.docker.internal"

// listenPair binds 0.0.0.0:port and then 127.0.0.1:port (port "0" picks a free one for the first).
// Go sets SO_REUSEADDR, and on macOS binding 0.0.0.0:P succeeds although another process (a host
// daemon, another stack's published port) already holds 127.0.0.1:P; connections to
// 127.0.0.1:P then reach THAT process, and the test client dials 127.0.0.1 for
// host.docker.internal. Binding the loopback address too makes the second bind fail with
// EADDRINUSE in that case, so the fake never ends up on a port that is not really its own.
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

// maxBindAttempts bounds the search for a port that is free on both addresses.
const maxBindAttempts = 20

// serve listens on 0.0.0.0:<random port> and on 127.0.0.1 with the same port, and serves h on
// both until the test ends. A port taken on the loopback address by another process is skipped
// (EADDRINUSE, at most maxBindAttempts times). In-flight requests (a deliberately slow or hanging
// answer) are cancelled by the cleanup. It returns the port.
func serve(t *testing.T, h http.Handler) int {
	t.Helper()
	var all, loopback net.Listener
	var err error
	for attempt := 1; ; attempt++ {
		all, loopback, err = listenPair("0")
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EADDRINUSE) || attempt == maxBindAttempts {
			t.Fatalf("fake listen: %v (attempt %d of %d)", err, attempt, maxBindAttempts)
		}
	}
	tcp, ok := all.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("fake listen: unexpected address %v", all.Addr())
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	var wg sync.WaitGroup
	for _, l := range []net.Listener{all, loopback} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("fake serve: %v", err)
			}
		}()
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		wg.Wait()
	})
	return tcp.Port
}

func baseURL(port int) string { return fmt.Sprintf("http://%s:%d", containerHost, port) }

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
