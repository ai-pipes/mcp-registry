package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// operations runs the same failure through both operations.
func operations(t *testing.T, f *fake, over ...func(*kernel.UpstreamTarget)) map[string]error {
	t.Helper()
	c := newClient()
	_, listErr := c.ListTools(context.Background(), target(f, over...))
	_, callErr := c.CallTool(context.Background(), target(f, over...), "tool_0", json.RawMessage(`{}`))
	return map[string]error{"ListTools": listErr, "CallTool": callErr}
}

func TestUnauthorizedAtEveryStep(t *testing.T) {
	for _, step := range []string{"initialize", "notifications/initialized", "tools/list", "tools/call"} {
		t.Run(step, func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) { c.tools, c.failOn, c.failStatus, c.session = rawTools(1), step, 401, "s" })
			var err error
			if step == "tools/call" {
				_, err = newClient().CallTool(context.Background(), target(f), "tool_0", nil)
			} else {
				_, err = newClient().ListTools(context.Background(), target(f))
			}
			_ = wantKind(t, err, kernel.UpstreamUnauthorized)
			// An attempt that fails ends there: nothing after the refused request, no DELETE.
			reqs := f.requests()
			if last := reqs[len(reqs)-1]; last.RPC != step {
				t.Errorf("requests after the refused one: %v", f.methods())
			}
		})
	}
}

func TestRefusedAtInitializeIsASingleRequest(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.failStatus, c.failBody = 401, "nope" })
	for name, err := range operations(t, f) {
		_ = wantKind(t, err, kernel.UpstreamUnauthorized)
		_ = name
	}
	if got := f.methods(); fmt.Sprint(got) != "[POST initialize POST initialize]" {
		t.Fatalf("requests = %v, want one initialize per operation", got)
	}
}

func TestUnauthorizedWinsOverTheJSONRPCError(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) {
		c.failStatus, c.failBody = 401, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"nope"}}`
	})
	for _, err := range operations(t, f) {
		_ = wantKind(t, err, kernel.UpstreamUnauthorized)
	}
}

func TestHTTPFailuresAreTransport(t *testing.T) {
	for _, status := range []int{400, 403, 404, 405, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) { c.tools, c.failStatus, c.failBody = rawTools(1), status, "secret-body-text" })
			for name, err := range operations(t, f) {
				ue := wantKind(t, err, kernel.UpstreamTransport)
				if strings.Contains(ue.Error(), "secret-body-text") {
					t.Errorf("%s: the body is in the error: %v", name, ue)
				}
			}
		})
	}
}

func TestNon2xxWithAJSONRPCBodyIsTransportNotRejected(t *testing.T) {
	for _, status := range []int{400, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) {
				c.tools, c.failStatus = rawTools(1), status
				c.failBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params: owner"}}`
			})
			for name, err := range operations(t, f) {
				ue := wantKind(t, err, kernel.UpstreamTransport)
				if ue.Rejection != nil {
					t.Errorf("%s: transport error carries a rejection: %+v", name, ue.Rejection)
				}
			}
		})
	}
	// The same through the real answer path: a JSON-RPC error sent with HTTP 500 to tools/call.
	f := newFake(t)
	f.set(func(c *cfg) { c.rpcErr, c.rpcErrHTTP = &rpcErr{-32603, "boom"}, 500 })
	_, err := newClient().CallTool(context.Background(), target(f), "t", nil)
	_ = wantKind(t, err, kernel.UpstreamTransport)
}

func TestJSONRPCErrorWith200IsRejected(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) {
				c.tools, c.sse, c.rpcErrOn = rawTools(1), sse, "tools/call"
				c.rpcErr = &rpcErr{-32602, "Invalid params: owner is required"}
			})
			_, err := newClient().CallTool(context.Background(), target(f), "tool_0", nil)
			ue := wantKind(t, err, kernel.UpstreamRejected)
			if ue.Rejection == nil || ue.Rejection.Code != -32602 || ue.Rejection.Message != "Invalid params: owner is required" {
				t.Fatalf("rejection = %+v", ue.Rejection)
			}
			if strings.Contains(ue.Error(), "owner") {
				t.Errorf("the upstream text is in Error(): %v", ue)
			}
			// the same for tools/list
			f.set(func(c *cfg) { c.rpcErrOn = "tools/list" })
			_, err = newClient().ListTools(context.Background(), target(f))
			ue = wantKind(t, err, kernel.UpstreamRejected)
			if ue.Rejection == nil || ue.Rejection.Code != -32602 {
				t.Fatalf("rejection = %+v", ue.Rejection)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.delay = rawTools(1), 5*time.Second })
	short := func(tg *kernel.UpstreamTarget) { tg.Timeout = 150 * time.Millisecond }
	start := time.Now()
	for name, err := range operations(t, f, short) {
		_ = wantKind(t, err, kernel.UpstreamTimeout)
		_ = name
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("two operations with a 150 ms deadline took %v", d)
	}
}

