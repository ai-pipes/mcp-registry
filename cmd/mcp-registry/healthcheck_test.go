package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// probeServer answers /healthz with status and body, and counts the requests it got.
func probeServer(t *testing.T, status int, body string) (port string, hits *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if r.URL.Path != "/healthz" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return portOf(t, srv.Listener.Addr()), &n
}

func portOf(t *testing.T, a net.Addr) string {
	t.Helper()
	_, p, err := net.SplitHostPort(a.String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// healthcheckRun runs the command through run, as the image's HEALTHCHECK does.
func healthcheckRun(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), append([]string{"healthcheck"}, args...), env, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// oneLine: a failure is one short line on stderr.
func oneLine(t *testing.T, stderr string) {
	t.Helper()
	if !strings.HasPrefix(stderr, "healthcheck: ") || strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") || len(stderr) > 120 {
		t.Errorf("stderr is not one short line: %q", stderr)
	}
}

func TestHealthcheckHealthyIsSilentExitZero(t *testing.T) {
	port, hits := probeServer(t, http.StatusOK, "ok")
	code, out, errOut := healthcheckRun(t, map[string]string{"PORT": port})
	if code != 0 || out != "" || errOut != "" || *hits != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q hits=%d", code, out, errOut, *hits)
	}
}

// No configuration beyond PORT, or a broken one: the probe still runs (the configuration is the
// server's to refuse, and a server that refused it does not answer).
func TestHealthcheckNeedsNoConfiguration(t *testing.T) {
	port, _ := probeServer(t, http.StatusOK, "ok")
	broken := map[string]string{"PORT": port, "SECRETS_KEY": "not a key", "PUBLIC_URL": "x", "LOG_LEVEL": "loud", "OIDC_ISSUER": "half"}
	if code, out, errOut := healthcheckRun(t, broken); code != 0 || out != "" || errOut != "" {
		t.Fatalf("broken configuration: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// Nothing at all: PORT defaults to 3000. Whatever listens there on this machine, the command
	// probes it and never reports the configuration.
	code, out, errOut := healthcheckRun(t, nil)
	if out != "" || strings.Contains(errOut, "configuration") {
		t.Fatalf("empty environment: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if code != 0 {
		oneLine(t, errOut)
		if !strings.Contains(errOut, "127.0.0.1:3000") && !strings.Contains(errOut, "answered") {
			t.Errorf("empty environment did not probe port 3000: %q", errOut)
		}
	}
}

func TestHealthcheckNotAnsweringIsExitOne(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, l.Addr())
	_ = l.Close() // nothing listens there now
	code, out, errOut := healthcheckRun(t, map[string]string{"PORT": port})
	if code != 1 || out != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	oneLine(t, errOut)
	if errOut != "healthcheck: no answer from http://127.0.0.1:"+port+"/healthz\n" {
		t.Errorf("stderr=%q", errOut)
	}
}

// A server that accepts and never answers fails within the probe's limit.
func TestHealthcheckHangingServerTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	port, err := strconv.Atoi(portOf(t, srv.Listener.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	start := time.Now()
	code := probe(context.Background(), port, 200*time.Millisecond, &errOut)
	if code != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("code=%d after %s stderr=%q", code, time.Since(start), errOut.String())
	}
	oneLine(t, errOut.String())
}

func TestHealthcheckNon200IsExitOneWithoutTheBody(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusNoContent, http.StatusFound} {
		port, _ := probeServer(t, status, "fail body-must-not-be-printed")
		code, out, errOut := healthcheckRun(t, map[string]string{"PORT": port})
		if code != 1 || out != "" {
			t.Fatalf("%d: code=%d stdout=%q stderr=%q", status, code, out, errOut)
		}
		oneLine(t, errOut)
		if errOut != "healthcheck: /healthz answered "+strconv.Itoa(status)+"\n" {
			t.Errorf("%d: stderr=%q", status, errOut)
		}
	}
}

// A redirect is not followed: only the server's own 200 counts.
func TestHealthcheckDoesNotFollowRedirects(t *testing.T) {
	target, hits := probeServer(t, http.StatusOK, "ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:"+target+"/healthz", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	code, _, errOut := healthcheckRun(t, map[string]string{"PORT": portOf(t, srv.Listener.Addr())})
	if code != 1 || *hits != 0 {
		t.Fatalf("code=%d hits=%d stderr=%q", code, *hits, errOut)
	}
}

func TestHealthcheckInvalidPortIsExitOneWithoutTheValue(t *testing.T) {
	for _, v := range []string{"0", "65536", "ZZ99"} {
		code, out, errOut := healthcheckRun(t, map[string]string{"PORT": v})
		if code != 1 || out != "" {
			t.Fatalf("PORT=%q: code=%d stdout=%q stderr=%q", v, code, out, errOut)
		}
		oneLine(t, errOut)
		if !strings.Contains(errOut, "PORT") || strings.Contains(errOut, v) {
			t.Errorf("PORT=%q: stderr=%q", v, errOut)
		}
	}
}

func TestHealthcheckTakesNoArguments(t *testing.T) {
	code, out, errOut := healthcheckRun(t, map[string]string{"PORT": "1"}, "extra")
	if code != 2 || out != "" || errOut != "healthcheck: takes no arguments\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}
