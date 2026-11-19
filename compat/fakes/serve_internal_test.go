//go:build compat

package fakes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func holdLoopback(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, l.Addr().(*net.TCPAddr).Port
}

// TestWildcardBindWithLoopbackHeldByAnother pins what each OS does when another listener already
// holds 127.0.0.1:P, which decides how serve has to protect a fake.
//   - linux: the kernel refuses the wildcard bind itself (EADDRINUSE), so a fake cannot end up there;
//   - elsewhere (macOS, BSDs): the wildcard bind succeeds (SO_REUSEADDR) and would steal the
//     traffic, and only the loopback bind of listenPair refuses.
func TestWildcardBindWithLoopbackHeldByAnother(t *testing.T) {
	_, port := holdLoopback(t)
	if runtime.GOOS == "linux" {
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			_ = l.Close()
			t.Fatalf("linux bound 0.0.0.0:%d although 127.0.0.1:%d is held", port, port)
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("error %v, want EADDRINUSE", err)
		}
		return
	}
	all, loopback, err := listenPair(fmt.Sprint(port))
	if err == nil {
		_ = all.Close()
		_ = loopback.Close()
		t.Fatalf("the pair was bound on port %d although 127.0.0.1:%d is held", port, port)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("error %v, want EADDRINUSE", err)
	}
}

// TestListenersForGivesWhatTheOSNeeds: one wildcard listener on linux, the wildcard and loopback
// pair elsewhere.
func TestListenersForGivesWhatTheOSNeeds(t *testing.T) {
	want := 2
	if runtime.GOOS == "linux" {
		want = 1
	}
	ls, err := listenersFor(runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		defer func() { _ = l.Close() }()
	}
	if len(ls) != want {
		t.Fatalf("%s: %d listeners, want %d", runtime.GOOS, len(ls), want)
	}
}

// TestServeAnswersOnLoopbackAndPassesItsSelfCheck holds many loopback ports, starts fakes and
// checks, on every OS, that each answers on 127.0.0.1 with its own identity and, where the OS
// would let the wildcard bind steal a held port (not linux), none of them lands on a held port.
func TestServeAnswersOnLoopbackAndPassesItsSelfCheck(t *testing.T) {
	held := map[int]bool{}
	for range 50 {
		_, port := holdLoopback(t)
		held[port] = true
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("fake")) })
	for range 20 {
		port := serve(t, h)
		if held[port] {
			t.Fatalf("the fake was served on the held port %d", port)
		}
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "fake" {
			t.Fatalf("127.0.0.1:%d answered %q, not the fake", port, body)
		}
	}
}

// TestSelfCheckDetectsAnotherProcess: something else answers on the port, so the check fails.
func TestSelfCheckDetectsAnotherProcess(t *testing.T) {
	l, port := holdLoopback(t)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("someone else")) })}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	if err := selfCheck(port, "fake-token"); err == nil {
		t.Fatal("the self-check accepted an answer that is not the fake's identity")
	}
	if err := selfCheck(1, "x"); err == nil { // nothing listens
		t.Fatal("the self-check passed with nothing listening")
	}
}