func TestTimeoutCoversTheWholeOperationNotEachStep(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.pageSize, c.delay = rawTools(8), 1, 60*time.Millisecond })
	start := time.Now()
	_, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.Timeout = 250 * time.Millisecond }))
	_ = wantKind(t, err, kernel.UpstreamTimeout)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestCallerCancellationIsNotAnUpstreamTimeout(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.delay = 5 * time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := newClient().CallTool(ctx, target(f), "t", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want one that is context.Canceled", err)
	}
	if _, ok := kernel.AsUpstreamError(err); ok {
		t.Fatalf("a caller's cancellation became an upstream error: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation was not prompt")
	}

	// A deadline of the caller's own context is the caller's too.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_, err = newClient().CallTool(ctx2, target(f), "t", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if _, ok := kernel.AsUpstreamError(err); ok {
		t.Fatalf("a caller's deadline became an upstream error: %v", err)
	}

	// Already cancelled: nothing is sent.
	f2 := newFake(t)
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	if _, err := newClient().ListTools(ctx3, target(f2)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f2.requests()); n != 0 {
		t.Fatalf("%d requests after cancellation", n)
	}
}

func TestResponseAboveTheLimitIsRefusedWithoutReadingItAll(t *testing.T) {
	for name, declared := range map[string]int64{"chunked": 0, "declared": 512 * mib} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) { c.hugeBody, c.declared = 512*mib, declared })
			start := time.Now()
			_, err := newClient().CallTool(context.Background(), target(f, func(tg *kernel.UpstreamTarget) {
				tg.MaxResponseBytes, tg.Timeout = 64<<10, 30*time.Second
			}), "t", nil)
			_ = wantKind(t, err, kernel.UpstreamTooLarge)
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("took %v: the limit was not enforced as the bytes arrived", d)
			}
			// 512 MiB were on offer; only what the sockets could buffer left the server.
			if w := f.written.Load(); w > 64*mib {
				t.Fatalf("the server wrote %d MiB before the client stopped it", w/mib)
			}
		})
	}
}

func TestResponseWithinTheLimitPasses(t *testing.T) {
	f := newFake(t)
	text := strings.Repeat("x", 40_000)
	f.set(func(c *cfg) {
		c.callResult = json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, text))
	})
	got, err := newClient().CallTool(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.MaxResponseBytes = 65536 }), "t", nil)
	if err != nil || len(got) < 40_000 {
		t.Fatalf("err = %v, %d bytes", err, len(got))
	}
}

func TestOversizedAnswerInSSEFramingIsTooLarge(t *testing.T) {
	f := newFake(t)
	text := strings.Repeat("x", 80_000)
	f.set(func(c *cfg) {
		c.sse = true
		c.callResult = json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, text))
	})
	_, err := newClient().CallTool(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.MaxResponseBytes = 65536 }), "t", nil)
	_ = wantKind(t, err, kernel.UpstreamTooLarge)
}

func TestLimitCountsAllResponsesOfTheOperation(t *testing.T) {
	f := newFake(t)
	big := strings.Repeat("y", 20_000)
	tools := make([]json.RawMessage, 5)
	for i := range tools {
		tools[i] = json.RawMessage(fmt.Sprintf(`{"name":"t%d","description":%q,"inputSchema":{"type":"object"}}`, i, big))
	}
	f.set(func(c *cfg) { c.tools, c.pageSize = tools, 1 })
	// five pages of ~20 KB each: every one is far below the limit, together they are above it
	_, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.MaxResponseBytes = 65536 }))
	_ = wantKind(t, err, kernel.UpstreamTooLarge)

	// and the same pages fit when the limit allows the total
	if got, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.MaxResponseBytes = 4 * mib })); err != nil || len(got) != 5 {
		t.Fatalf("err = %v, %d tools", err, len(got))
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	for _, step := range []string{"", "tools/list"} {
		t.Run("at "+step, func(t *testing.T) {
			other := newFake(t)
			f := newFake(t)
			f.set(func(c *cfg) { c.tools, c.redirectTo, c.failOn = rawTools(1), other.url(), step })
			_, err := newClient().ListTools(context.Background(), target(f))
			_ = wantKind(t, err, kernel.UpstreamTransport)
			if n := len(other.requests()); n != 0 {
				t.Fatalf("the redirect target got %d requests: %v", n, other.methods())
			}
			for _, r := range other.requests() {
				if r.Header.Get("Authorization") != "" {
					t.Fatal("the credential reached the redirect target")
				}
			}
		})
	}
	// every redirect status
	for _, status := range []int{301, 302, 303, 307, 308} {
		other := newFake(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.url(), status)
		}))
		_, err := newClient().ListTools(context.Background(), kernel.UpstreamTarget{
			URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer x"}, Timeout: 5 * time.Second, MaxResponseBytes: mib,
		})
		srv.Close()
		_ = wantKind(t, err, kernel.UpstreamTransport)
		if n := len(other.requests()); n != 0 {
			t.Fatalf("status %d: the redirect target got %d requests", status, n)
		}
	}
}

