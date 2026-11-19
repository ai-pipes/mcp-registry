//go:build compat

package fakes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
)

// TestListenPairRefusesAPortThatLoopbackAlreadyHolds: another process holds 127.0.0.1:P. Binding
// 0.0.0.0:P alone may succeed (macOS with SO_REUSEADDR) and would steal the fake's traffic into
// that process; the pair must fail with EADDRINUSE instead.
func TestListenPairRefusesAPortThatLoopbackAlreadyHolds(t *testing.T) {
	held, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port

	all, loopback, err := listenPair(fmt.Sprint(port))
	if err == nil {
		_ = all.Close()
		_ = loopback.Close()
		t.Fatalf("the pair was bound on port %d although 127.0.0.1:%d is held", port, port)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("error %v, want EADDRINUSE", err)
	}
	// Control: a free port is bound on both addresses.
	all, loopback, err = listenPair("0")
	if err != nil {
		t.Fatal(err)
	}
	_ = all.Close()
	_ = loopback.Close()
}

// TestServeAnswersOnLoopbackAndNeverOnAHeldPort: many fakes are started while 127.0.0.1 ports are
// held by someone else; none of them lands on a held port and each answers on 127.0.0.1.
func TestServeAnswersOnLoopbackAndNeverOnAHeldPort(t *testing.T) {
	held := map[int]net.Listener{}
	for range 50 {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		held[l.Addr().(*net.TCPAddr).Port] = l
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("fake")) })
	for range 20 {
		port := serve(t, h)
		if _, taken := held[port]; taken {
			t.Fatalf("the fake was served on the held port %d", port)
		}
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8)
		n, _ := resp.Body.Read(buf)
		_ = resp.Body.Close()
		if string(buf[:n]) != "fake" {
			t.Fatalf("127.0.0.1:%d answered %q, not the fake", port, buf[:n])
		}
	}
}
