package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const mib = 1 << 20

func target(f *fake, over ...func(*kernel.UpstreamTarget)) kernel.UpstreamTarget {
	t := kernel.UpstreamTarget{
		URL:              f.url(),
		Headers:          map[string]string{"Authorization": "Bearer tok-1", "X-Extra": "yes"},
		Timeout:          10 * time.Second,
		MaxResponseBytes: mib,
	}
	for _, o := range over {
		o(&t)
	}
	return t
}

func newClient() *Client { return New(Options{Version: "9.9.9"}) }

func rawTools(n int) []json.RawMessage {
	out := make([]json.RawMessage, n)
	for i := range out {
		out[i] = json.RawMessage(fmt.Sprintf(`{"name":"tool_%d","title":"Tool %d","description":"does %d","inputSchema":{"type":"object","properties":{"a":{"type":"string"}}},"annotations":{"readOnlyHint":true}}`, i, i, i))
	}
	return out
}

// kindOf asserts err is an UpstreamError and returns it.
func kindOf(t *testing.T, err error) *kernel.UpstreamError {
	t.Helper()
	if err == nil {
		t.Fatal("want an UpstreamError, got nil")
	}
	ue, ok := kernel.AsUpstreamError(err)
	if !ok {
		t.Fatalf("want an UpstreamError, got %T: %v", err, err)
	}
	return ue
}

func wantKind(t *testing.T, err error, kind kernel.UpstreamErrorKind) *kernel.UpstreamError {
	t.Helper()
	ue := kindOf(t, err)
	if ue.Kind != kind {
		t.Fatalf("kind = %s, want %s", ue.Kind, kind)
	}
	return ue
}

func TestListToolsOnePage(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools = rawTools(3) })
	got, err := newClient().ListTools(context.Background(), target(f))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "tool_0" || got[2].Name != "tool_2" {
		t.Fatalf("tools = %+v", got)
	}
	if got[1].Title == nil || *got[1].Title != "Tool 1" || got[1].Description == nil || *got[1].Description != "does 1" {
		t.Fatalf("title/description lost: %+v", got[1])
	}
	if string(got[0].InputSchema) != `{"type":"object","properties":{"a":{"type":"string"}}}` {
		t.Fatalf("inputSchema = %s", got[0].InputSchema)
	}
	if string(got[0].Annotations) != `{"readOnlyHint":true}` {
		t.Fatalf("annotations = %s", got[0].Annotations)
	}
}

func TestListToolsFollowsEveryPage(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.pageSize = rawTools(7), 3 })
	got, err := newClient().ListTools(context.Background(), target(f))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("got %d tools, want 7", len(got))
	}
	for i, tool := range got {
		if tool.Name != fmt.Sprintf("tool_%d", i) {
			t.Fatalf("tool %d is %s", i, tool.Name)
		}
	}
	if n := f.count("tools/list"); n != 3 {
		t.Fatalf("tools/list sent %d times, want 3 pages", n)
	}
}

// The exact exchange of one successful ListTools and one CallTool: the session is initialised,
// the notification follows, no standalone GET stream is opened.
func TestRequestSequence(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools = rawTools(1) })
	c := newClient()
	if _, err := c.ListTools(context.Background(), target(f)); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST initialize", "POST notifications/initialized", "POST tools/list"}
	if got := f.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ListTools sent %v, want %v", got, want)
	}
	f2 := newFake(t)
	if _, err := c.CallTool(context.Background(), target(f2), "tool_0", json.RawMessage(`{"a":"b"}`)); err != nil {
		t.Fatal(err)
	}
	want = []string{"POST initialize", "POST notifications/initialized", "POST tools/call"}
	if got := f2.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("CallTool sent %v, want %v", got, want)
	}
}

func TestHandshakeParameters(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools = rawTools(1) })
	if _, err := newClient().ListTools(context.Background(), target(f)); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	var init struct {
		Params struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      struct{ Name, Version string }
		}
	}
	if err := json.Unmarshal(reqs[0].Body, &init); err != nil {
		t.Fatal(err)
	}
	if init.Params.ProtocolVersion != "2025-11-25" {
		t.Errorf("protocolVersion = %q, want 2025-11-25 (no discovery request of a newer revision)", init.Params.ProtocolVersion)
	}
	if string(init.Params.Capabilities) != "{}" {
		t.Errorf("capabilities = %s, want {}", init.Params.Capabilities)
	}
	if init.Params.ClientInfo.Name != "mcp-registry" || init.Params.ClientInfo.Version != "9.9.9" {
		t.Errorf("clientInfo = %+v", init.Params.ClientInfo)
	}
	// After initialize the negotiated version travels on every request.
	for _, r := range reqs[1:] {
		if got := r.Header.Get("Mcp-Protocol-Version"); got != "2025-11-25" {
			t.Errorf("%s %s: Mcp-Protocol-Version = %q", r.HTTP, r.RPC, got)
		}
	}
	for _, r := range reqs {
		if r.HTTP == http.MethodGet {
			t.Errorf("a standalone stream was opened: %v", f.methods())
		}
	}
}