func TestDroppedConnectionIsTransport(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.drop = true })
	_, err := newClient().CallTool(context.Background(), target(f), "t", nil)
	_ = wantKind(t, err, kernel.UpstreamTransport)
}

func TestUnreachableAndInvalidURLsAreTransport(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens there now
	for _, u := range []string{"http://" + addr + "/mcp", "not a url", "", "ftp://example.com/mcp", "http:///mcp", "http://%zz/"} {
		t.Run(u, func(t *testing.T) {
			_, err := newClient().ListTools(context.Background(), kernel.UpstreamTarget{
				URL: u, Headers: map[string]string{"Authorization": "Bearer x"}, Timeout: 3 * time.Second, MaxResponseBytes: mib,
			})
			_ = wantKind(t, err, kernel.UpstreamTransport)
		})
	}
}

func TestInvalidHeaderIsTransportAndNeverQuoted(t *testing.T) {
	f := newFake(t)
	_, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) {
		tg.Headers = map[string]string{"Authorization": "Bearer bad\nvalue-xyz"}
	}))
	ue := wantKind(t, err, kernel.UpstreamTransport)
	if strings.Contains(fmt.Sprintf("%v %+v %#v", ue, ue, ue), "value-xyz") {
		t.Fatal("a header value is in the error")
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

func TestEndlessPaginationIsBounded(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.pageSize, c.endless = rawTools(2), 1, true })
	_, err := newClient().ListTools(context.Background(), target(f))
	_ = wantKind(t, err, kernel.UpstreamTransport)
	if n := f.count("tools/list"); n != maxPages {
		t.Fatalf("followed %d pages, want exactly %d", n, maxPages)
	}
}

func TestTooManyToolsAreTransport(t *testing.T) {
	f := newFake(t)
	tools := make([]json.RawMessage, maxTools+1)
	for i := range tools {
		tools[i] = json.RawMessage(fmt.Sprintf(`{"name":"t%d","inputSchema":{"type":"object"}}`, i))
	}
	f.set(func(c *cfg) { c.tools = tools })
	_, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.MaxResponseBytes = 16 * mib }))
	_ = wantKind(t, err, kernel.UpstreamTransport)
}

func TestPaginationEndsOnNullCursor(t *testing.T) {
	// a server that answers one page with "nextCursor": null (the TypeScript client would refuse it; a documented difference)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in rpcIn
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		switch in.Method {
		case "initialize":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"x","version":"1"}}}`, in.ID)
		case "tools/list":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"a","inputSchema":{"type":"object"}}],"nextCursor":null}}`, in.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	got, err := newClient().ListTools(context.Background(), kernel.UpstreamTarget{URL: srv.URL, Timeout: 5 * time.Second, MaxResponseBytes: mib})
	if err != nil || len(got) != 1 {
		t.Fatalf("err = %v, %d tools", err, len(got))
	}
}

