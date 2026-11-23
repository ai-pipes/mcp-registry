package http_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// callElement is one tools/call of call_tool in a batch.
func callElement(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"call_tool","arguments":{"tool":"gw.echo","arguments":{"n":%d}}}}`, id, id)
}

func batchRequest(ctx context.Context, t *testing.T, e *env, ids ...int) *http.Request {
	t.Helper()
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = callElement(id)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", e.url+"/mcp", strings.NewReader("["+strings.Join(parts, ",")+"]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range append([]string{"Authorization: Bearer " + aliceToken}, mcpHeaders...) {
		k, v, _ := strings.Cut(h, ":")
		req.Header.Set(k, strings.TrimSpace(v))
	}
	return req
}

// A batch answer is written element by element: the first answer reaches the client while the
// second call is still running, so no answer waits in memory for the others.
func TestABatchAnswerIsWrittenAsEachElementIsDone(t *testing.T) {
	e := newEnv(t)
	e.gw.echoArgs = true
	release := make(chan struct{})
	e.gw.onCall = func(_ context.Context, n int) {
		if n == 2 {
			<-release
		}
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	type answer struct {
		res *http.Response
		err error
	}
	got := make(chan answer, 1)
	go func() {
		res, err := http.DefaultClient.Do(batchRequest(t.Context(), t, e, 1, 2))
		got <- answer{res, err}
	}()
	var res *http.Response
	select {
	case a := <-got:
		if a.err != nil {
			t.Fatal(a.err)
		}
		res = a.res
	case <-time.After(10 * time.Second):
		t.Fatal("the answer did not start while the second call was running")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	rd := bufio.NewReader(res.Body)
	first, err := rd.ReadString('}')
	for err == nil && !strings.HasSuffix(first, `"id":1}`) {
		var more string
		more, err = rd.ReadString('}')
		first += more
	}
	if err != nil {
		t.Fatalf("reading the first answer: %v (%q)", err, first)
	}
	close(release)
	rest, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	var batch []map[string]any
	if err := json.Unmarshal([]byte(first+string(rest)), &batch); err != nil || len(batch) != 2 || batch[0]["id"] != float64(1) || batch[1]["id"] != float64(2) {
		t.Fatalf("batch %v: %s%s", err, first, rest)
	}
}

// Once the client has gone, the remaining elements of a batch are not run.
func TestABatchStopsWhenTheClientHasGone(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	e.gw.onCall = func(callCtx context.Context, n int) {
		if n == 1 {
			close(started)
			<-callCtx.Done()
		}
	}
	done := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(batchRequest(ctx, t, e, 1, 2, 3))
		if err == nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the cancelled request got an answer")
	}
	// Shutdown waits for the handler to return.
	shut, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if err := e.srv.Shutdown(shut); err != nil {
		t.Fatal(err)
	}
	e.gw.mu.Lock()
	defer e.gw.mu.Unlock()
	if e.gw.started != 1 {
		t.Fatalf("%d calls ran, want 1: the batch went on after the client had gone", e.gw.started)
	}
}
