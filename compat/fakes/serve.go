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
	"testing"
	"time"
)

// containerHost is how the application container reaches the test process.
const containerHost = "host.docker.internal"

// serve listens on 0.0.0.0:<random port> and serves h until the test ends. In-flight requests
// (a deliberately slow or hanging answer) are cancelled by the cleanup. It returns the port.
func serve(t *testing.T, h http.Handler) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("fake listen: %v", err)
	}
	tcp, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		_ = l.Close()
		t.Fatalf("fake listen: unexpected address %v", l.Addr())
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fake serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		<-done
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