func TestErrorTextsCarryNothingOfTheExchange(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.failStatus, c.failBody = 500, "body-secret-123" })
	u := strings.Replace(f.url(), "http://", "http://user-name:pass-word@", 1) + "?token=query-secret"
	for _, status := range []int{500, 401, 403} {
		f.set(func(c *cfg) { c.failStatus = status })
		_, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) {
			tg.URL = u
			tg.Headers = map[string]string{"Authorization": "Bearer header-secret-456"}
		}))
		ue := kindOf(t, err)
		for _, shown := range []string{ue.Error(), fmt.Sprintf("%v|%+v|%#v|%s", err, err, err, err)} {
			for _, secret := range []string{"body-secret-123", "header-secret-456", "user-name", "pass-word", "query-secret", "127.0.0.1"} {
				if strings.Contains(shown, secret) {
					t.Errorf("status %d: %q is in %q", status, secret, shown)
				}
			}
		}
	}
	// a JSON-RPC rejection: the text is reachable only through Rejection
	f2 := newFake(t)
	f2.set(func(c *cfg) { c.rpcErr = &rpcErr{-32603, "upstream-text-789"} })
	_, err := newClient().CallTool(context.Background(), target(f2), "t", nil)
	ue := wantKind(t, err, kernel.UpstreamRejected)
	if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "upstream-text-789") && ue.Rejection == nil {
		t.Fatal("unreachable")
	}
	if strings.Contains(ue.Error(), "upstream-text-789") {
		t.Fatalf("rejection text in Error(): %v", ue)
	}
}

func TestCertificatesAreVerified(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a request reached a server whose certificate is not trusted")
	}))
	defer srv.Close()
	_, err := newClient().ListTools(context.Background(), kernel.UpstreamTarget{
		URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer x"}, Timeout: 5 * time.Second, MaxResponseBytes: mib,
	})
	_ = wantKind(t, err, kernel.UpstreamTransport)
}

// A stateful server of the official SDK (session ids, DELETE) talks to the client: ListTools and
// CallTool work and the session is ended once.
func TestInteroperatesWithTheSDKServer(t *testing.T) {
	newServer := func(*http.Request) *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "sdk", Version: "1"}, nil)
		type in struct {
			Who string `json:"who"`
		}
		mcp.AddTool(s, &mcp.Tool{Name: "greet.v1", Description: "greets", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
			func(_ context.Context, _ *mcp.CallToolRequest, a in) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi " + a.Who}}}, nil, nil
			})
		return s
	}
	var seenAuth []string
	var methods []string
	h := mcp.NewStreamableHTTPHandler(newServer, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	tg := kernel.UpstreamTarget{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer sdk-tok"}, Timeout: 10 * time.Second, MaxResponseBytes: mib}

	tools, err := newClient().ListTools(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "greet.v1" || string(tools[0].Annotations) == "" {
		t.Fatalf("tools = %+v", tools)
	}
	res, err := newClient().CallTool(context.Background(), tg, "greet.v1", json.RawMessage(`{"who":"you"}`))
	if err != nil || !strings.Contains(string(res), "hi you") {
		t.Fatalf("CallTool: %v %s", err, res)
	}
	deletes := 0
	for _, m := range methods {
		if m == http.MethodDelete {
			deletes++
		}
		if m == http.MethodGet {
			t.Errorf("a GET was sent: %v", methods)
		}
	}
	if deletes != 0 {
		t.Errorf("no DELETE is ever sent, got %d: %v", deletes, methods)
	}
	for _, a := range seenAuth {
		if a != "Bearer sdk-tok" {
			t.Errorf("a request without the credential: %q", a)
		}
	}
}

// No DELETE is ever sent, on any path: a stateful upstream that would hang it costs nothing.
func TestNoSessionDeleteAndNoWaitForOne(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.session, c.hangDelete = rawTools(2), "s-1", true })
	c := newClient()
	start := time.Now()
	if got, err := c.ListTools(context.Background(), target(f)); err != nil || len(got) != 2 {
		t.Fatalf("ListTools: %v, %d tools", err, len(got))
	}
	if _, err := c.CallTool(context.Background(), target(f), "tool_0", nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("two successes took %v: the operation waited for something", d)
	}
	for _, r := range f.requests() {
		if r.HTTP == http.MethodDelete {
			t.Fatalf("a DELETE was sent: %v", f.methods())
		}
	}
	// and not after a failure either
	f2 := newFake(t)
	f2.set(func(c *cfg) { c.session, c.failOn, c.failStatus = "s", "tools/call", 500 })
	_, _ = c.CallTool(context.Background(), target(f2), "t", nil)
	for _, r := range f2.requests() {
		if r.HTTP == http.MethodDelete {
			t.Fatalf("a DELETE was sent after a failure: %v", f2.methods())
		}
	}
}

func serverRequests(t *testing.T, n int, hang bool) (*fake, error) {
	t.Helper()
	f := newFake(t)
	f.set(func(c *cfg) { c.sse, c.serverReqs, c.hangReply = true, n, hang })
	_, err := newClient().CallTool(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.Timeout = 3 * time.Second }), "t", nil)
	return f, err
}

