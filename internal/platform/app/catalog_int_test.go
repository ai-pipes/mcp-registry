//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
)

// mcpFake is the smallest upstream MCP server over Streamable HTTP that a synchronisation talks
// to: initialize, notifications/initialized and tools/list, as JSON. It records the Authorization
// header of every tools/list.
type mcpFake struct {
	srv *httptest.Server

	mu      sync.Mutex
	tools   []string
	down    bool
	auths   []string
	hold    chan struct{} // when set, tools/list blocks until it is closed or the request ends
	listed  atomic.Int32  // tools/list requests that arrived
	arrived chan struct{} // one signal per tools/list request
}

func newMCPFake(t *testing.T, tools ...string) *mcpFake {
	t.Helper()
	f := &mcpFake{tools: tools, arrived: make(chan struct{}, 64)}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *mcpFake) url() string { return f.srv.URL + "/mcp" }

func (f *mcpFake) setDown(v bool) { f.mu.Lock(); f.down = v; f.mu.Unlock() }

func (f *mcpFake) setHold(c chan struct{}) { f.mu.Lock(); f.hold = c; f.mu.Unlock() }

func (f *mcpFake) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.auths)
}

func (f *mcpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "no stream", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	down, hold, tools := f.down, f.hold, slices.Clone(f.tools)
	if in.Method == "tools/list" {
		f.auths = append(f.auths, r.Header.Get("Authorization"))
	}
	f.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	reply := func(result string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, in.ID, result)
	}
	switch in.Method {
	case "initialize":
		reply(fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`, in.Params.ProtocolVersion))
	case "tools/list":
		f.listed.Add(1)
		select {
		case f.arrived <- struct{}{}:
		default:
		}
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		parts := make([]string, len(tools))
		for i, n := range tools {
			parts[i] = fmt.Sprintf(`{"name":%q,"description":"the %s tool","inputSchema":{"type":"object"}}`, n, n)
		}
		reply(`{"tools":[` + strings.Join(parts, ",") + `]}`)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// admin is the REST client of one signed-in person.
type person struct {
	s      *realServer
	cookie string
}

// do sends the request and decodes a JSON answer into any (nil for an empty body).
func (p person) do(method, path string, body any) (int, any, string) {
	p.s.t.Helper()
	status, raw, _ := call(p.s.t, method, p.s.base+path, p.cookie, body)
	var v any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			p.s.t.Fatalf("%s %s: not JSON: %s", method, path, raw)
		}
	}
	return status, v, string(raw)
}

func serverBody(slug, url string, auth map[string]any) map[string]any {
	return map[string]any{"slug": slug, "name": slug, "description": "d", "url": url, "auth": auth, "access": map[string]any{"kind": "all"}}
}

func refsOf(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		for _, tl := range e.(map[string]any)["tools"].([]any) {
			out = append(out, tl.(map[string]any)["ref"].(string))
		}
	}
	slices.Sort(out)
	return out
}

func catalogConfig(c *config.Config) {
	c.UpstreamMaxResponseBytes = 1 << 20
	c.SyncIntervalHours = 6
}

func outboxTypes(t *testing.T, url string) map[string]int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(context.Background(), `select type, count(*)::int from platform.outbox where processed_at is null group by type`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var typ string
		var n int
		if err := rows.Scan(&typ, &n); err != nil {
			t.Fatal(err)
		}
		out[typ] = n
	}
	return out
}

func TestCatalogOnTheComposedServer(t *testing.T) {
	url := pgtest.StartURL(t)
	rs := startReal(t, url, newKey(t), catalogConfig)
	admin := person{rs, rs.person("alice", idapp.RoleAdmin)}
	member := person{rs, rs.person("bob", idapp.RoleMember)}
	up := newMCPFake(t, "alpha", "beta")

	t.Run("a none server is created, synchronised and visible to members", func(t *testing.T) {
		status, v, raw := admin.do("POST", "/api/servers", serverBody("files", up.url(), map[string]any{"kind": "none"}))
		if status != 201 {
			t.Fatalf("POST /api/servers: %d %s", status, raw)
		}
		view := v.(map[string]any)
		if view["toolCount"] != float64(2) || view["lastSyncError"] != nil || view["lastSyncAt"] == nil {
			t.Fatalf("view: %s", raw)
		}
		status, v, raw = member.do("GET", "/api/catalog", nil)
		if status != 200 || !slices.Equal(refsOf(v), []string{"files.alpha", "files.beta"}) {
			t.Fatalf("GET /api/catalog as a member: %d %s", status, raw)
		}
	})

	t.Run("the events wait in the outbox, unprocessed", func(t *testing.T) {
		got := outboxTypes(t, url)
		if got["catalog.tools_changed"] != 1 || got["catalog.server_changed"] < 1 {
			t.Fatalf("outbox: %v", got)
		}
	})

	t.Run("a shared secret reaches the upstream and never comes back", func(t *testing.T) {
		const secret = "s3cr3t-value-for-the-upstream"
		body := serverBody("vault", up.url(), map[string]any{"kind": "shared_secret", "header": "Authorization", "template": "Bearer {secret}"})
		body["secret"] = secret
		status, _, raw := admin.do("POST", "/api/servers", body)
		if status != 201 || !strings.Contains(raw, `"secretSet":true`) {
			t.Fatalf("POST: %d %s", status, raw)
		}
		if got := up.authHeaders(); got[len(got)-1] != "Bearer "+secret {
			t.Fatalf("the upstream saw %q", got)
		}
		for _, path := range []string{"/api/servers", "/api/servers/vault", "/api/catalog"} {
			_, _, raw := admin.do("GET", path, nil)
			if strings.Contains(raw, secret) {
				t.Errorf("GET %s returns the secret", path)
			}
		}
		if strings.Contains(rs.logs.String(), secret) {
			t.Error("the log has the secret")
		}
	})

	t.Run("a member cannot reach the administration", func(t *testing.T) {
		for _, c := range [][2]string{{"GET", "/api/servers"}, {"POST", "/api/servers"}, {"GET", "/api/servers/files"}, {"POST", "/api/servers/files/sync"}, {"PUT", "/api/servers/files/tools/alpha/hidden"}} {
			if status, _, raw := member.do(c[0], c[1], map[string]any{}); status != 403 {
				t.Errorf("%s %s as a member: %d %s", c[0], c[1], status, raw)
			}
		}
	})

	t.Run("an sk_ token is refused on the catalog and the administration", func(t *testing.T) {
		created, err := idapp.CreateToken(context.Background(), rs.deps, domain.Principal{UserID: adminID(t, rs, "alice"), Role: domain.RoleAdmin, Groups: []string{}, Via: domain.ViaSession}, "t")
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/servers", "/api/catalog", "/api/servers/files/tools"} {
			for name, cookie := range map[string]string{"alone": "", "next to an admin session": admin.cookie} {
				status, raw, _ := call(t, "GET", rs.base+path, cookie, nil, "Authorization", "Bearer "+created.Token)
				if status != 401 || !strings.Contains(string(raw), "tokens do not work on the web API") {
					t.Errorf("GET %s with a token %s: %d %s", path, name, status, raw)
				}
			}
		}
	})

	t.Run("a hidden tool leaves the catalog and stays in the administration", func(t *testing.T) {
		if status, _, raw := admin.do("PUT", "/api/servers/files/tools/alpha/hidden", map[string]any{"hidden": true}); status != 204 {
			t.Fatalf("hide: %d %s", status, raw)
		}
		_, v, _ := member.do("GET", "/api/catalog", nil)
		if slices.Contains(refsOf(v), "files.alpha") {
			t.Fatalf("members still see the hidden tool: %v", refsOf(v))
		}
		_, v, _ = admin.do("GET", "/api/servers/files/tools", nil)
		if len(v.([]any)) != 2 {
			t.Fatalf("admin tools: %v", v)
		}
	})

	t.Run("a failed synchronisation answers 502 and leaves the tools", func(t *testing.T) {
		up.setDown(true)
		defer up.setDown(false)
		status, _, raw := admin.do("POST", "/api/servers/files/sync", nil)
		if status != 502 || !strings.Contains(raw, "sync_failed") {
			t.Fatalf("sync with the upstream down: %d %s", status, raw)
		}
		_, v, _ := admin.do("GET", "/api/servers/files/tools", nil)
		if len(v.([]any)) != 2 {
			t.Fatalf("tools after a failed sync: %v", v)
		}
		_, v, _ = admin.do("GET", "/api/servers/files", nil)
		if v.(map[string]any)["lastSyncError"] == nil {
			t.Fatal("lastSyncError not recorded")
		}
		if status, _, raw := admin.do("POST", "/api/servers/files/sync", nil); status != 502 {
			t.Fatalf("again: %d %s", status, raw)
		}
	})
	if status, _, raw := admin.do("POST", "/api/servers/files/sync", nil); status != 200 {
		t.Fatalf("sync after recovery: %d %s", status, raw)
	}

	t.Run("a personal-auth server needs a connection until milestone 5", func(t *testing.T) {
		before := up.listed.Load()
		status, v, raw := admin.do("POST", "/api/servers", serverBody("personal", up.url(), map[string]any{"kind": "pat_user", "header": "Authorization", "template": "Bearer {secret}"}))
		if status != 201 || v.(map[string]any)["lastSyncError"] != "An administrator needs to connect this server" {
			t.Fatalf("create: %d %s", status, raw)
		}
		status, _, raw = admin.do("POST", "/api/servers/personal/sync", nil)
		if status != 400 || !strings.Contains(raw, "needs_connection") {
			t.Fatalf("sync: %d %s", status, raw)
		}
		if up.listed.Load() != before {
			t.Fatal("the upstream was contacted without credentials")
		}
	})
}

func adminID(t *testing.T, rs *realServer, sub string) string {
	t.Helper()
	res, err := idapp.ProvisionUser(context.Background(), rs.deps, idapp.ProvisionInput{Sub: sub, Name: sub, Role: idapp.RoleKeep})
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

// The synchronisation job runs at start, after the listener is up and without holding it back,
// logs what the scheduler of TypeScript logs, and gives way to the shutdown.
func TestScheduledSyncAtStart(t *testing.T) {
	url := pgtest.StartURL(t)
	key := newKey(t)
	up := newMCPFake(t, "alpha")
	first := startReal(t, url, key, catalogConfig)
	admin := person{first, first.person("alice", idapp.RoleAdmin)}
	for slug, auth := range map[string]map[string]any{
		"files":    {"kind": "none"},
		"personal": {"kind": "pat_user", "header": "Authorization", "template": "Bearer {secret}"},
	} {
		if status, _, raw := admin.do("POST", "/api/servers", serverBody(slug, up.url(), auth)); status != 201 {
			t.Fatalf("create %s: %d %s", slug, status, raw)
		}
	}
	if code := first.stop("SIGTERM"); code != 0 {
		t.Fatalf("exit %d", code)
	}

	t.Run("a sync in flight does not hold the server back and the shutdown cancels it", func(t *testing.T) {
		hold := make(chan struct{})
		up.setHold(hold)
		for len(up.arrived) > 0 {
			<-up.arrived
		}
		held := startReal(t, url, key, catalogConfig) // returns once `listening` is logged
		select {
		case <-up.arrived:
		case <-time.After(waitLimit):
			t.Fatalf("the job never asked the upstream\n%s", held.logs.String())
		}
		if status, body, _ := get(t, held.base+"/api/auth/methods"); status != 200 {
			t.Fatalf("while the first sync runs: %d %s", status, body)
		}
		if status, body, _ := get(t, held.base+"/healthz"); status != 200 {
			t.Fatalf("/healthz while the first sync runs: %d %s", status, body)
		}
		if code := held.stop("SIGTERM"); code != 0 {
			t.Fatalf("exit %d\n%s", code, held.logs.String())
		}
		if n := len(held.logs.find(t, "catalog sync: done")) + len(held.logs.find(t, "catalog sync: run failed")); n != 0 {
			t.Fatalf("a cancelled run logged %d records\n%s", n, held.logs.String())
		}
		up.setHold(nil)
	})

	t.Run("the finished run is logged with its failures", func(t *testing.T) {
		before := up.listed.Load()
		again := startReal(t, url, key, catalogConfig)
		again.waitFor("catalog sync: done")
		rec := again.logs.find(t, "catalog sync: done")[0]
		failed, _ := rec["failed"].([]any)
		if rec["servers"] != float64(2) || len(failed) != 1 || levelOf(rec) != levelInfo {
			t.Fatalf("record: %v", rec)
		}
		f := failed[0].(map[string]any)
		if f["server"] != "personal" || f["code"] != "needs_connection" || len(f) != 2 {
			t.Fatalf("failed: %v", failed)
		}
		if up.listed.Load() != before+1 {
			t.Fatalf("upstream tools/list requests: %d, want 1 (the personal server is not contacted)", up.listed.Load()-before)
		}
		// The events are still in the table: nobody consumes them before milestone 4.
		if got := outboxTypes(t, url); got["catalog.tools_changed"] == 0 {
			t.Fatalf("outbox: %v", got)
		}
		if len(again.logs.find(t, "outbox worker not started: no handler is registered")) != 1 {
			t.Fatalf("the worker should report that it is off\n%s", again.logs.String())
		}
	})
}
