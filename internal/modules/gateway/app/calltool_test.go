package app_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

var (
	oauthAuth  = catalog.AuthMode{Kind: catalog.AuthOAuthUser, Header: "Authorization", Template: "Bearer {secret}"}
	patAuth    = catalog.AuthMode{Kind: catalog.AuthPATUser, Header: "X-Token", Template: "token {secret}"}
	sharedAuth = catalog.AuthMode{Kind: catalog.AuthSharedSecret, Header: "X-API-Key", Template: "key {secret}"}
)

func (h *harness) outcomes(t *testing.T) []domain.CallOutcome {
	t.Helper()
	var out []domain.CallOutcome
	for _, e := range h.log.entries {
		out = append(out, e.Outcome)
	}
	return out
}

func (h *harness) wantOneRow(t *testing.T, want domain.CallOutcome) app.CallLogEntry {
	t.Helper()
	if len(h.log.entries) != 1 || h.log.entries[0].Outcome != want {
		t.Fatalf("log rows %+v, want one %s", h.log.entries, want)
	}
	return h.log.entries[0]
}

func wantText(t *testing.T, r domain.ToolResult, want string) {
	t.Helper()
	got, ok := isErrText(r)
	if !ok || got != want {
		t.Fatalf("answer %s, want error text %q", r.JSON(), want)
	}
}