func TestServerRequestsAreCappedPerOperation(t *testing.T) {
	f, err := serverRequests(t, maxServerRequests, false)
	if err != nil {
		t.Fatalf("%d server requests: %v", maxServerRequests, err)
	}
	if n := f.count("(response)"); n != maxServerRequests {
		t.Fatalf("answered %d, want %d", n, maxServerRequests)
	}
	for range 3 { // deterministic
		f, err = serverRequests(t, maxServerRequests+1, false)
		_ = wantKind(t, err, kernel.UpstreamTransport)
		if n := f.count("(response)"); n > maxServerRequests {
			t.Fatalf("answered %d requests, the cap is %d", n, maxServerRequests)
		}
	}
	if maxServerRequests != 32 {
		t.Fatalf("cap = %d, want 32", maxServerRequests)
	}
}

func TestHungReplyDoesNotDelayOrFailTheOperation(t *testing.T) {
	for range 3 {
		start := time.Now()
		_, err := serverRequests(t, 3, true)
		if err != nil {
			t.Fatalf("a hung reply endpoint failed the operation: %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v: the read loop waited for a reply", d)
		}
	}
}

func TestFailingReplyDoesNotFailTheOperation(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.sse, c.serverReqs, c.failOn, c.failStatus = true, 2, "(response)", 400 })
	if _, err := newClient().CallTool(context.Background(), target(f), "t", nil); err != nil {
		t.Fatalf("a refused reply failed the operation: %v", err)
	}
}

func TestProtocolHeadersWinOverTargetHeaders(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.session = rawTools(1), "real-session" })
	hostile := map[string]string{
		"Authorization":        "Bearer tok-1",
		"mcp-protocol-version": "1999-01-01",
		"MCP-SESSION-ID":       "forged",
		"accept":               "text/plain",
		"content-type":         "text/plain",
		"Content-Length":       "1",
		"Host":                 "evil.example",
	}
	if _, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.Headers = hostile })); err != nil {
		t.Fatal(err)
	}
	for i, r := range f.requests() {
		if got := r.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("request %d: Accept = %q", i, got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("request %d: Content-Type = %q", i, got)
		}
		if got := r.Header.Get("Mcp-Protocol-Version"); got == "1999-01-01" || (i > 0 && got != "2025-11-25") {
			t.Errorf("request %d: Mcp-Protocol-Version = %q", i, got)
		}
		if got := r.Header.Get("Mcp-Session-Id"); got == "forged" || (i > 0 && got != "real-session") {
			t.Errorf("request %d: Mcp-Session-Id = %q", i, got)
		}
		if r.Host == "evil.example" {
			t.Errorf("request %d: Host = %q", i, r.Host)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("request %d: Authorization = %q", i, got)
		}
	}
}

func TestListToolsAsksNothingWhenTheServerHasNoToolsCapability(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) {
		c.tools, c.noTools, c.rpcErr, c.rpcErrOn = rawTools(2), true, &rpcErr{-32601, "Method not found"}, "tools/list"
	})
	got, err := newClient().ListTools(context.Background(), target(f))
	if err != nil || len(got) != 0 {
		t.Fatalf("err = %v, %d tools, want none and no error", err, len(got))
	}
	if n := f.count("tools/list"); n != 0 {
		t.Fatalf("tools/list was sent %d times", n)
	}
}

func TestPageWalkStopsOnARepeatedPage(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.pageSize, c.repeat = rawTools(3), 2, true })
	got, err := newClient().ListTools(context.Background(), target(f))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "tool_0" || got[1].Name != "tool_1" {
		t.Fatalf("tools = %d, want the 2 of the first page once", len(got))
	}
	if n := f.count("tools/list"); n != 2 {
		t.Fatalf("tools/list sent %d times, want 2", n)
	}
}

func TestEmptyNextCursorIsFollowedLikeTheTypeScriptWalk(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.emptyCur = rawTools(2), true })
	got, err := newClient().ListTools(context.Background(), target(f))
	if err != nil || len(got) != 2 {
		t.Fatalf("err = %v, %d tools", err, len(got))
	}
	// The first page is asked without parameters, the next with cursor "" and, being the same page
	// under the same cursor, ends the walk.
	var lists []seen
	for _, r := range f.requests() {
		if r.RPC == "tools/list" {
			lists = append(lists, r)
		}
	}
	if len(lists) != 2 || strings.Contains(string(lists[0].Body), "params") || !strings.Contains(string(lists[1].Body), `"cursor":""`) {
		t.Fatalf("tools/list requests: %d, %s", len(lists), func() string {
			var b []string
			for _, r := range lists {
				b = append(b, string(r.Body))
			}
			return strings.Join(b, " | ")
		}())
	}
}