func TestUnsupportedServerProtocolVersionIsTransport(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.protocol = "1999-01-01" })
	_, err := newClient().ListTools(context.Background(), target(f))
	_ = wantKind(t, err, kernel.UpstreamTransport)
}

func TestToolFieldsAreKeptAsSent(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) {
		c.tools = []json.RawMessage{
			// the annotations of the compat scenario: a false hint is kept, not dropped
			json.RawMessage(`{"name":"a","title":"A","description":"da","inputSchema":{"type":"object","properties":{"n":{"type":"integer","maximum":12345678901234567890,"minimum":1.0}}},"outputSchema":{"type":"object","properties":{"r":{"type":"string"}}},"annotations":{"readOnlyHint":true,"idempotentHint":false}}`),
			// no title, description, outputSchema or annotations
			json.RawMessage(`{"name":"b.with.dots","inputSchema":{"type":"object"}}`),
			// present but empty
			json.RawMessage(`{"name":"c","title":"","description":"","inputSchema":{"type":"object"},"outputSchema":{},"annotations":{}}`),
		}
	})
	got, err := newClient().ListTools(context.Background(), target(f))
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := got[0], got[1], got[2]
	if string(a.Annotations) != `{"readOnlyHint":true,"idempotentHint":false}` {
		t.Errorf("annotations = %s", a.Annotations)
	}
	if !strings.Contains(string(a.InputSchema), `12345678901234567890`) || !strings.Contains(string(a.InputSchema), `"minimum":1.0`) {
		t.Errorf("number precision lost: %s", a.InputSchema)
	}
	if string(a.OutputSchema) != `{"type":"object","properties":{"r":{"type":"string"}}}` {
		t.Errorf("outputSchema = %s", a.OutputSchema)
	}
	if b.Name != "b.with.dots" {
		t.Errorf("name = %q", b.Name)
	}
	if b.Title != nil || b.Description != nil || b.OutputSchema != nil || b.Annotations != nil {
		t.Errorf("absent fields became present: %+v", b)
	}
	if c.Title == nil || *c.Title != "" || c.Description == nil || *c.Description != "" {
		t.Errorf("empty title/description lost: %+v", c)
	}
	if string(c.OutputSchema) != `{}` || string(c.Annotations) != `{}` {
		t.Errorf("empty outputSchema/annotations lost: %s %s", c.OutputSchema, c.Annotations)
	}
}

// The TypeScript client parses annotations with a strict zod object: unknown keys are dropped.
func TestAnnotationsKeepOnlyTheKnownKeys(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) {
		c.tools = []json.RawMessage{json.RawMessage(`{"name":"a","inputSchema":{"type":"object"},"annotations":{"openWorldHint":false,"vendorHint":"x","title":"T","destructiveHint":true}}`)}
	})
	got, err := newClient().ListTools(context.Background(), target(f))
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Annotations) != `{"title":"T","destructiveHint":true,"openWorldHint":false}` {
		t.Fatalf("annotations = %s", got[0].Annotations)
	}
}

func TestMalformedToolsAreTransport(t *testing.T) {
	cases := map[string]string{
		"no name":                  `{"inputSchema":{"type":"object"}}`,
		"name not a string":        `{"name":3,"inputSchema":{"type":"object"}}`,
		"no inputSchema":           `{"name":"a"}`,
		"inputSchema not object":   `{"name":"a","inputSchema":[]}`,
		"inputSchema without type": `{"name":"a","inputSchema":{"properties":{}}}`,
		"title not a string":       `{"name":"a","title":1,"inputSchema":{"type":"object"}}`,
		"annotations null":         `{"name":"a","inputSchema":{"type":"object"},"annotations":null}`,
		"annotation wrong type":    `{"name":"a","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":"yes"}}`,
		"tool not an object":       `7`,
	}
	for name, tool := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) { c.tools = []json.RawMessage{json.RawMessage(tool)} })
			_, err := newClient().ListTools(context.Background(), target(f))
			_ = wantKind(t, err, kernel.UpstreamTransport)
		})
	}
}

func TestSSEAndPlainJSONAreBothUnderstood(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) { c.tools, c.pageSize, c.sse = rawTools(4), 2, sse })
			got, err := newClient().ListTools(context.Background(), target(f))
			if err != nil || len(got) != 4 {
				t.Fatalf("ListTools: %v, %d tools", err, len(got))
			}
			res, err := newClient().CallTool(context.Background(), target(f), "tool_0", json.RawMessage(`{"a":1}`))
			if err != nil || !strings.Contains(string(res), `"content"`) {
				t.Fatalf("CallTool: %v %s", err, res)
			}
		})
	}
}

func TestServerToClientRequestIsAnsweredNotFound(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.sse, c.serverReq = true, true })
	if _, err := newClient().CallTool(context.Background(), target(f), "t", nil); err != nil {
		t.Fatal(err)
	}
	var answered bool
	for _, r := range f.requests() {
		if r.RPC == "(response)" {
			answered = true
			if !strings.Contains(string(r.Body), `"srv-1"`) || !strings.Contains(string(r.Body), "-32601") {
				t.Errorf("answer to the server request = %s", r.Body)
			}
		}
	}
	if !answered {
		t.Fatal("the server's request was not answered")
	}
}

