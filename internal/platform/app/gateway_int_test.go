//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	conapp "github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	condomain "github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	coninfra "github.com/ai-pipes/mcp-registry/internal/modules/connections/infra"
	gwdomain "github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
)

// gwEnv is the scenario kit of the gateway on the composed server: people with `sk_` tokens, the
// agent's side of /mcp (call_tool over raw JSON-RPC) and counters over the call log and the quota.
type gwEnv struct {
	*conEnv
	tokens map[string]string // sub -> sk_ token
	// markers are the values that must never reach a log, an audit record or a table.
	markers []string
}

func gatewayConfig(c *config.Config) {
	catalogConfig(c)
	c.MCPMaxBodyBytes = 1 << 20
	c.CallLogRetentionDays = 90
}

func newGwEnv(t *testing.T) *gwEnv {
	t.Helper()
	return newGwEnvWith(t, gatewayConfig, nil)
}

// newGwEnvWith starts the composed server with its configuration and options edited.
func newGwEnvWith(t *testing.T, edit func(*config.Config), opts func(*Options)) *gwEnv {
	t.Helper()
	url := pgtest.StartURL(t)
	rs := startRealOpts(t, url, newKey(t), edit, modules, opts)
	return &gwEnv{conEnv: &conEnv{t: t, rs: rs, url: url}, tokens: map[string]string{}}
}

// token is a person's `sk_` token (created once).
func (g *gwEnv) token(sub string, role domain.Role) string {
	g.t.Helper()
	if tok, ok := g.tokens[sub]; ok {
		return tok
	}
	created, err := idapp.CreateToken(context.Background(), g.rs.deps, domain.Principal{UserID: adminID(g.t, g.rs, sub), Role: role, Groups: []string{}, Via: domain.ViaSession}, "gw-"+sub)
	if err != nil {
		g.t.Fatal(err)
	}
	g.tokens[sub] = created.Token
	g.markers = append(g.markers, created.Token)
	return created.Token
}

// agentReply is what an agent sees of one call_tool.
type agentReply struct {
	Status  int
	Text    string
	IsError bool
	Raw     string
}

// callTool sends one `tools/call` of call_tool over /mcp. args is the raw JSON object of the
// tool's arguments ("" omits the field).
func (g *gwEnv) callTool(tok, ref, args string) agentReply {
	g.t.Helper()
	return mcpCallTool(g.t, g.rs.base, tok, ref, args)
}