func TestPageCapIsTheSDKs64(t *testing.T) {
	if maxPages != 64 {
		t.Fatalf("maxPages = %d, want 64 (DEFAULT_LIST_MAX_PAGES of the TypeScript SDK)", maxPages)
	}
}

// Nothing of what upstream sent or of what the SDK says reaches an error text: not the body of a
// refused request, not a JSON-RPC message, not a redirect's Location. The one exception is the
// rejection's own message.
func TestNoMarkerReachesAnErrorText(t *testing.T) {
	const (
		bodyMark = "BODY-MARK-1"
		rpcMark  = "RPC-MARK-2"
		locMark  = "LOC-MARK-3"
	)
	marks := []string{bodyMark, rpcMark, locMark, "Bearer tok-1", "tok-1", "127.0.0.1"}
	other := newFake(t)
	scenarios := map[string]struct {
		kind  kernel.UpstreamErrorKind
		setup func(*cfg)
		over  func(*kernel.UpstreamTarget)
	}{
		"401 with a body": {kernel.UpstreamUnauthorized, func(c *cfg) { c.failStatus, c.failBody = 401, bodyMark }, nil},
		"401 with a JSON-RPC body": {kernel.UpstreamUnauthorized, func(c *cfg) {
			c.failStatus, c.failBody = 401, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"`+rpcMark+`"}}`
		}, nil},
		"500 with a body": {kernel.UpstreamTransport, func(c *cfg) { c.failStatus, c.failBody = 500, bodyMark }, nil},
		"400 with a JSON-RPC error body": {kernel.UpstreamTransport, func(c *cfg) {
			c.failStatus, c.failBody = 400, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"`+rpcMark+`"}}`
		}, nil},
		"500 with a JSON-RPC error": {kernel.UpstreamTransport, func(c *cfg) { c.rpcErr, c.rpcErrHTTP = &rpcErr{-32603, rpcMark}, 500 }, nil},
		"redirect":                  {kernel.UpstreamTransport, func(c *cfg) { c.redirectTo = other.url() + "/" + locMark }, nil},
		"too large":                 {kernel.UpstreamTooLarge, func(c *cfg) { c.hugeBody = 8 * mib }, func(tg *kernel.UpstreamTarget) { tg.MaxResponseBytes = 64 << 10 }},
		"timeout":                   {kernel.UpstreamTimeout, func(c *cfg) { c.delay = 5 * time.Second }, func(tg *kernel.UpstreamTarget) { tg.Timeout = 100 * time.Millisecond }},
		"200 with a JSON-RPC error": {kernel.UpstreamRejected, func(c *cfg) { c.rpcErr = &rpcErr{-32602, rpcMark} }, nil},
	}
	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) { c.tools = rawTools(1) })
			f.set(sc.setup)
			if sc.kind == kernel.UpstreamRejected {
				f.set(func(c *cfg) { c.rpcErrOn = "tools/call" })
			}
			tg := target(f)
			if sc.over != nil {
				sc.over(&tg)
			}
			_, err := newClient().CallTool(context.Background(), tg, "tool_0", nil)
			ue := wantKind(t, err, sc.kind)
			for _, shown := range []string{ue.Error(), err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%v", err)} {
				for _, m := range marks {
					if strings.Contains(shown, m) {
						t.Errorf("%q is in %q", m, shown)
					}
				}
			}
			if sc.kind == kernel.UpstreamRejected && (ue.Rejection == nil || ue.Rejection.Message != rpcMark) {
				t.Errorf("rejection = %+v", ue.Rejection)
			}
		})
	}
	if n := len(other.requests()); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

// A header value the HTTP layer refuses (a NUL in a stored secret) fails the operation as a
// transport failure, like the reference's fetch does, and the value is not quoted.
func TestAHeaderValueWithANULIsTransportAndNotQuoted(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools = rawTools(1) })
	nul := func(tg *kernel.UpstreamTarget) {
		tg.Headers = map[string]string{"Authorization": "Bearer sek\x00ret-value"}
	}
	for name, err := range operations(t, f, nul) {
		ue := wantKind(t, err, kernel.UpstreamTransport)
		if strings.Contains(ue.Error(), "sek") {
			t.Errorf("%s: the value is in the error: %v", name, ue)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests reached the upstream", n)
	}
}
