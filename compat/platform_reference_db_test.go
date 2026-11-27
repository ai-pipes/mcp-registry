//go:build compat

package compat_test

// A scenario of the platform that needs no person, no session, no token and no CLI, so it runs
// against an image that has only the platform layer.
//
// TestPlatformStartsOnReferenceDatabase: the database is created by the REFERENCE image (the
// TypeScript one, harness.ReferenceImage), then the app container is swapped for the image under
// test (COMPAT_IMAGE) with the same key. That is the hand-over an operator makes when changing
// images in place. The new image must:
//   - start healthy, apply no migration (the ten names in platform.schema_migrations are the same
//     and no row is new), leave the key check byte for byte as it was, log nothing at error level
//     or above, and serve the web shell on GET /;
//   - refuse another key: exit 1, one fatal record (the operator-facing key-mismatch text, which
//     names the README section), no key material in the log, the key check unchanged;
//   - refuse a database with a migration it does not know: exit 1, one fatal record that names that
//     migration, nothing applied; and start again once the row is gone.
//   - hand over the PEOPLE too: an admin and a member created and signed in by the reference image,
//     and an API token the admin created through the web API, are still there: both session
//     cookies work (GET /api/auth/me names the same person), the token list shows the token with
//     the same id and prefix, and a new sign-in by a link of the existing admin works.
//   - hand over a CATALOG SERVER too: a shared_secret server with a secret, three tools (one
//     hidden), created through the reference image's REST API, reads the same through the image
//     under test, and a sync (the upstream accepts only the stored secret) works and keeps the
//     hidden flag.
//   - hand over CONNECTIONS too: an oauth_user server whose administrator and member connected
//     through the fake provider and a pat_user server with a personal token of each (no /mcp
//     needed): the connections are still ready (the stored configuration fingerprint compares
//     equal), their rows are untouched, a sync sends the stored tokens, an access token about to
//     expire is refreshed once with the STORED refresh token, and a disconnect revokes at the
//     revoke URL stored at issue time.
//   - read the UNUSUAL connection rows the reference image left (handover_edges_test.go): an expired
//     row that still holds a refresh token, a member's expiring token, a connect started and not
//     finished (and one whose window ran out), a token without a refresh token on a server that had
//     no revoke URL at issue time, tokens without an expiry.
// When the image under test IS the reference image (the default) this is a restart test.
//
// TestPlatformReverseHandOver is the other direction: the database is created by the image under
// test, then the app container is swapped for the REFERENCE image with the same key. What the new
// image wrote (the migrations, the sealed key check) must be accepted by 1.0.x as it is: healthy,
// nothing applied, the key check untouched, the web shell served, and the people, sessions and
// token that the image under test created are accepted the same way as in the forward direction.
// This is the rollback an operator makes after trying the new image. The catalog server created
// by the image under test (its sealed secret, its tools) is read and synchronised by 1.0.x the
// same way, and so are the unusual connection rows (among them the Go server's NULL expires_at of a
// token whose expires_in lies beyond 100 years). Last, the reference image's CLI rotates the key
// the image under test sealed with, and 1.0.x runs on the new key alone with every credential.
//
// The token's use on /mcp is not checked here: the Go server has no /mcp before milestone 6 (the
// scenarios that use a token on /mcp are skipped for it until then), so the hand-over proves the
// token row only (id, prefix, not revoked), not that the other image still accepts the value.

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func migrationNames(t *testing.T, s *harness.Stack) []string {
	t.Helper()
	rows, err := s.DB.Query(t.Context(), `select name from platform.schema_migrations order by name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// handOverPeople is what one image leaves behind for the next: two signed-in people and a token.
type handOverPeople struct {
	admin, member       *harness.Client
	adminSub, memberSub string
	adminID, memberID   string
	tokenID, tokenValue string
	tokenPrefix         string
}

type handOverToken struct {
	ID, Name, Prefix string
	LastUsedAt       *string
	RevokedAt        *string
}

func handOverTokens(t *testing.T, c *harness.Client) []handOverToken {
	t.Helper()
	var out []handOverToken
	c.Do(t, "GET", "/api/tokens", nil).JSON(t, "TokenSummary[]", &out)
	return out
}

// createHandOverPeople makes an admin and a member, signs both in with a link and lets the admin
// create a token, all through the image the stack runs now.
func createHandOverPeople(t *testing.T, s *harness.Stack) *handOverPeople {
	t.Helper()
	p := &handOverPeople{adminSub: harness.Unique("hand-admin"), memberSub: harness.Unique("hand-member")}
	for _, c := range []struct {
		sub, name string
		extra     []string
		client    **harness.Client
	}{
		{p.adminSub, "Hand Admin", []string{"--admin"}, &p.admin},
		{p.memberSub, "Hand Member", nil, &p.member},
	} {
		args := append([]string{"create-user", "--sub", c.sub, "--name", c.name}, c.extra...)
		if r := s.CLI(t, "", args...); r.Code != 0 {
			t.Fatalf("create-user %s: %d %s", c.sub, r.Code, r.Stderr)
		}
		client := s.Anonymous()
		if r := identityPostLink(t, client, identityLinkFor(t, s, c.sub)); r.Status != 204 {
			t.Fatalf("sign in %s: %d %s", c.sub, r.Status, r.Body)
		}
		*c.client = client
	}
	p.adminID, p.memberID = whoami(t, p.admin).UserID, whoami(t, p.member).UserID
	p.tokenValue = p.admin.Token(t, "hand-over")
	connNote(p.tokenValue)
	list := handOverTokens(t, p.admin)
	if len(list) != 1 || list[0].Name != "hand-over" || !strings.HasPrefix(p.tokenValue, list[0].Prefix) {
		t.Fatalf("the token list after creating the token: %+v", list)
	}
	p.tokenID, p.tokenPrefix = list[0].ID, list[0].Prefix
	return p
}

// checkHandOverPeople asserts, against the image the stack runs now, that what the other image
// created is accepted: the sessions, the token row, and a new sign-in.
func checkHandOverPeople(t *testing.T, s *harness.Stack, p *handOverPeople) {
	t.Helper()
	if me := whoami(t, p.admin); me.UserID != p.adminID || me.Role != "admin" || me.Name != "Hand Admin" {
		t.Fatalf("the admin's old session: %+v, want %s as admin", me, p.adminID)
	}
	if me := whoami(t, p.member); me.UserID != p.memberID || me.Role != "member" || me.Name != "Hand Member" {
		t.Fatalf("the member's old session: %+v, want %s as member", me, p.memberID)
	}
	// The sessions are real: the admin may list people, the member may not.
	if r := p.admin.Do(t, "GET", "/api/users", nil); r.Status != 200 {
		t.Fatalf("the admin's old session on /api/users: %d %s", r.Status, r.Body)
	}
	if r := p.member.Do(t, "GET", "/api/users", nil); r.Status != 403 {
		t.Fatalf("the member's old session on /api/users: %d, want 403", r.Status)
	}
	list := handOverTokens(t, p.admin)
	if len(list) != 1 || list[0].ID != p.tokenID || list[0].Prefix != p.tokenPrefix || list[0].Name != "hand-over" || list[0].RevokedAt != nil {
		t.Fatalf("the token list: %+v, want id %s prefix %s, not revoked", list, p.tokenID, p.tokenPrefix)
	}
	if got := handOverTokens(t, p.member); len(got) != 0 {
		t.Fatalf("the member sees %d token(s), want none", len(got))
	}
	// A new sign-in of the existing admin works and names the same person.
	fresh := s.Anonymous()
	if r := identityPostLink(t, fresh, identityLinkFor(t, s, p.adminSub)); r.Status != 204 {
		t.Fatalf("a new sign-in: %d %s", r.Status, r.Body)
	}
	if me := whoami(t, fresh); me.UserID != p.adminID {
		t.Fatalf("a new sign-in names %+v, want %s", me, p.adminID)
	}
}

// handOverServer is a catalog server one image leaves behind for the next: a shared_secret server
// whose upstream refuses any other secret, with three tools, one of them hidden.
type handOverServer struct {
	slug  string
	up    *fakes.Upstream
	view  catalogView
	tools []catalogAdminTool
}

const handOverHeaderValue = "key hand-over-secret"

// createHandOverServer registers the server through the REST API of the image the stack runs now
// (the first sync lists the tools with the secret), then hides one tool.
func createHandOverServer(t *testing.T, p *handOverPeople) *handOverServer {
	t.Helper()
	h := &handOverServer{slug: harness.Unique("hand-srv")}
	h.up = fakes.StartUpstream(t, fakes.UpstreamOptions{
		Tools:         fakes.Tools("list_repos", "delete_repo", "get_issue"),
		RequireHeader: [2]string{"X-API-Key", handOverHeaderValue},
	})
	v := catalogRegister(t, p.admin, fakes.ServerInput(h.up, h.slug, "shared_secret",
		fakes.WithHeader("X-API-Key", "key {secret}"), fakes.WithSecret("hand-over-secret")))
	if v.ToolCount != 3 || v.LastSyncError != nil || !v.SecretSet {
		t.Fatalf("the server created for the hand-over: toolCount %d, lastSyncError %q, secretSet %v", v.ToolCount, v.syncError(), v.SecretSet)
	}
	catalogHide(t, p.admin, h.slug, "delete_repo", true)
	h.view = catalogGet(t, p.admin, h.slug)
	_, h.tools = catalogTools(t, p.admin, h.slug)
	if h.view.ToolCount != 2 || len(h.tools) != 3 {
		t.Fatalf("the server after hiding a tool: toolCount %d, %d tools", h.view.ToolCount, len(h.tools))
	}
	return h
}

// checkHandOverServer asserts, against the image the stack runs now, that the server the other
// image created is read as it was (view, tools with their hidden flag, the catalog a member sees)
// and that it can be synchronised: the upstream accepts only the stored secret, so a sync that
// succeeds proves this image opens the secret the other one sealed. The hidden flag survives it.
func checkHandOverServer(t *testing.T, p *handOverPeople, h *handOverServer) {
	t.Helper()
	got := catalogGet(t, p.admin, h.slug)
	// The image that starts synchronises every server at once (the scheduler), so the time moves
	// forward; the rest of the view is what the other image left.
	if got.LastSyncAt == nil || got.syncedAt(t).Before(h.view.syncedAt(t)) {
		t.Fatalf("lastSyncAt: before %v, now %v", h.view.LastSyncAt, got.LastSyncAt)
	}
	got.LastSyncAt, h.view.LastSyncAt = nil, nil
	if !reflect.DeepEqual(got, h.view) {
		t.Fatalf("the server as the other image left it: %+v, now %+v", h.view, got)
	}
	if _, got := catalogTools(t, p.admin, h.slug); !reflect.DeepEqual(got, h.tools) {
		t.Fatalf("the tools as the other image left them: %+v, now %+v", h.tools, got)
	}
	var seen []string
	var entries []catalogEntry
	p.member.Do(t, "GET", "/api/catalog", nil).JSON(t, "CatalogServer[]", &entries)
	for _, e := range entries {
		if e.Slug != h.slug {
			continue
		}
		for _, tl := range e.Tools {
			seen = append(seen, tl.Ref)
		}
	}
	slices.Sort(seen)
	if want := []string{h.slug + ".get_issue", h.slug + ".list_repos"}; !slices.Equal(seen, want) {
		t.Fatalf("the catalog a member sees lists %v for the server, want %v (the hidden tool stays out)", seen, want)
	}

	// The upstream changes: one tool goes, one comes. The sync reads the sealed secret.
	h.up.SetTools(fakes.Tools("list_repos", "delete_repo", "create_issue"))
	before := len(h.up.Requests())
	if res := catalogSyncOK(t, p.admin, h.slug); res != (catalogSyncResult{Added: 1, Changed: 0, Removed: 1}) {
		t.Fatalf("sync after the hand-over: %+v", res)
	}
	requests := h.up.Requests()[before:]
	if len(requests) == 0 {
		t.Fatal("the sync did not reach the upstream")
	}
	for _, req := range requests {
		if got := req.Header.Get("X-API-Key"); got != handOverHeaderValue {
			t.Fatalf("a %q request carried X-API-Key %q, want the stored secret", req.Method, got)
		}
	}
	_, tools := catalogTools(t, p.admin, h.slug)
	hidden := map[string]bool{}
	for _, tl := range tools {
		hidden[tl.Name] = tl.Hidden
	}
	if want := map[string]bool{"list_repos": false, "delete_repo": true, "create_issue": false}; !reflect.DeepEqual(hidden, want) {
		t.Fatalf("the tools after the sync (name: hidden): %v, want %v", hidden, want)
	}
	if v := catalogGet(t, p.admin, h.slug); v.LastSyncError != nil || !v.SecretSet || v.ToolCount != 2 {
		t.Fatalf("the server after the sync: lastSyncError %q, secretSet %v, toolCount %d", v.syncError(), v.SecretSet, v.ToolCount)
	}
}

// handOverConnections are the connections one image leaves behind for the next: an oauth_user
// server (the administrator and the member are connected through the fake provider, the tools were
// synced with the administrator's token) and a pat_user server (both have a personal token). They
// need no /mcp: REST, SQL and the fake provider judge them. The check below is the cross-image
// proof that a token is never asked for again: the stored configuration fingerprint (issued_for)
// of one image compares equal in the other, the sealed tokens open, an expiring access token is
// refreshed with the STORED refresh token, and the revoke URL stored at issue time is used.
type handOverConnections struct {
	oauth                  *connEnv
	patSlug                string
	patUp                  *fakes.Upstream
	adminTokens, memberTok fakes.IssuedTokens
	rows                   map[string]connRow // "who/slug"
}

const (
	handOverAdminPAT  = "hand-over-admin-pat"
	handOverMemberPAT = "hand-over-member-pat"
)

func createHandOverConnections(t *testing.T, s *harness.Stack, p *handOverPeople) *handOverConnections {
	t.Helper()
	h := &handOverConnections{rows: map[string]connRow{}}
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	provider := connWatch(fakes.StartOAuthProvider(t)) // its tokens are looked for by connLeakCheck
	slug := harness.Unique("hand-oauth")
	connNote(handOverAdminPAT, handOverMemberPAT, "hand-over-secret")
	catalogRegister(t, p.admin, fakes.ServerInput(up, slug, "oauth_user",
		fakes.WithOAuth(provider, "repo"),
		fakes.WithExtraAuthorizeParams(map[string]string{"access_type": "offline"}),
		fakes.WithTimeoutMs(5000)))
	h.oauth = &connEnv{s: s, admin: p.admin, slug: slug, ref: slug + ".echo", up: up, provider: provider}
	h.adminTokens = connConnect(t, h.oauth, p.admin)
	h.memberTok = connConnect(t, h.oauth, p.member)
	if r := catalogSyncOK(t, p.admin, slug); r.Added != 1 {
		t.Fatalf("sync with the administrator's token: %+v", r)
	}

	h.patUp = fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	h.patSlug = harness.Unique("hand-pat")
	catalogRegister(t, p.admin, fakes.ServerInput(h.patUp, h.patSlug, "pat_user",
		fakes.WithHeader("X-Token", "pat {secret}"), fakes.WithTimeoutMs(5000)))
	for _, c := range []struct {
		who    *harness.Client
		secret string
	}{{p.admin, handOverAdminPAT}, {p.member, handOverMemberPAT}} {
		if r := c.who.Do(t, "PUT", "/api/connections/"+h.patSlug+"/pat", map[string]string{"token": c.secret}); r.Status != 204 {
			t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
		}
	}
	if r := catalogSyncOK(t, p.admin, h.patSlug); r.Added != 1 {
		t.Fatalf("sync with the administrator's personal token: %+v", r)
	}
	for who, id := range map[string]string{"admin": p.adminID, "member": p.memberID} {
		for _, sl := range []string{slug, h.patSlug} {
			h.rows[who+"/"+sl] = connRowOf(t, s, id, sl)
		}
	}
	return h
}

// checkHandOverConnections asserts, against the image the stack runs now, that the connections
// the other image stored are usable without a reconnect.
func checkHandOverConnections(t *testing.T, s *harness.Stack, p *handOverPeople, h *handOverConnections) {
	t.Helper()
	slug, provider, up := h.oauth.slug, h.oauth.provider, h.oauth.up
	for _, who := range []struct {
		name string
		c    *harness.Client
		id   string
	}{{"admin", p.admin, p.adminID}, {"member", p.member, p.memberID}} {
		for _, sl := range []string{slug, h.patSlug} {
			if got := connStatus(t, who.c, sl); got != "ready" {
				t.Fatalf("%s's connection to %s is %q after the hand-over, want ready (the configuration fingerprint must compare equal)", who.name, sl, got)
			}
			// Nothing in the connection row changes by changing the image: tokens, expiry,
			// fingerprint, revoke URL and updated_at are the bytes the other image wrote.
			connExpectSameRow(t, who.name+"'s connection to "+sl, h.rows[who.name+"/"+sl], connRowOf(t, s, who.id, sl))
		}
	}

	// A sync reads the administrator's sealed access token and sends it.
	up.Reset()
	if r := catalogSyncOK(t, p.admin, slug); r.Added+r.Changed+r.Removed != 0 {
		t.Fatalf("sync with the stored access token: %+v", r)
	}
	connExpectEveryRequestWith(t, "sync with the stored access token", up, "Authorization", "Bearer "+h.adminTokens.AccessToken)
	h.patUp.Reset()
	catalogSyncOK(t, p.admin, h.patSlug)
	connExpectEveryRequestWith(t, "sync with the stored personal token", h.patUp, "X-Token", "pat "+handOverAdminPAT)

	// The access token is about to expire: one refresh, with the stored refresh token and the
	// client's credentials, and the new token goes to the upstream.
	connExpireIn(t, s, p.adminID, slug, 10)
	before := len(provider.Requests())
	up.Reset()
	catalogSyncOK(t, p.admin, slug)
	reqs := provider.Requests()[before:]
	if len(reqs) != 1 || reqs[0].GrantType != "refresh_token" || reqs[0].Form.Get("refresh_token") != h.adminTokens.RefreshToken || reqs[0].Authorization != connBasic(provider) {
		t.Fatalf("token requests after the expiry: %d, want exactly one refresh_token grant with the stored refresh token and the client's Basic header", len(reqs))
	}
	if reqs[0].AccessToken == "" || reqs[0].AccessToken == h.adminTokens.AccessToken {
		t.Fatalf("the refresh did not hand out a new access token")
	}
	connExpectEveryRequestWith(t, "sync after the refresh", up, "Authorization", "Bearer "+reqs[0].AccessToken)
	connExpectFreshExpiry(t, s, p.adminID, slug)

	// Disconnect revokes at the revoke URL stored when the token was issued.
	if r := p.member.Do(t, "DELETE", "/api/connections/"+slug, nil); r.Status != 204 {
		t.Fatalf("DELETE member's connection: %d %s", r.Status, r.Body)
	}
	connEqual(t, "revoked at the stored revoke URL", provider.Revoked(), []string{h.memberTok.RefreshToken})
	if _, ok := connGetRow(t, s, p.memberID, slug); ok {
		t.Fatal("the member's connection row survived the disconnect")
	}
}

func TestPlatformStartsOnReferenceDatabase(t *testing.T) {
	harness.RequireReferenceImage(t)
	key := newKey(t)
	s := harness.Start(t, harness.Options{
		Image: harness.ReferenceImage(),
		CLI:   harness.ReferenceCLI(),
		Env:   map[string]string{"SECRETS_KEY": key},
	})
	image, cli := platformImage(), harness.DefaultCLI()

	migrations := migrationNames(t, s)
	if len(migrations) == 0 || migrations[0] != "0001_platform.sql" {
		t.Fatalf("the reference image recorded the migrations %v", migrations)
	}
	check := keyCheckBytes(t, s)
	people := createHandOverPeople(t, s)
	server := createHandOverServer(t, people)
	conns := createHandOverConnections(t, s, people)
	edges := createHandOverEdges(t, s, people.admin)
	swapped := false
	// The tokens, personal tokens and secrets of this stack must appear in no app log (the current
	// container's and every replaced one's) and no audit record.
	kept := &handOverKeptLogs{}
	connLeakCheck(t, s, people.admin)
	kept.scanAtEnd(t, connNoted)

	t.Run("the same key: healthy, nothing applied, the check untouched, the shell served, the reference image's people accepted", func(t *testing.T) {
		kept.keep(t, s)
		s.SwapImage(t, image, cli)
		swapped = true
		if got := migrationNames(t, s); !slices.Equal(got, migrations) {
			t.Fatalf("migrations before %v, after %v", migrations, got)
		}
		if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 || string(keyCheckBytes(t, s)) != string(check) {
			t.Fatalf("the key check changed (%d rows)", n)
		}
		recs, other := logRecords(t, s.Logs(t))
		if len(other) != 0 {
			t.Errorf("log lines that are not JSON: %q", other)
		}
		if bad := recordsFrom(recs, "error"); len(bad) != 0 {
			t.Fatalf("records at error level or above on a healthy start: %v", bad)
		}
		if n := len(recordsWithMsg(recs, keyCreatedMsg)); n != 0 {
			t.Fatalf("the key check was recorded again: %d records", n)
		}
		r := s.Anonymous().Do(t, "GET", "/", nil, "Accept: text/html")
		if r.Status != 200 || !strings.Contains(string(r.Body), `<div id="root">`) {
			t.Fatalf("GET /: %d %.200s", r.Status, r.Body)
		}
		// Only after the swap just above: the sessions and the token were created while the reference
		// image ran, so accepting them here is what proves the image under test reads what the
		// reference wrote. (The check lives in this subtest so that it cannot run against the
		// reference image by a reordering or a -run filter.)
		checkHandOverPeople(t, s, people)
		checkHandOverServer(t, people, server)
		checkHandOverConnections(t, s, people, conns)
	})

	t.Run("the unusual rows the reference image left: expired, pending, without a refresh token, revoke URL or expiry", func(t *testing.T) {
		if !swapped {
			t.Fatal("the image under test was not started on the reference image's database: run the scenario as a whole")
		}
		checkHandOverEdges(t, s, edges)
	})

	t.Run("another key: exit 1, one fatal record, no key in the log, the check unchanged", func(t *testing.T) {
		wrong := newKey(t)
		kept.keep(t, s)
		code, logs := s.SwapImageExpectingExit(t, image, cli, map[string]string{"SECRETS_KEY": wrong})
		kept.add(logs)
		if code != 1 {
			t.Fatalf("exit code %d, logs: %s", code, logs)
		}
		recs, _ := logRecords(t, logs)
		fatal := recordsAt(recs, "fatal")
		if len(fatal) != 1 || fatal[0]["msg"] != keyMismatchMsg {
			t.Fatalf("fatal records: %v", fatal)
		}
		assertNoKey(t, logs, key, wrong)
		if string(keyCheckBytes(t, s)) != string(check) {
			t.Fatal("a refused start changed the key check")
		}
	})

	t.Run("a migration the image does not know: exit 1, one fatal record naming it, nothing applied", func(t *testing.T) {
		execSQL(t, s, `insert into platform.schema_migrations (name) values ($1)`, newerDBMigrName)
		code, logs := s.SwapImageExpectingExit(t, image, cli, nil)
		kept.add(logs)
		if code != 1 {
			t.Fatalf("exit code %d, logs: %s", code, logs)
		}
		recs, _ := logRecords(t, logs)
		fatal := recordsAt(recs, "fatal")
		if len(fatal) != 1 {
			t.Fatalf("fatal records: %v", fatal)
		}
		// The two fragments an operator acts on, as in TestPlatformMigrationsAndKeyCheck.
		msg, _ := fatal[0]["msg"].(string)
		if !strings.Contains(msg, "migrated by a newer version of mcp-registry (unknown migration: "+newerDBMigrName+")") ||
			!strings.Contains(msg, "restore the backup taken before the upgrade") {
			t.Fatalf("fatal message: %q", msg)
		}
		if got := migrationNames(t, s); len(got) != len(migrations)+1 {
			t.Fatalf("migrations after the refusal: %v", got)
		}
		assertNoKey(t, logs, key)
		execSQL(t, s, `delete from platform.schema_migrations where name = $1`, newerDBMigrName)
		s.SwapImage(t, image, cli) // the lock was released and the key is the right one: a normal start
		if got := migrationNames(t, s); !slices.Equal(got, migrations) {
			t.Fatalf("migrations %v after the row was removed", got)
		}
	})
}

func TestPlatformReverseHandOver(t *testing.T) {
	harness.RequireReferenceImage(t)
	key := newKey(t)
	s := harness.Start(t, harness.Options{
		Image: platformImage(),
		CLI:   harness.DefaultCLI(),
		Env:   map[string]string{"SECRETS_KEY": key},
	})
	migrations := migrationNames(t, s)
	if len(migrations) == 0 || migrations[0] != "0001_platform.sql" {
		t.Fatalf("the image under test recorded the migrations %v", migrations)
	}
	if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 {
		t.Fatalf("the image under test recorded %d key checks, want 1", n)
	}
	check := keyCheckBytes(t, s)
	people := createHandOverPeople(t, s)
	server := createHandOverServer(t, people)
	conns := createHandOverConnections(t, s, people)
	edges := createHandOverEdges(t, s, people.admin)
	rotation := createHandOverRotation(t, s, people.admin)
	// The tokens, personal tokens and secrets of this stack must appear in no app log (the current
	// container's and every replaced one's) and no audit record.
	kept := &handOverKeptLogs{}
	connLeakCheck(t, s, people.admin)
	kept.scanAtEnd(t, connNoted)

	kept.keep(t, s)
	s.SwapImage(t, harness.ReferenceImage(), harness.ReferenceCLI())

	if got := migrationNames(t, s); !slices.Equal(got, migrations) {
		t.Fatalf("migrations before %v, after the reference image started: %v", migrations, got)
	}
	if n := countSQL(t, s, "select count(*) from platform.key_check"); n != 1 || string(keyCheckBytes(t, s)) != string(check) {
		t.Fatalf("the key check changed under the reference image (%d rows)", n)
	}
	recs, other := logRecords(t, s.Logs(t))
	if len(other) != 0 {
		t.Errorf("log lines that are not JSON: %q", other)
	}
	if bad := recordsFrom(recs, "error"); len(bad) != 0 {
		t.Fatalf("records at error level or above in the reference image's start: %v", bad)
	}
	if n := len(recordsWithMsg(recs, keyCreatedMsg)); n != 0 {
		t.Fatalf("the reference image recorded the key check again: %d records", n)
	}
	assertNoKey(t, s.Logs(t), key)
	r := s.Anonymous().Do(t, "GET", "/", nil, "Accept: text/html")
	if r.Status != 200 || !strings.Contains(string(r.Body), `<div id="root">`) {
		t.Fatalf("GET /: %d %.200s", r.Status, r.Body)
	}
	checkHandOverPeople(t, s, people)
	checkHandOverServer(t, people, server)
	checkHandOverConnections(t, s, people, conns)
	t.Run("the unusual rows the image under test left", func(t *testing.T) { checkHandOverEdges(t, s, edges) })
	// Last: the stack's own key stays the old one, so nothing may swap images after this subtest.
	t.Run("the reference image's CLI rotates the key the image under test sealed with; the reference then runs on the new key alone", func(t *testing.T) {
		checkHandOverRotation(t, s, rotation, harness.ReferenceImage(), harness.ReferenceCLI(), key, kept)
	})
}