func TestSuccessPassesTheUpstreamResultAndLogsOK(t *testing.T) {
	h := newHarness()
	h.clock = &stepClock{t: epoch, steps: []time.Duration{1500 * time.Millisecond}}
	h.rebuild()
	h.up.steps = []upstreamStep{okResult(`{"content":[{"type":"text","text":"hi <b>"}],"structuredContent":{"a":1},"_meta":{"x":1},"resultType":"complete"}`)}
	res, err := h.call(context.Background(), "gitea.create_issue", `{"title":  "Bug"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(res.JSON()), `{"content":[{"type":"text","text":"hi <b>"}],"structuredContent":{"a":1}}`; got != want {
		t.Fatalf("answer %s, want %s", got, want)
	}
	e := h.wantOneRow(t, domain.OutcomeOK)
	want := app.CallLogEntry{UserID: "u-alice", TokenID: "t-1", Server: "gitea", Tool: "create_issue", StartedAt: epoch, DurationMs: 1500, Outcome: domain.OutcomeOK}
	if e != want {
		t.Fatalf("entry %+v, want %+v", e, want)
	}
	c := h.up.calls[0]
	// Arguments reach the upstream as the bytes the agent sent (ruling 4).
	if c.name != "create_issue" || c.args != `{"title":  "Bug"}` || c.target.URL != "https://gitea.example.com/mcp" ||
		len(c.target.Headers) != 0 || c.target.Timeout != 30*time.Second || c.target.MaxResponseBytes != 20<<20 {
		t.Fatalf("upstream saw %+v", c)
	}
	if h.val.seen[0] != `{"title":  "Bug"}` || h.val.schem[0] != `gitea.create_issue|{"type":"object"}` {
		t.Fatalf("validator saw %v %v", h.val.seen, h.val.schem)
	}
}

func TestNoArgumentsMeansAnEmptyObjectForTheValidator(t *testing.T) {
	h := newHarness()
	if _, err := h.call(context.Background(), "gitea.create_issue", ""); err != nil {
		t.Fatal(err)
	}
	if h.val.seen[0] != "{}" || h.up.calls[0].args != "{}" {
		t.Fatalf("validator saw %q, upstream %q", h.val.seen[0], h.up.calls[0].args)
	}
}

func TestSessionCallLogsNoToken(t *testing.T) {
	h := newHarness()
	h.alice.TokenID = ""
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	if h.log.entries[0].TokenID != "" {
		t.Fatalf("token %q", h.log.entries[0].TokenID)
	}
}

func TestToolNamesWithDotsSplitAtTheFirstDot(t *testing.T) {
	h := newHarness()
	h.addTool("gitea", "issues.create")
	if _, err := h.call(context.Background(), "gitea.issues.create", `{}`); err != nil {
		t.Fatal(err)
	}
	if h.up.calls[0].name != "issues.create" || h.log.entries[0].Server != "gitea" || h.log.entries[0].Tool != "issues.create" {
		t.Fatalf("%+v %+v", h.up.calls[0], h.log.entries[0])
	}
}

func TestNotFoundSpendsNothingAndCallsNothing(t *testing.T) {
	cases := map[string]func(h *harness) string{
		"unknown tool":   func(_ *harness) string { return "gitea.nope" },
		"no dot":         func(_ *harness) string { return "nodot" },
		"bad slug":       func(_ *harness) string { return "Bad.X" },
		"empty tool":     func(_ *harness) string { return "gitea." },
		"server removed": func(h *harness) string { delete(h.cat.servers, "gitea"); return "gitea.create_issue" },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.cat.servers["gitea"].DailyLimit = ip(1)
			ref := setup(h)
			res, err := h.call(context.Background(), ref, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			wantText(t, res, `Tool "`+ref+`" is not available. Use search_tools to find another.`)
			h.wantOneRow(t, domain.OutcomeDeniedNotFound)
			if len(h.up.calls) != 0 || len(h.quota.requests) != 0 || len(h.creds.calls) != 0 || len(h.val.seen) != 0 {
				t.Fatal("something was called")
			}
		})
	}
}

func TestLogNamesForARefThatDoesNotParse(t *testing.T) {
	h := newHarness()
	ref := strings.Repeat("é", 300)
	if _, err := h.call(context.Background(), ref, `{}`); err != nil {
		t.Fatal(err)
	}
	e := h.log.entries[0]
	if e.Server != "" || e.Tool != strings.Repeat("é", 200) {
		t.Fatalf("server %q tool len %d", e.Server, len(e.Tool))
	}
}

func TestLogNamesAreBounded(t *testing.T) {
	h := newHarness()
	long := strings.Repeat("a", 501)
	h.addTool("gitea", long)
	if _, err := h.call(context.Background(), "gitea."+long, `{}`); err != nil {
		t.Fatal(err)
	}
	if len(h.log.entries[0].Tool) != 200 || h.log.entries[0].Server != "gitea" {
		t.Fatalf("%q %d", h.log.entries[0].Server, len(h.log.entries[0].Tool))
	}
}

func TestInvalidArgumentsAreRefusedBeforeCredentialsAndQuota(t *testing.T) {
	h := newHarness()
	s := h.cat.servers["gitea"]
	s.Auth, s.DailyLimit = patAuth, ip(1)
	h.val.bad = &app.ArgsCheck{Path: "/title", Message: "must be string"}
	res, err := h.call(context.Background(), "gitea.create_issue", `{"title":1}`)
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, res, `Invalid arguments at /title: must be string. Call describe_tool("gitea.create_issue") for the schema.`)
	h.wantOneRow(t, domain.OutcomeDeniedInvalidArgs)
	if len(h.creds.calls) != 0 || len(h.quota.requests) != 0 || len(h.up.calls) != 0 {
		t.Fatal("credentials, quota or upstream touched")
	}
}

func TestSharedSecretHeader(t *testing.T) {
	h := newHarness()
	h.cat.servers["gitea"].Auth = sharedAuth
	h.cat.secrets["gitea"] = "s3"
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	if got := h.up.calls[0].target.Headers["X-API-Key"]; got != "key s3" {
		t.Fatalf("header %q", got)
	}
	if len(h.creds.calls) != 0 {
		t.Fatal("a personal credential was asked for")
	}
}

func TestSharedSecretNotSetIsRejectedWithoutQuotaOrUpstream(t *testing.T) {
	h := newHarness()
	s := h.cat.servers["gitea"]
	s.Auth, s.DailyLimit = sharedAuth, ip(1)
	res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, res, "Gitea rejected the gateway credentials. Ask the administrator to check the server key.")
	h.wantOneRow(t, domain.OutcomeTransportError)
	if len(h.up.calls) != 0 || len(h.quota.requests) != 0 {
		t.Fatal("upstream or quota touched")
	}
}

func TestAnEmptySharedSecretIsUsed(t *testing.T) {
	h := newHarness()
	h.cat.servers["gitea"].Auth = sharedAuth
	h.cat.secrets["gitea"] = ""
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	if len(h.up.calls) != 1 {
		t.Fatal("not called")
	}
}

func TestPersonalCredentialFailures(t *testing.T) {
	cases := []struct {
		failure connections.CredentialFailure
		outcome domain.CallOutcome
		text    string
	}{
		{connections.FailureNeedsConnection, domain.OutcomeDeniedNeedsConnection, "Gitea is not connected. Ask the user to open https://mcp.example.com/connections and connect Gitea."},
		{connections.FailureExpired, domain.OutcomeDeniedExpired, "Gitea connection expired. Ask the user to reconnect at https://mcp.example.com/connections."},
		{connections.FailureUnavailable, domain.OutcomeTransportError, "Gitea is unavailable."},
	}
	for _, c := range cases {
		t.Run(string(c.failure), func(t *testing.T) {
			h := newHarness()
			s := h.cat.servers["gitea"]
			s.Auth, s.DailyLimit = oauthAuth, ip(1)
			h.creds.results = []connections.CredentialResult{{Failure: c.failure}}
			res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
			if err != nil {
				t.Fatal(err)
			}
			wantText(t, res, c.text)
			h.wantOneRow(t, c.outcome)
			if len(h.quota.requests) != 0 || len(h.up.calls) != 0 {
				t.Fatal("no quota and no upstream before credentials")
			}
		})
	}
}

func TestAnUnknownCredentialFailureIsUnexpected(t *testing.T) {
	h := newHarness()
	h.cat.servers["gitea"].Auth = oauthAuth
	h.creds.results = []connections.CredentialResult{{Failure: "new_one"}}
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err == nil {
		t.Fatal("want an error")
	}
	h.wantOneRow(t, domain.OutcomeTransportError)
}

// The secret is sent to the target the gateway read, and only the person's own secret goes there.
func TestPersonalCredentialIsResolvedForTheTargetRead(t *testing.T) {
	h := newHarness()
	h.cat.servers["gitea"].Auth = patAuth
	h.creds.results = []connections.CredentialResult{{Secret: "pat-1"}}
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	c := h.creds.calls[0]
	if c.userID != "u-alice" || c.force || c.target.Slug != "gitea" || c.target.URL != "https://gitea.example.com/mcp" {
		t.Fatalf("resolve %+v", c)
	}
	if got := h.up.calls[0].target.Headers["X-Token"]; got != "token pat-1" {
		t.Fatalf("header %q", got)
	}
}

func TestDailyLimit(t *testing.T) {
	h := newHarness()
	h.cat.servers["gitea"].DailyLimit = ip(2)
	clock := &stepClock{t: time.Date(2026, 10, 30, 23, 59, 0, 0, time.UTC)}
	h.clock = clock
	h.rebuild()
	var texts []string
	for range 3 {
		res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(res.JSON()))
	}
	if !contains(texts[2], "Daily limit for Gitea reached. Resets at 2026-10-31T00:00:00.000Z.") {
		t.Fatalf("third: %s", texts[2])
	}
	if want := []domain.CallOutcome{domain.OutcomeOK, domain.OutcomeOK, domain.OutcomeDeniedQuota}; !slices.Equal(h.outcomes(t), want) {
		t.Fatalf("outcomes %v", h.outcomes(t))
	}
	if len(h.up.calls) != 2 || h.quota.requests[0] != (app.QuotaRequest{UserID: "u-alice", Server: "gitea", Day: "2026-10-30", Limit: 2}) {
		t.Fatalf("calls %d, request %+v", len(h.up.calls), h.quota.requests[0])
	}
}

func TestNoLimitNeverConsultsTheQuota(t *testing.T) {
	h := newHarness()
	for range 3 {
		if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.quota.requests) != 0 {
		t.Fatal("quota consulted")
	}
}

func TestUpstreamFailures(t *testing.T) {
	cases := []struct {
		name    string
		step    upstreamStep
		outcome domain.CallOutcome
		text    string
	}{
		{"timeout", upstreamStep{err: kernel.NewUpstreamError(kernel.UpstreamTimeout)}, domain.OutcomeTimeout, "Gitea did not respond in 30 s."},
		{"too large", upstreamStep{err: kernel.NewUpstreamError(kernel.UpstreamTooLarge)}, domain.OutcomeTransportError, "Gitea returned a response larger than 20 MiB."},
		{"transport", upstreamStep{err: kernel.NewUpstreamError(kernel.UpstreamTransport)}, domain.OutcomeTransportError, "Gitea is unavailable."},
		{"rejected", upstreamStep{err: kernel.RejectedUpstreamError(-32602, "  Invalid params: owner is required ")}, domain.OutcomeUpstreamError, "Gitea rejected the call (JSON-RPC -32602): Invalid params: owner is required"},
		{"rejected without detail", upstreamStep{err: &kernel.UpstreamError{Kind: kernel.UpstreamRejected}}, domain.OutcomeUpstreamError, "Gitea rejected the call (JSON-RPC 0)."},
		{"the client's own limits are a transport failure", upstreamStep{err: kernel.NewUpstreamError(kernel.UpstreamTransport)}, domain.OutcomeTransportError, "Gitea is unavailable."},
		{"a result that is not a tool result", okResult(`{"content":"nope"}`), domain.OutcomeTransportError, "Gitea is unavailable."},
		{"a result that is not an object", okResult(`[1]`), domain.OutcomeTransportError, "Gitea is unavailable."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.cat.servers["gitea"].DailyLimit = ip(5)
			h.up.steps = []upstreamStep{c.step}
			res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
			if err != nil {
				t.Fatal(err)
			}
			wantText(t, res, c.text)
			h.wantOneRow(t, c.outcome)
			if len(h.quota.requests) != 1 {
				t.Fatal("the unit is spent and never refunded")
			}
		})
	}
}

func TestUpstreamIsErrorIsPassedAsIs(t *testing.T) {
	h := newHarness()
	h.up.steps = []upstreamStep{okResult(`{"content":[{"type":"text","text":"boom"}],"isError":true}`)}
	res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.JSON()) != `{"content":[{"type":"text","text":"boom"}],"isError":true}` {
		t.Fatalf("%s", res.JSON())
	}
	h.wantOneRow(t, domain.OutcomeUpstreamError)
}

func TestTheRejectionMessageNeverReachesTheLog(t *testing.T) {
	h := newHarness()
	h.up.steps = []upstreamStep{{err: kernel.RejectedUpstreamError(-1, "private-marker")}}
	h.log.err = errors.New("db down")
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	if contains(h.lg.text(), "private-marker") || contains(h.lg.text(), "db down") {
		t.Fatalf("log: %s", h.lg.text())
	}
}

func TestA401OnSharedSecretAndNoneIsRejectedWithoutRetry(t *testing.T) {
	for name, auth := range map[string]catalog.AuthMode{"none": {Kind: catalog.AuthNone}, "shared": sharedAuth} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.cat.servers["gitea"].Auth = auth
			h.cat.secrets["gitea"] = "s"
			h.up.steps = []upstreamStep{unauthorized()}
			res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
			if err != nil {
				t.Fatal(err)
			}
			wantText(t, res, "Gitea rejected the gateway credentials. Ask the administrator to check the server key.")
			h.wantOneRow(t, domain.OutcomeTransportError)
			if len(h.up.calls) != 1 || len(h.creds.expired) != 0 {
				t.Fatal("retry or expiry")
			}
		})
	}
}

func TestA401OnAPATExpiresTheConnection(t *testing.T) {
	h := newHarness()
	h.cat.servers["gitea"].Auth = patAuth
	h.up.steps = []upstreamStep{unauthorized()}
	res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, res, "Gitea connection expired. Ask the user to reconnect at https://mcp.example.com/connections.")
	h.wantOneRow(t, domain.OutcomeDeniedExpired)
	if len(h.up.calls) != 1 || len(h.creds.calls) != 1 || !slices.Equal(h.creds.expired, []connections.MarkExpiredCommand{{UserID: "u-alice", Server: "gitea"}}) {
		t.Fatalf("calls %d resolves %d expired %v", len(h.up.calls), len(h.creds.calls), h.creds.expired)
	}
}

func oauthServer(h *harness) {
	s := h.cat.servers["gitea"]
	s.Auth, s.DailyLimit = oauthAuth, ip(1)
}

func TestA401OnOAuthRefreshesOnceAndRetriesWithTheNewSecret(t *testing.T) {
	h := newHarness()
	oauthServer(h)
	h.creds.results = []connections.CredentialResult{{Secret: "old"}, {Secret: "new"}}
	h.up.steps = []upstreamStep{unauthorized(), okResult(`{"content":[]}`)}
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	h.wantOneRow(t, domain.OutcomeOK)
	if h.up.calls[0].target.Headers["Authorization"] != "Bearer old" || h.up.calls[1].target.Headers["Authorization"] != "Bearer new" {
		t.Fatalf("headers %v %v", h.up.calls[0].target.Headers["Authorization"], h.up.calls[1].target.Headers["Authorization"])
	}
	// Both resolves carry the whole target the gateway read, not just its slug.
	if len(h.creds.calls) != 2 || h.creds.calls[0].force || !h.creds.calls[1].force {
		t.Fatalf("resolves %+v", h.creds.calls)
	}
	if want := *h.cat.servers["gitea"]; !reflect.DeepEqual(h.creds.calls[0].target, want) || !reflect.DeepEqual(h.creds.calls[1].target, want) {
		t.Fatalf("targets %+v / %+v, want %+v", h.creds.calls[0].target, h.creds.calls[1].target, want)
	}
	if h.sec.count() != 1 {
		t.Fatalf("the secrets comparer was consulted %d times, want once on the forced path", h.sec.count())
	}
	if len(h.quota.requests) != 1 || len(h.creds.expired) != 0 {
		t.Fatal("the retry spends no second unit and expires nothing")
	}
}

func TestASecond401ExpiresTheConnection(t *testing.T) {
	h := newHarness()
	oauthServer(h)
	h.creds.results = []connections.CredentialResult{{Secret: "old"}, {Secret: "new"}}
	h.up.steps = []upstreamStep{unauthorized(), unauthorized()}
	res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, res, "Gitea connection expired. Ask the user to reconnect at https://mcp.example.com/connections.")
	h.wantOneRow(t, domain.OutcomeDeniedExpired)
	if len(h.up.calls) != 2 || len(h.quota.requests) != 1 || len(h.creds.expired) != 1 {
		t.Fatalf("calls %d quota %d expired %d", len(h.up.calls), len(h.quota.requests), len(h.creds.expired))
	}
}

// Owner decision of 2026-11-23: the same secret back means the provider is down and nothing was
// refreshed: no MarkExpired, no second upstream call, "unavailable".
func TestA401ThenTheSameSecretIsUnavailableNotExpired(t *testing.T) {
	h := newHarness()
	oauthServer(h)
	h.creds.results = []connections.CredentialResult{{Secret: "same"}}
	h.up.steps = []upstreamStep{unauthorized()}
	res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, res, "Gitea is unavailable.")
	h.wantOneRow(t, domain.OutcomeTransportError)
	if len(h.up.calls) != 1 || len(h.creds.expired) != 0 || len(h.creds.calls) != 2 || len(h.quota.requests) != 1 {
		t.Fatalf("calls %d expired %d resolves %d", len(h.up.calls), len(h.creds.expired), len(h.creds.calls))
	}
}

func TestA401ThenARefusedRefresh(t *testing.T) {
	cases := []struct {
		failure connections.CredentialFailure
		outcome domain.CallOutcome
		text    string
	}{
		{connections.FailureExpired, domain.OutcomeDeniedExpired, "Gitea connection expired. Ask the user to reconnect at https://mcp.example.com/connections."},
		{connections.FailureUnavailable, domain.OutcomeTransportError, "Gitea is unavailable."},
		{connections.FailureNeedsConnection, domain.OutcomeDeniedNeedsConnection, "Gitea is not connected. Ask the user to open https://mcp.example.com/connections and connect Gitea."},
	}
	for _, c := range cases {
		t.Run(string(c.failure), func(t *testing.T) {
			h := newHarness()
			oauthServer(h)
			h.creds.results = []connections.CredentialResult{{Secret: "old"}, {Failure: c.failure}}
			h.up.steps = []upstreamStep{unauthorized()}
			res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
			if err != nil {
				t.Fatal(err)
			}
			wantText(t, res, c.text)
			h.wantOneRow(t, c.outcome)
			if len(h.up.calls) != 1 || len(h.creds.expired) != 0 {
				t.Fatal("one upstream call and no expiry")
			}
		})
	}
}

func TestTheRetryMayTimeOut(t *testing.T) {
	h := newHarness()
	oauthServer(h)
	h.creds.results = []connections.CredentialResult{{Secret: "old"}, {Secret: "new"}}
	h.up.steps = []upstreamStep{unauthorized(), {err: kernel.NewUpstreamError(kernel.UpstreamTimeout)}}
	res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	wantText(t, res, "Gitea did not respond in 30 s.")
	h.wantOneRow(t, domain.OutcomeTimeout)
}

// The 401 handling uses the target read before the call, not a fresh read.
func TestTheRetryUsesTheTargetReadBeforeTheCall(t *testing.T) {
	h := newHarness()
	oauthServer(h)
	h.creds.results = []connections.CredentialResult{{Secret: "old"}, {Secret: "new"}}
	h.up.steps = []upstreamStep{{wait: func(context.Context) error {
		delete(h.cat.servers, "gitea") // gone while the call is in flight
		return kernel.NewUpstreamError(kernel.UpstreamUnauthorized)
	}}, okResult(`{"content":[]}`)}
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	if len(h.up.calls) != 2 || h.up.calls[1].target.URL != "https://gitea.example.com/mcp" {
		t.Fatalf("calls %+v", h.up.calls)
	}
}

func TestUnexpectedFailuresAreReturnedAndLoggedAsTransportError(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(h *harness){
		"tool lookup":   func(h *harness) { h.cat.toolErr = boom },
		"server lookup": func(h *harness) { h.cat.serverErr = boom },
		"server secret": func(h *harness) { h.cat.servers["gitea"].Auth = sharedAuth; h.cat.secretErr = boom },
		"credentials":   func(h *harness) { h.cat.servers["gitea"].Auth = patAuth; h.creds.err = boom },
		"quota":         func(h *harness) { h.cat.servers["gitea"].DailyLimit = ip(1); h.quota.err = boom },
		"upstream":      func(h *harness) { h.up.steps = []upstreamStep{{err: boom}} },
		"mark expired": func(h *harness) {
			h.cat.servers["gitea"].Auth = patAuth
			h.up.steps = []upstreamStep{unauthorized()}
			h.creds.expiredErr = boom
		},
		"refresh resolve": func(h *harness) {
			oauthServer(h)
			h.up.steps = []upstreamStep{unauthorized()}
			h.creds.results = []connections.CredentialResult{{Secret: "o"}}
			h.creds.err = nil
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			setup(h)
			if name == "refresh resolve" {
				// the first resolve works, the forced one fails
				h.creds.err = nil
				h.creds.results = []connections.CredentialResult{{Secret: "o"}}
				first := true
				h.d.Credentials = &failSecond{fakeCreds: h.creds, boom: boom, first: &first}
			}
			res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
			if !errors.Is(err, boom) || len(res.Content) != 0 {
				t.Fatalf("err %v, answer %s", err, res.JSON())
			}
			h.wantOneRow(t, domain.OutcomeTransportError)
		})
	}
}

// failSecond lets the first resolve through and fails the forced one.
type failSecond struct {
	*fakeCreds
	boom  error
	first *bool
}

func (f *failSecond) ResolveForSending(ctx context.Context, userID string, target catalog.ServerCallTarget, force bool) (connections.CredentialResult, error) {
	if force {
		return connections.CredentialResult{}, f.boom
	}
	return f.fakeCreds.ResolveForSending(ctx, userID, target, force)
}

func TestAFailedLogWriteIsLoggedAndSwallowed(t *testing.T) {
	h := newHarness()
	h.log.err = errors.New("db down: secret-marker")
	res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
	if err != nil || len(res.Content) == 0 {
		t.Fatalf("err %v answer %s", err, res.JSON())
	}
	if len(h.lg.records) != 1 || h.lg.records[0].msg != "gateway: call log record failed" {
		t.Fatalf("records %v", h.lg.records)
	}
	if contains(h.lg.text(), "secret-marker") {
		t.Fatalf("message text logged: %s", h.lg.text())
	}
	if got, want := h.lg.text(), "[{gateway: call log record failed [err errors.errorString server gitea tool create_issue outcome ok]}]"; got != want {
		t.Fatalf("log %s, want %s", got, want)
	}
}

func TestAFailedLogWriteDoesNotHideTheOriginalError(t *testing.T) {
	h := newHarness()
	boom := errors.New("bug")
	h.cat.toolErr = boom
	h.log.err = errors.New("db down")
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if len(h.lg.records) != 1 {
		t.Fatalf("records %v", h.lg.records)
	}
}

// Ruling 7: the client leaving changes nothing; the upstream is called and the row written.
func TestACancelledRequestStillCallsUpstreamAndWritesTheRow(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.up.steps = []upstreamStep{{wait: func(c context.Context) error { return c.Err() }, result: []byte(`{"content":[]}`)}}
	res, err := h.call(ctx, "gitea.create_issue", `{}`)
	if err != nil || len(res.Content) == 0 {
		t.Fatalf("err %v", err)
	}
	h.wantOneRow(t, domain.OutcomeOK)
	if h.log.ctxErr != nil {
		t.Fatalf("the log write ran on a cancelled context: %v", h.log.ctxErr)
	}
}

// At process shutdown (BaseContext) a call in flight is cancelled and logs transport_error.
func TestShutdownCancelsACallInFlightAndStillWritesTheRow(t *testing.T) {
	h := newHarness()
	base, shutdown := context.WithCancel(context.Background())
	h.rebuild()
	h.d.BaseContext = base
	started := make(chan struct{})
	h.up.steps = []upstreamStep{{wait: func(c context.Context) error {
		close(started)
		<-c.Done()
		return c.Err()
	}}}
	type answer struct {
		res domain.ToolResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := app.CallTool(context.Background(), h.d, app.CallToolCommand{Principal: h.alice, Ref: "gitea.create_issue", Arguments: []byte(`{}`)})
		done <- answer{res, err}
	}()
	<-started
	shutdown()
	select {
	case a := <-done:
		// The call was cut by the shutdown: the server's transport text, not Internal error.
		if a.err != nil {
			t.Fatalf("err %v, want the transport answer", a.err)
		}
		wantText(t, a.res, "Gitea is unavailable.")
	case <-time.After(10 * time.Second):
		t.Fatal("the call in flight was not cancelled by BaseContext")
	}
	h.wantOneRow(t, domain.OutcomeTransportError)
	if h.log.ctxErr != nil {
		t.Fatalf("the log write ran on a cancelled context: %v", h.log.ctxErr)
	}
}

// A base context that is already done cancels the call's context before anything runs (not a
// few microseconds later from another goroutine).
func TestACallStartedAfterTheBaseContextEndedIsCancelledBeforeItRuns(t *testing.T) {
	h := newHarness()
	base, shutdown := context.WithCancel(context.Background())
	shutdown()
	h.rebuild()
	h.d.BaseContext = base
	for range 200 {
		_, _ = h.call(context.Background(), "gitea.create_issue", `{}`)
		h.cat.mu.Lock()
		err := h.cat.toolCtxErr
		h.cat.mu.Unlock()
		if err == nil {
			t.Fatal("the first step of the call ran on a context that was not yet cancelled")
		}
	}
}

func TestStartedAtAndDurationAreMillisecondsAndNeverNegative(t *testing.T) {
	h := newHarness()
	h.clock = &stepClock{t: epoch.Add(123456789 * time.Nanosecond), steps: []time.Duration{-5 * time.Second}}
	h.rebuild()
	if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
		t.Fatal(err)
	}
	e := h.log.entries[0]
	if !e.StartedAt.Equal(epoch.Add(123*time.Millisecond)) || e.DurationMs != 0 {
		t.Fatalf("%v %d", e.StartedAt, e.DurationMs)
	}
}

func TestThePublicURLKeepsItsPathAndLosesTrailingSlashes(t *testing.T) {
	h := newHarness()
	h.cat.servers["gitea"].Auth = oauthAuth
	h.creds.results = []connections.CredentialResult{{Failure: connections.FailureNeedsConnection}}
	h.d.PublicURL = "https://example.com/gw//"
	res, _ := h.call(context.Background(), "gitea.create_issue", `{}`)
	wantText(t, res, "Gitea is not connected. Ask the user to open https://example.com/gw/connections and connect Gitea.")
}

func TestConcurrentCallsDoNotMixAnswers(t *testing.T) {
	h := newHarness()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.call(context.Background(), "gitea.create_issue", `{}`); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(h.log.entries) != 20 || len(h.up.calls) != 20 {
		t.Fatalf("rows %d calls %d", len(h.log.entries), len(h.up.calls))
	}
}

// A panic inside the call is an unexpected failure: the row is still written (transport_error, as
// the reference's finally records it) and the caller gets an error that does not carry the value.
func TestAPanicInTheCallStillWritesTheRowAndIsAnError(t *testing.T) {
	const value = "PANIC-VALUE-31337"
	for name, setup := range map[string]func(h *harness){
		"upstream":  func(h *harness) { h.up.steps = []upstreamStep{{wait: func(context.Context) error { panic(value) }}} },
		"catalog":   func(h *harness) { h.cat.onServerFor = func() { panic(value) } },
		"validator": func(h *harness) { h.val.panicWith = value },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			setup(h)
			res, err := h.call(context.Background(), "gitea.create_issue", `{}`)
			if err == nil || strings.Contains(err.Error(), value) || len(res.Content) != 0 {
				t.Fatalf("err %v, answer %s", err, res.JSON())
			}
			h.wantOneRow(t, domain.OutcomeTransportError)
			if strings.Contains(h.lg.text(), value) {
				t.Fatalf("the log carries the panic value: %s", h.lg.text())
			}
		})
	}
}
