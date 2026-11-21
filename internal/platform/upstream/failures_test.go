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

func TestPaginationEndsOnEmptyOrNullCursor(t *testing.T) {
	// a server that answers one page with "nextCursor": null (the TypeScript client would refuse it)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in rpcIn
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		switch in.Method {
		case "initialize":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"x","version":"1"}}}`, in.ID)
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
	if deletes != 2 {
		t.Errorf("want one DELETE per operation, got %d: %v", deletes, methods)
	}
	for _, a := range seenAuth {
		if a != "Bearer sdk-tok" {
			t.Errorf("a request without the credential: %q", a)
		}
	}
}

// The session-ending DELETE is a courtesy: an upstream that never answers it neither fails the
// operation nor holds it past its deadline.
func TestHungSessionEndDoesNotOutliveTheOperation(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.session, c.hangDelete = rawTools(2), "s-1", true })
	start := time.Now()
	got, err := newClient().ListTools(context.Background(), target(f, func(tg *kernel.UpstreamTarget) { tg.Timeout = 500 * time.Millisecond }))
	if err != nil || len(got) != 2 {
		t.Fatalf("err = %v, %d tools", err, len(got))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v with a 500 ms deadline", d)
	}
	if n := len(f.requests()); n != 4 || f.requests()[3].HTTP != http.MethodDelete {
		t.Fatalf("requests = %v", f.methods())
	}
}