func TestCallToolReturnsTheResultUnchanged(t *testing.T) {
	result := `{"content":[{"type":"text","text":"hi"},{"type":"image","data":"AAAA","mimeType":"image/png"}],"structuredContent":{"n":12345678901234567890,"f":1.0},"isError":true,"_meta":{"k":"v"},"extra":[1,2]}`
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			f := newFake(t)
			f.set(func(c *cfg) { c.callResult, c.sse = json.RawMessage(result), sse })
			got, err := newClient().CallTool(context.Background(), target(f), "tool_1", json.RawMessage(`{"a":"b"}`))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != result {
				t.Fatalf("result changed:\n got  %s\n want %s", got, result)
			}
		})
	}
}

func TestCallToolSendsTheArgumentsRaw(t *testing.T) {
	cases := []struct{ name, args, want string }{
		{"object", `{"big": 12345678901234567890, "f": 1.0, "s": "é"}`, `{"big":12345678901234567890,"f":1.0,"s":"é"}`},
		{"nil", ``, `{}`},
		{"null", `null`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			var args json.RawMessage
			if tc.args != "" {
				args = json.RawMessage(tc.args)
			}
			if _, err := newClient().CallTool(context.Background(), target(f), "a.b.c", args); err != nil {
				t.Fatal(err)
			}
			var p struct {
				Params struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"params"`
			}
			reqs := f.requests()
			if err := json.Unmarshal(reqs[len(reqs)-1].Body, &p); err != nil {
				t.Fatal(err)
			}
			if p.Params.Name != "a.b.c" || string(p.Params.Arguments) != tc.want {
				t.Fatalf("sent name %q arguments %s, want %s", p.Params.Name, p.Params.Arguments, tc.want)
			}
		})
	}
}

func TestInvalidArgumentsJSONIsNotSentAndNotAnUpstreamError(t *testing.T) {
	f := newFake(t)
	_, err := newClient().CallTool(context.Background(), target(f), "t", json.RawMessage(`{"a":`))
	if err == nil {
		t.Fatal("want an error")
	}
	if _, ok := kernel.AsUpstreamError(err); ok {
		t.Fatalf("a caller's bug is not an upstream failure: %v", err)
	}
	if len(f.requests()) != 0 {
		t.Fatalf("a request was sent: %v", f.methods())
	}
}

func TestTargetHeadersAreOnEveryRequest(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.pageSize, c.session = rawTools(5), 2, "sess-1" })
	if _, err := newClient().ListTools(context.Background(), target(f)); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) < 5 {
		t.Fatalf("too few requests to be a test: %v", f.methods())
	}
	var deletes int
	for _, r := range reqs {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("%s %s: Authorization = %q", r.HTTP, r.RPC, got)
		}
		if got := r.Header.Get("X-Extra"); got != "yes" {
			t.Errorf("%s %s: X-Extra = %q", r.HTTP, r.RPC, got)
		}
		if r.HTTP == http.MethodDelete {
			deletes++
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Errorf("DELETE without the session id")
			}
		}
	}
	if deletes != 1 {
		t.Errorf("a successful stateful session ends with one DELETE, got %d: %v", deletes, f.methods())
	}
}

func TestOperationsShareNothing(t *testing.T) {
	f := newFake(t)
	c := newClient()
	if _, err := c.ListTools(context.Background(), target(f)); err != nil {
		t.Fatal(err)
	}
	f.set(func(c *cfg) { c.session = "" })
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
	second := target(f, func(t *kernel.UpstreamTarget) { t.Headers = map[string]string{"Authorization": "Bearer tok-2"} })
	if _, err := c.ListTools(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) == 0 {
		t.Fatal("no requests")
	}
	for _, r := range reqs {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-2" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Extra"); got != "" {
			t.Errorf("a header of the previous target leaked: X-Extra = %q", got)
		}
	}
}

func TestConcurrentOperationsKeepTheirOwnCredentials(t *testing.T) {
	f := newFake(t)
	f.set(func(c *cfg) { c.tools, c.pageSize = rawTools(5), 2 })
	c := newClient()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			tok := fmt.Sprintf("Bearer tok-%d", i)
			tg := target(f, func(t *kernel.UpstreamTarget) { t.Headers = map[string]string{"Authorization": tok} })
			if _, err := c.ListTools(context.Background(), tg); err != nil {
				t.Errorf("operation %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	// Every session's requests carry one token; none carries a token of another.
	perToken := map[string]int{}
	for _, r := range f.requests() {
		perToken[r.Header.Get("Authorization")]++
	}
	if len(perToken) != 16 {
		t.Fatalf("saw %d distinct tokens, want 16: %v", len(perToken), perToken)
	}
	for tok, n := range perToken {
		if n != 5 { // initialize, initialized, three pages
			t.Errorf("%s sent %d requests, want 5", tok, n)
		}
	}
}