func mcpCallTool(t *testing.T, base, tok, ref, args string) agentReply {
	t.Helper()
	inner := fmt.Sprintf(`{"tool":%q`, ref)
	if args != "" {
		inner += `,"arguments":` + args
	}
	inner += "}"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"call_tool","arguments":` + inner + `}}`
	req, err := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return parseAgentReply(t, res.StatusCode, raw)
}

func parseAgentReply(t *testing.T, status int, raw []byte) agentReply {
	t.Helper()
	out := agentReply{Status: status, Raw: string(raw)}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("/mcp answered %d, not JSON: %s", status, raw)
	}
	if env.Error != nil {
		out.Text, out.IsError = env.Error.Message, true
		return out
	}
	if len(env.Result.Content) > 0 {
		out.Text = env.Result.Content[0].Text
	}
	out.IsError = env.Result.IsError
	return out
}

// okAnswer is an upstream answer with a result text carrying a marker.
func okAnswer(text string) callAnswer {
	return callAnswer{Result: fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, text)}
}

// serve makes the fake answer every call with the marked result and return the auth header it saw.
func serve(up *mcpFake, marker string) {
	up.setCall(func(upstreamCall) callAnswer { return okAnswer(marker) })
}

// register creates a server (not oauth) as the admin and expects 201.
func (g *gwEnv) register(admin person, slug string, up *mcpFake, auth map[string]any, secret string, limit int) {
	g.t.Helper()
	body := serverBody(slug, up.url(), auth)
	if secret != "" {
		body["secret"] = secret
		g.markers = append(g.markers, secret)
	}
	if limit > 0 {
		body["dailyLimit"] = limit
	}
	if status, _, raw := admin.do("POST", "/api/servers", body); status != 201 {
		g.t.Fatalf("create %s: %d %s", slug, status, raw)
	}
}

// oauthReady registers an oauth_user server with a daily limit, connects the admin and
// synchronises with their token, so the tools exist and the admin can call.
func (g *gwEnv) oauthReady(admin person, slug string, prov *oauthProvider, up *mcpFake, limit int) {
	g.t.Helper()
	body := serverBody(slug, up.url(), oauthAuth(prov))
	body["secret"] = prov.clientSecret
	if limit > 0 {
		body["dailyLimit"] = limit
	}
	if status, _, raw := admin.do("POST", "/api/servers", body); status != 201 {
		g.t.Fatalf("create %s: %d %s", slug, status, raw)
	}
	g.connect(admin, slug, prov)
	if status, raw := g.sync(admin, slug); status != 200 {
		g.t.Fatalf("sync %s: %d %s", slug, status, raw)
	}
}

func (g *gwEnv) calls(server string, outcome string) int {
	return queryInt(g.t, g.url, `select count(*)::int from gateway.calls where server = $1 and outcome = $2`, server, outcome)
}

func (g *gwEnv) quota(server string) int {
	return queryInt(g.t, g.url, `select coalesce(sum(count), 0)::int from gateway.quota where server = $1`, server)
}

func TestGatewayOnTheComposedServer(t *testing.T) {
	g := newGwEnv(t)
	alice, bob := g.admin("alice"), g.member("bob")
	tokA, tokB := g.token("alice", domain.RoleAdmin), g.token("bob", domain.RoleMember)
	const pat = "PAT-MARKER-gateway"
	g.markers = append(g.markers, pat)

	t.Run("call_tool end to end for the four kinds of authorization: the header, the arguments and the call log", func(t *testing.T) {
		const args = `{"q":"ARG-MARKER-%s","n":[1,2.50,"é"]}`
		type kind struct {
			slug   string
			auth   map[string]any
			secret string
			want   func() string // the Authorization header the upstream must see
		}
		prov := g.provider("kinds")
		kinds := []kind{
			{"k-none", map[string]any{"kind": "none"}, "", func() string { return "" }},
			{"k-shared", map[string]any{"kind": "shared_secret", "header": "Authorization", "template": "Bearer {secret}"}, "SHARED-MARKER-1", func() string { return "Bearer SHARED-MARKER-1" }},
			{"k-pat", map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Token {secret}"}, "", func() string { return "Token " + pat }},
			{"k-oauth", oauthAuth(prov), prov.clientSecret, func() string { return "Bearer " + prov.lastAccess() }},
		}
		for _, k := range kinds {
			up := newMCPFake(t, "echo")
			serve(up, "RESULT-MARKER-"+k.slug)
			switch k.slug {
			case "k-oauth":
				g.oauthReady(alice, k.slug, prov, up, 0)
			case "k-pat":
				g.register(alice, k.slug, up, k.auth, k.secret, 0)
				// The tools come from the admin's own PAT; bob calls with his own.
				for _, p := range []person{alice, bob} {
					if status, _, raw := p.do("PUT", "/api/connections/k-pat/pat", map[string]any{"token": pat}); status != 204 {
						t.Fatalf("save PAT: %d %s", status, raw)
					}
				}
				if status, raw := g.sync(alice, k.slug); status != 200 {
					t.Fatalf("sync %s: %d %s", k.slug, status, raw)
				}
			default:
				g.register(alice, k.slug, up, k.auth, k.secret, 0)
			}
			a := fmt.Sprintf(args, k.slug)
			got := g.callTool(tokB, k.slug+".echo", a)
			if k.slug == "k-oauth" {
				// OAuth: bob is not connected (only alice is), and says so; alice calls.
				if !got.IsError || !strings.Contains(got.Text, "is not connected") {
					t.Fatalf("%s: a person without a connection got %+v", k.slug, got)
				}
				got = g.callTool(tokA, k.slug+".echo", a)
			}
			if got.Status != 200 || got.IsError || got.Text != "RESULT-MARKER-"+k.slug {
				t.Fatalf("%s: %+v", k.slug, got)
			}
			seen := up.seenCalls()
			if len(seen) != 1 || seen[0].Name != "echo" || seen[0].Auth != k.want() || seen[0].Args != a {
				t.Fatalf("%s: the upstream saw %+v, want header %q and arguments %s", k.slug, seen, k.want(), a)
			}
			if g.calls(k.slug, "ok") != 1 {
				t.Fatalf("%s: no `ok` row in the call log", k.slug)
			}
		}
	})

	t.Run("the call list shows the rows to an administrator and refuses a member and a token", func(t *testing.T) {
		status, v, raw := alice.do("GET", "/api/calls?server=k-none", nil)
		list, _ := v.([]any)
		if status != 200 || len(list) != 1 {
			t.Fatalf("GET /api/calls: %d %s", status, raw)
		}
		row := list[0].(map[string]any)
		if row["server"] != "k-none" || row["tool"] != "echo" || row["outcome"] != "ok" || row["tokenId"] == nil {
			t.Fatalf("row %v", row)
		}
		for _, banned := range []string{"ARG-MARKER", "RESULT-MARKER"} {
			if strings.Contains(raw, banned) {
				t.Fatalf("the call list shows %s", banned)
			}
		}
		if status, _, _ := bob.do("GET", "/api/calls", nil); status != 403 {
			t.Fatalf("a member: %d", status)
		}
		if status, body, _ := call(t, "GET", g.rs.base+"/api/calls", "", nil, "Authorization", "Bearer "+tokA); status != 401 {
			t.Fatalf("a token on /api/calls: %d %s", status, body)
		}
	})

	t.Run("an upstream 401 on oauth_user refreshes once, retries, and spends one unit of the limit", func(t *testing.T) {
		prov, up := g.provider("retry"), newMCPFake(t, "echo")
		g.oauthReady(alice, "retry", prov, up, 5)
		first := "Bearer " + prov.lastAccess()
		up.setCall(func(c upstreamCall) callAnswer {
			if c.Auth == first {
				return callAnswer{Status: 401}
			}
			return okAnswer("RESULT-MARKER-retry")
		})
		got := g.callTool(tokA, "retry.echo", `{}`)
		if got.IsError || got.Text != "RESULT-MARKER-retry" {
			t.Fatalf("%+v", got)
		}
		seen := up.seenCalls()
		if len(seen) != 2 || seen[0].Auth != first || seen[1].Auth != "Bearer "+prov.lastAccess() || seen[1].Auth == first {
			t.Fatalf("the upstream saw %+v", seen)
		}
		if prov.count("/token", "refresh_token") != 1 {
			t.Fatalf("refreshes: %d", prov.count("/token", "refresh_token"))
		}
		if g.quota("retry") != 1 || g.calls("retry", "ok") != 1 {
			t.Fatalf("quota %d, ok rows %d: the retry must not spend a second unit nor write a second row", g.quota("retry"), g.calls("retry", "ok"))
		}
		if g.connectionStatus(alice, "retry") != "ready" {
			t.Fatal("the connection is not ready after a refresh that worked")
		}
	})

	t.Run("a second 401 expires the connection and the agent is told to reconnect", func(t *testing.T) {
		prov, up := g.provider("second"), newMCPFake(t, "echo")
		g.oauthReady(alice, "second", prov, up, 5)
		up.setCall(func(upstreamCall) callAnswer { return callAnswer{Status: 401} })
		got := g.callTool(tokA, "second.echo", `{}`)
		if !got.IsError || got.Text != "second connection expired. Ask the user to reconnect at http://localhost:3000/connections." {
			t.Fatalf("%+v", got)
		}
		if n := len(up.seenCalls()); n != 2 {
			t.Fatalf("the upstream saw %d calls, want 2 (one retry)", n)
		}
		if g.connectionStatus(alice, "second") != "expired" || g.expiredReason("second") != "rejected_upstream" {
			t.Fatalf("status %s, reason %q", g.connectionStatus(alice, "second"), g.expiredReason("second"))
		}
		if g.calls("second", "denied:expired") != 1 || g.quota("second") != 1 {
			t.Fatalf("rows %d, quota %d", g.calls("second", "denied:expired"), g.quota("second"))
		}
		// Expired: the next call is refused before the upstream and before the quota.
		got = g.callTool(tokA, "second.echo", `{}`)
		if !got.IsError || !strings.Contains(got.Text, "connection expired") || len(up.seenCalls()) != 2 || g.quota("second") != 1 {
			t.Fatalf("%+v, calls %d, quota %d", got, len(up.seenCalls()), g.quota("second"))
		}
	})

	t.Run("the provider is down and the same token is rejected: unavailable, the connection stays ready", func(t *testing.T) {
		prov, up := g.provider("down"), newMCPFake(t, "echo")
		g.oauthReady(alice, "down", prov, up, 0)
		up.setCall(func(upstreamCall) callAnswer { return callAnswer{Status: 401} })
		before := g.snapshot("down")
		prov.set(func(p *oauthProvider) { p.mode = "down" })
		got := g.callTool(tokA, "down.echo", `{}`)
		if !got.IsError || got.Text != "down is unavailable." {
			t.Fatalf("%+v", got)
		}
		if n := len(up.seenCalls()); n != 1 {
			t.Fatalf("the upstream saw %d calls, want 1 (no second call with the same token)", n)
		}
		if g.snapshot("down") != before || g.connectionStatus(alice, "down") != "ready" || g.audit("connection.expired", "down") != 0 {
			t.Fatal("an unreachable provider expired the connection")
		}
		if g.calls("down", "transport_error") != 1 {
			t.Fatal("no transport_error row")
		}
	})

	t.Run("the daily limit is per person and server; a refused call reaches nothing", func(t *testing.T) {
		up := newMCPFake(t, "echo")
		serve(up, "RESULT-MARKER-limit")
		g.register(alice, "limited", up, map[string]any{"kind": "none"}, "", 3)
		for i := range 3 {
			if got := g.callTool(tokB, "limited.echo", `{}`); got.IsError {
				t.Fatalf("call %d: %+v", i+1, got)
			}
		}
		got := g.callTool(tokB, "limited.echo", `{}`)
		if !got.IsError || !strings.HasPrefix(got.Text, "Daily limit for limited reached. Resets at ") || !strings.HasSuffix(got.Text, "T00:00:00.000Z.") {
			t.Fatalf("%+v", got)
		}
		if n := len(up.seenCalls()); n != 3 || g.quota("limited") != 3 || g.calls("limited", "denied:quota") != 1 {
			t.Fatalf("upstream calls %d, quota %d, quota rows %d", n, g.quota("limited"), g.calls("limited", "denied:quota"))
		}
		// Another person has their own allowance.
		if got := g.callTool(tokA, "limited.echo", `{}`); got.IsError {
			t.Fatalf("another person: %+v", got)
		}
	})

	t.Run("twenty parallel calls on a token due for refresh: one refresh, one answer each, one row each", func(t *testing.T) {
		prov, up := g.provider("par"), newMCPFake(t, "echo")
		prov.set(func(p *oauthProvider) { p.expiresIn = 30 }) // inside the 60 s margin: every call wants a refresh
		g.oauthReady(alice, "par", prov, up, 100)
		first := prov.lastAccess()
		refreshed := prov.count("/token", "refresh_token") // the synchronisation above refreshed too
		prov.set(func(p *oauthProvider) { p.expiresIn = 3600; p.refreshDelay = 300 * time.Millisecond })
		serve(up, "RESULT-MARKER-par")

		var wg sync.WaitGroup
		replies := make([]agentReply, 20)
		for i := range replies {
			wg.Add(1)
			go func() {
				defer wg.Done()
				replies[i] = mcpCallTool(t, g.rs.base, tokA, "par.echo", fmt.Sprintf(`{"i":%d}`, i))
			}()
		}
		wg.Wait()
		for i, r := range replies {
			if r.Status != 200 || r.IsError || r.Text != "RESULT-MARKER-par" {
				t.Fatalf("call %d: %+v", i, r)
			}
		}
		if n := prov.count("/token", "refresh_token") - refreshed; n != 1 {
			t.Fatalf("the provider saw %d refreshes for the 20 calls, want exactly 1", n)
		}
		latest := prov.lastAccess()
		if latest == first {
			t.Fatal("no new token")
		}
		seen := up.seenCalls()
		if len(seen) != 20 {
			t.Fatalf("the upstream saw %d calls", len(seen))
		}
		for _, c := range seen {
			if c.Auth != "Bearer "+latest {
				t.Fatalf("the upstream saw %q, want the refreshed token", c.Auth)
			}
		}
		if g.calls("par", "ok") != 20 || g.quota("par") != 20 {
			t.Fatalf("ok rows %d, quota %d", g.calls("par", "ok"), g.quota("par"))
		}
	})

	t.Run("upstream errors reach the agent and the log by outcome only", func(t *testing.T) {
		up := newMCPFake(t, "echo")
		g.register(alice, "errs", up, map[string]any{"kind": "none"}, "", 0)
		up.setCall(func(upstreamCall) callAnswer {
			return callAnswer{RPCError: `{"code":-32000,"message":"UPSTREAM-ERR-MARKER-1"}`}
		})
		got := g.callTool(tokB, "errs.echo", `{"q":"ARG-MARKER-errs"}`)
		if !got.IsError || got.Text != "errs rejected the call (JSON-RPC -32000): UPSTREAM-ERR-MARKER-1" {
			t.Fatalf("%+v", got)
		}
		up.setCall(func(upstreamCall) callAnswer {
			return callAnswer{Result: `{"content":[{"type":"text","text":"RESULT-MARKER-isError"}],"isError":true}`}
		})
		got = g.callTool(tokB, "errs.echo", `{}`)
		if !got.IsError || got.Text != "RESULT-MARKER-isError" {
			t.Fatalf("%+v", got)
		}
		if g.calls("errs", "upstream_error") != 2 {
			t.Fatalf("upstream_error rows: %d", g.calls("errs", "upstream_error"))
		}
		if got := g.callTool(tokB, "errs.nothing", `{}`); !got.IsError || !strings.Contains(got.Text, "is not available") || g.calls("errs", "denied:not_found") != 1 {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("a failed call-log write changes nothing for the agent and is logged once without data", func(t *testing.T) {
		up := newMCPFake(t, "echo")
		serve(up, "RESULT-MARKER-logfail")
		g.register(alice, "logfail", up, map[string]any{"kind": "none"}, "", 0)
		exec(t, g.url, `create function gateway.zz_fail() returns trigger language plpgsql as $$ begin raise exception 'ZZ-TRIGGER-MARKER'; end $$`)
		exec(t, g.url, `create trigger zz_fail before insert on gateway.calls for each row execute function gateway.zz_fail()`)
		got := g.callTool(tokB, "logfail.echo", `{"q":"ARG-MARKER-logfail"}`)
		exec(t, g.url, `drop trigger zz_fail on gateway.calls`)
		if got.Status != 200 || got.IsError || got.Text != "RESULT-MARKER-logfail" {
			t.Fatalf("the agent's answer depended on the log: %+v", got)
		}
		if g.calls("logfail", "ok") != 0 {
			t.Fatal("a row was written despite the failing trigger")
		}
		recs := g.rs.logs.find(t, "gateway: call log record failed")
		if len(recs) != 1 || recs[0]["server"] != "logfail" || recs[0]["outcome"] != "ok" {
			t.Fatalf("records: %v", recs)
		}
		if got := g.callTool(tokB, "logfail.echo", `{}`); got.IsError || g.calls("logfail", "ok") != 1 {
			t.Fatalf("after the trigger is gone: %+v", got)
		}
	})

	t.Run("no marker of an argument, a result, a secret or an upstream message is in any table or log", func(t *testing.T) {
		all := append([]string{}, g.markers...)
		for _, p := range g.provs {
			all = append(all, p.secrets()...)
		}
		all = append(all, "ARG-MARKER-", "RESULT-MARKER-", "UPSTREAM-ERR-MARKER-", "ZZ-TRIGGER-MARKER")
		if len(all) < 30 {
			t.Fatalf("only %d markers", len(all))
		}
		tables := queryText(t, g.url, `select string_agg(format('%I.%I', schemaname, tablename), ',') from pg_tables where schemaname in ('gateway', 'platform', 'identity', 'catalog', 'connections', 'search')`)
		var dump strings.Builder
		for _, tbl := range strings.Split(tables, ",") {
			dump.WriteString(queryText(t, g.url, `select coalesce(string_agg(to_jsonb(t)::text, ' '), '') from `+tbl+` t`))
		}
		logs := g.rs.logs.String() + strings.Join(g.oldLogs, "")
		for _, s := range all {
			// Tables: the catalog keeps the upstream's own tool text, which carries no marker;
			// "ARG-MARKER-" and the others are prefixes, so a hit means a value of ours.
			if strings.Contains(dump.String(), s) {
				t.Errorf("a table holds a marker (prefix %q)", s[:min(12, len(s))])
			}
			if strings.Contains(logs, s) {
				t.Errorf("the log holds a marker (prefix %q)", s[:min(12, len(s))])
			}
		}
	})
}

// holdConnections pauses ONE read of a person's connection: the window between the gateway having
// read the server it will call and its asking connections for the person's secret.
type holdConnections struct {
	conapp.ConnectionStore
	arm     atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (h *holdConnections) Get(ctx context.Context, userID, server string) (*condomain.Connection, error) {
	if h.arm.CompareAndSwap(true, false) {
		h.entered <- struct{}{}
		<-h.release
	}
	return h.ConnectionStore.Get(ctx, userID, server)
}

// buildWithHold is the composed server's module list (without search) whose connections store is
// held by hold.
func buildWithHold(hold *holdConnections) func(Runtime) []Module {
	return func(rt Runtime) []Module {
		id := newIdentityWiring(rt)
		cat := newCatalogWiring(rt, id)
		stores := coninfra.NewPg(rt.Pool)
		hold.ConnectionStore = stores.Connections
		con := connectionsWiringOver(rt, id, cat, hold, stores.Pending)
		gw := newGatewayWiring(rt, id, cat, con, newSearchWiring(rt, cat))
		return []Module{id.module(), platformModule(rt, id), cat.module(), con.module(), gw.module()}
	}
}

// A call that read configuration A must never send a token issued for B to A's URL, nor to B's,
// even when the person saved a token for B in between.
func TestACallNeverSendsATokenIssuedForAnotherConfiguration(t *testing.T) {
	url := pgtest.StartURL(t)
	hold := &holdConnections{entered: make(chan struct{}), release: make(chan struct{})}
	build := buildWithHold(hold)
	rs := startRealWith(t, url, newKey(t), gatewayConfig, build)
	g := &gwEnv{conEnv: &conEnv{t: t, rs: rs, url: url}, tokens: map[string]string{}}
	alice := g.admin("alice")
	tok := g.token("alice", domain.RoleAdmin)
	upA, upB := newMCPFake(t, "alpha"), newMCPFake(t, "alpha")
	serve(upA, "RESULT-MARKER-A")
	serve(upB, "RESULT-MARKER-B")
	const patA, patB = "PAT-MARKER-A", "PAT-MARKER-B"
	auth := map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Token {secret}"}
	if status, _, raw := alice.do("POST", "/api/servers", serverBody("moving", upA.url(), auth)); status != 201 {
		t.Fatalf("create: %d %s", status, raw)
	}
	if status, _, raw := alice.do("PUT", "/api/connections/moving/pat", map[string]any{"token": patA}); status != 204 {
		t.Fatalf("save PAT: %d %s", status, raw)
	}
	if status, raw := g.sync(alice, "moving"); status != 200 {
		t.Fatalf("sync: %d %s", status, raw)
	}

	hold.arm.Store(true)
	done := make(chan agentReply, 1)
	go func() { done <- mcpCallTool(t, rs.base, tok, "moving.alpha", `{}`) }()
	await(t, hold.entered, "the call to read the person's connection")

	// The server moves to B and the person saves a token for B.
	if status, _, raw := alice.do("PUT", "/api/servers/moving", serverBody("moving", upB.url(), auth)); status != 200 {
		t.Fatalf("move: %d %s", status, raw)
	}
	if status, _, raw := alice.do("PUT", "/api/connections/moving/pat", map[string]any{"token": patB}); status != 204 {
		t.Fatalf("save the PAT for B: %d %s", status, raw)
	}
	before := g.snapshot("moving")
	close(hold.release)
	got := await(t, done, "the call to finish")

	if !got.IsError || got.Text != "moving is unavailable." {
		t.Fatalf("the call that read A: %+v", got)
	}
	if len(upA.seenCalls()) != 0 || len(upB.seenCalls()) != 0 {
		t.Fatalf("a token was sent: A saw %+v, B saw %+v", upA.seenCalls(), upB.seenCalls())
	}
	if g.snapshot("moving") != before || g.connectionStatus(alice, "moving") != "ready" {
		t.Fatal("the connection for B was changed")
	}
	if g.calls("moving", "transport_error") != 1 || g.quota("moving") != 0 {
		t.Fatalf("transport_error rows %d", g.calls("moving", "transport_error"))
	}
	// A new call reads B and works with B's token.
	if got := g.callTool(tok, "moving.alpha", `{}`); got.IsError || lastOf(authsOf(upB.seenCalls())) != "Token "+patB {
		t.Fatalf("against B: %+v, %+v", got, upB.seenCalls())
	}
}

func authsOf(calls []upstreamCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Auth
	}
	return out
}

// inFlight is a server with one call blocked in the upstream, and the shutdown begun while it is.
type inFlight struct {
	g     *gwEnv
	hold  chan struct{} // closing it lets the upstream answer
	reply chan agentReply
	exit  chan int
}

// startInFlight starts a server with the given grace, sends a call that blocks in the upstream and
// begins the shutdown; it returns once the log says the shutdown began and the listener is closed.
func startInFlight(t *testing.T, grace time.Duration) *inFlight {
	t.Helper()
	g := newGwEnvWith(t, gatewayConfig, func(o *Options) { o.callsGrace = grace })
	alice := g.admin("alice")
	tok := g.token("alice", domain.RoleAdmin)
	up := newMCPFake(t, "slow")
	g.register(alice, "slowsrv", up, map[string]any{"kind": "none"}, "", 0)
	f := &inFlight{g: g, hold: make(chan struct{}), reply: make(chan agentReply, 1), exit: make(chan int, 1)}
	up.setCall(func(upstreamCall) callAnswer {
		a := okAnswer("RESULT-MARKER-slow")
		a.Hold = f.hold
		return a
	})
	t.Cleanup(func() {
		select {
		case <-f.hold:
		default:
			close(f.hold)
		}
	})
	go func() { f.reply <- mcpCallTool(t, g.rs.base, tok, "slowsrv.slow", `{"q":"ARG-MARKER-shutdown"}`) }()
	await(t, up.callArrived, "the call to reach the upstream")
	go func() { f.exit <- g.rs.stop("SIGTERM") }()
	g.rs.waitFor("shutting down")
	// The listener closes at once, long before the grace ends.
	addr := strings.TrimPrefix(g.rs.base, "http://")
	deadline := time.Now().Add(waitLimit)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener is still open after the shutdown began")
		}
		time.Sleep(10 * time.Millisecond) //nolint:forbidigo // polling a socket that has no event to wait on
	}
	return f
}

// A call that finishes within the grace gets its normal answer and an `ok` row; the shutdown then
// completes cleanly (the grace is far longer than the test: nothing is cancelled).
func TestACallThatFinishesWithinTheGraceIsAnsweredNormally(t *testing.T) {
	f := startInFlight(t, time.Minute)
	select {
	case r := <-f.reply:
		t.Fatalf("the call answered before it was released: %+v", r)
	default:
	}
	close(f.hold)
	got := await(t, f.reply, "the agent's answer")
	if got.Status != 200 || got.IsError || got.Text != "RESULT-MARKER-slow" {
		t.Fatalf("%+v", got)
	}
	if code := await(t, f.exit, "the exit"); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, f.g.rs.logs.String())
	}
	if f.g.calls("slowsrv", "ok") != 1 || f.g.calls("slowsrv", "transport_error") != 0 {
		t.Fatalf("rows: ok %d, transport_error %d", f.g.calls("slowsrv", "ok"), f.g.calls("slowsrv", "transport_error"))
	}
	if len(f.g.rs.logs.find(t, "mcp tool failed")) != 0 || len(f.g.rs.logs.find(t, "shutdown timed out")) != 0 {
		t.Fatalf("error records:\n%s", f.g.rs.logs.String())
	}
}

// A call still running when the grace ends is cancelled and writes `transport_error` before the
// pool closes, inside the overall shutdown limit.
func TestACallStillRunningAtTheEndOfTheGraceIsCancelledAndItsRowIsWritten(t *testing.T) {
	f := startInFlight(t, 300*time.Millisecond)
	got := await(t, f.reply, "the agent's answer")
	if got.Status != 200 || !got.IsError {
		t.Fatalf("the agent got %+v", got)
	}
	if code := await(t, f.exit, "the exit"); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, f.g.rs.logs.String())
	}
	if len(f.g.rs.logs.find(t, "shutdown timed out")) != 0 || len(f.g.rs.logs.find(t, "shutdown failed")) != 0 {
		t.Fatalf("the shutdown did not go cleanly:\n%s", f.g.rs.logs.String())
	}
	if n := f.g.calls("slowsrv", "transport_error"); n != 1 {
		t.Fatalf("transport_error rows after the shutdown: %d\n%s", n, f.g.rs.logs.String())
	}
	if strings.Contains(f.g.rs.logs.String(), "ARG-MARKER-shutdown") {
		t.Fatal("an argument is in the log")
	}
}

// A request that arrives after the shutdown began is refused by the closed listener; it is never
// answered `Internal error.` because of the shutdown.
func TestARequestAfterTheShutdownBeganIsRefusedByTheClosedListener(t *testing.T) {
	f := startInFlight(t, time.Minute)
	req, err := http.NewRequest("POST", f.g.rs.base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	if res, err := client.Do(req); err == nil {
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		t.Fatalf("a request after the shutdown began was answered: %d %s", res.StatusCode, raw)
	}
	close(f.hold)
	await(t, f.reply, "the agent's answer")
	if code := await(t, f.exit, "the exit"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(f.g.rs.logs.find(t, "mcp tool failed")) != 0 {
		t.Fatal("an error record for a call cut by the shutdown")
	}
}

// With nothing in flight the shutdown is not held back by the grace.
func TestAShutdownWithNoCallsInFlightIsNotDelayedByTheGrace(t *testing.T) {
	g := newGwEnvWith(t, gatewayConfig, func(o *Options) { o.callsGrace = time.Minute })
	started := time.Now()
	if code := g.rs.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the shutdown took %v with nothing in flight", took)
	}
}

// The retention job runs at start and drops the call log older than CALL_LOG_RETENTION_DAYS and the
// quota counters older than yesterday (UTC).
func TestTheRetentionJobRunsAtStart(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	first := startReal(t, url, key, gatewayConfig)
	if code := first.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	const user = "11111111-1111-4111-8111-111111111111"
	exec(t, url, `insert into gateway.calls (user_id, server, tool, started_at, duration_ms, outcome) values
		($1, 'old', 't', now() - interval '91 days', 1, 'ok'), ($1, 'fresh', 't', now() - interval '89 days', 1, 'ok')`, user)
	exec(t, url, `insert into gateway.quota (user_id, server, day, count) values
		($1, 'old', (now() at time zone 'utc')::date - 2, 1), ($1, 'fresh', (now() at time zone 'utc')::date - 1, 1)`, user)

	again := startReal(t, url, key, func(c *config.Config) { gatewayConfig(c); c.CallLogRetentionDays = 90 })
	again.waitFor("gateway retention: done")
	recs := again.logs.find(t, "gateway retention: done")
	if len(recs) != 1 || recs[0]["calls"] != float64(1) || recs[0]["quotaDays"] != float64(1) {
		t.Fatalf("records: %v", recs)
	}
	if n := queryInt(t, url, `select count(*)::int from gateway.calls where server = 'old'`); n != 0 {
		t.Fatal("the old row is still there")
	}
	if n := queryInt(t, url, `select count(*)::int from gateway.calls where server = 'fresh'`) + queryInt(t, url, `select count(*)::int from gateway.quota where server = 'fresh'`); n != 2 {
		t.Fatalf("the fresh rows: %d", n)
	}
}

// The server's configuration changes between the upstream's 401 and the forced resolve: the person's
// refresh token is sent to neither the old nor the new token endpoint, the call is not retried
// anywhere, and the stored connection is untouched.
func TestAConfigurationChangedBetweenThe401AndTheForcedResolveSendsNothing(t *testing.T) {
	url := pgtest.StartURL(t)
	hold := &holdConnections{entered: make(chan struct{}), release: make(chan struct{})}
	rs := startRealWith(t, url, newKey(t), gatewayConfig, buildWithHold(hold))
	g := &gwEnv{conEnv: &conEnv{t: t, rs: rs, url: url}, tokens: map[string]string{}}
	alice := g.admin("alice")
	tok := g.token("alice", domain.RoleAdmin)
	old, moved, up := g.provider("old401"), g.provider("moved401"), newMCPFake(t, "echo")
	g.oauthReady(alice, "mv401", old, up, 5)
	// The 401 arrives, and the next read of the person's connection (the forced resolve) is held.
	up.setCall(func(upstreamCall) callAnswer {
		hold.arm.Store(true)
		return callAnswer{Status: 401}
	})
	refreshes, before := old.count("/token", "refresh_token"), g.snapshot("mv401")

	done := make(chan agentReply, 1)
	go func() { done <- mcpCallTool(t, rs.base, tok, "mv401.echo", `{}`) }()
	await(t, hold.entered, "the forced resolve to read the connection")
	next := oauthAuth(old)
	next["oauth"].(map[string]any)["tokenUrl"] = moved.endpoint("/token")
	if status, _, raw := alice.do("PUT", "/api/servers/mv401", serverBody("mv401", up.url(), next)); status != 200 {
		t.Fatalf("move the token endpoint: %d %s", status, raw)
	}
	close(hold.release)
	got := await(t, done, "the call to finish")

	if !got.IsError || got.Text != "mv401 is unavailable." {
		t.Fatalf("%+v", got)
	}
	if moved.total() != 0 || old.count("/token", "refresh_token") != refreshes {
		t.Fatalf("a refresh token was sent: new endpoint %d requests, old endpoint %d refreshes (was %d)", moved.total(), old.count("/token", "refresh_token"), refreshes)
	}
	if n := len(up.seenCalls()); n != 1 {
		t.Fatalf("the upstream saw %d calls, want 1 (no retry)", n)
	}
	if g.snapshot("mv401") != before {
		t.Fatal("the stored connection changed")
	}
	if g.calls("mv401", "transport_error") != 1 || g.quota("mv401") != 1 {
		t.Fatalf("transport_error rows %d, quota %d", g.calls("mv401", "transport_error"), g.quota("mv401"))
	}
}

// MCP_MAX_BODY_BYTES is the /mcp limit on both sides, and the REST limit stays 1 MiB.
func TestTheMCPBodyLimitIsTheConfiguredOneAndTheRESTLimitStaysOneMiB(t *testing.T) {
	const limit = 2 << 20
	g := newGwEnvWith(t, func(c *config.Config) { gatewayConfig(c); c.MCPMaxBodyBytes = limit }, nil)
	tok := g.token("alice", domain.RoleAdmin)
	padded := func(n int) string {
		const msg = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
		return strings.Repeat(" ", n-len(msg)) + msg
	}
	mcp := func(n int) int {
		status, _, _ := call(t, "POST", g.rs.base+"/mcp", "", padded(n), "Authorization", "Bearer "+tok, "Content-Type", "application/json", "Accept", "application/json, text/event-stream")
		return status
	}
	for n, want := range map[int]int{limit: 200, limit + 1: 413, 3 << 19: 200} {
		if got := mcp(n); got != want {
			t.Errorf("/mcp with %d bytes: %d, want %d", n, got, want)
		}
	}
	rest := func(n int) int {
		status, _, _ := call(t, "POST", g.rs.base+"/api/auth/link", "", padded(n))
		return status
	}
	if got := rest(1 << 20); got == 413 {
		t.Errorf("REST with exactly 1 MiB: %d", got)
	}
	for _, n := range []int{1<<20 + 1, 3 << 19} {
		if got := rest(n); got != 413 {
			t.Errorf("REST with %d bytes: %d, want 413 (the /mcp limit must not widen it)", n, got)
		}
	}
}

// CALL_LOG_RETENTION_DAYS decides what the job at start deletes.
func TestTheRetentionJobUsesTheConfiguredDays(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	first := startReal(t, url, key, gatewayConfig)
	if code := first.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	const user = "11111111-1111-4111-8111-111111111111"
	exec(t, url, `insert into gateway.calls (user_id, server, tool, started_at, duration_ms, outcome) values
		($1, 'two-days', 't', now() - interval '2 days', 1, 'ok'), ($1, 'half-day', 't', now() - interval '12 hours', 1, 'ok')`, user)
	again := startReal(t, url, key, func(c *config.Config) { gatewayConfig(c); c.CallLogRetentionDays = 1 })
	again.waitFor("gateway retention: done")
	if n := queryInt(t, url, `select count(*)::int from gateway.calls where server = 'two-days'`); n != 0 {
		t.Fatal("the row older than the configured day is still there")
	}
	if n := queryInt(t, url, `select count(*)::int from gateway.calls where server = 'half-day'`); n != 1 {
		t.Fatal("the row inside the configured day was deleted")
	}
}

// UPSTREAM_MAX_RESPONSE_BYTES limits what call_tool accepts.
func TestAnUpstreamAnswerOverTheConfiguredLimitIsRefusedWithThatSize(t *testing.T) {
	const limit = 65536
	g := newGwEnvWith(t, func(c *config.Config) { gatewayConfig(c); c.UpstreamMaxResponseBytes = limit }, nil)
	alice := g.admin("alice")
	tok := g.token("alice", domain.RoleAdmin)
	up := newMCPFake(t, "big")
	g.register(alice, "bigsrv", up, map[string]any{"kind": "none"}, "", 0)
	big := strings.Repeat("x", 3*limit/2)
	up.setCall(func(upstreamCall) callAnswer { return okAnswer(big) })
	got := g.callTool(tok, "bigsrv.big", `{}`)
	if want := gwdomain.TextTooLarge("bigsrv", limit); !got.IsError || got.Text != want {
		t.Fatalf("%q, want %q", got.Text, want)
	}
	if g.calls("bigsrv", "transport_error") != 1 {
		t.Fatal("no transport_error row")
	}
	// Under the limit the same server answers.
	up.setCall(func(upstreamCall) callAnswer { return okAnswer("small") })
	if got := g.callTool(tok, "bigsrv.big", `{}`); got.IsError || got.Text != "small" {
		t.Fatalf("%+v", got)
	}
}

// One validator serves the process: a schema that cannot be compiled is warned about once, however
// many calls use it (a validator per call would warn every time).
func TestTheArgumentValidatorIsOneForTheWholeProcess(t *testing.T) {
	g := newGwEnv(t)
	alice := g.admin("alice")
	tok := g.token("alice", domain.RoleAdmin)
	up := newMCPFake(t, "odd")
	up.schema = `{"type":"object","properties":{"a":{"$ref":"https://other.example/schema.json"}}}`
	serve(up, "RESULT-MARKER-odd")
	g.register(alice, "oddsrv", up, map[string]any{"kind": "none"}, "", 0)
	for range 3 {
		if got := g.callTool(tok, "oddsrv.odd", `{}`); got.IsError {
			t.Fatalf("%+v", got)
		}
	}
	if n := len(g.rs.logs.find(t, "gateway: tool input schema not compiled, not validating")); n != 1 {
		t.Fatalf("%d warnings for one schema over three calls, want 1", n)
	}
}
