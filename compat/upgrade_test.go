//go:build compat

package compat_test

// The upgrade scenario: an image (COMPAT_UPGRADE_FROM) creates the state an operator has in
// production, the app container is swapped for the image under test (COMPAT_IMAGE) on the same
// database, and everything that must survive is checked without a reindex, a reconnect or a new
// token.
//
// COMPAT_UPGRADE_FROM defaults to COMPAT_IMAGE, and COMPAT_UPGRADE_FROM_CLI (the invocation prefix
// of the old image's CLI) to the CLI of the image under test (COMPAT_CLI, see harness.DefaultCLI).
// With the default (the same image on both sides) this is a RESTART test: the app is
// stopped and started again on its database. It becomes the real upgrade test in milestone 8, when
// COMPAT_UPGRADE_FROM names the TypeScript 1.x image and COMPAT_IMAGE the Go one.
//
// What the old image creates: a shared_secret server with a secret, a pat_user server with a
// member's personal token, an oauth_user server with the member's connection (so a sealed access
// and refresh token exist), a member with an sk_ token, an indexed catalog (about thirty tools),
// calls in the call log with quota counters (every server has a daily limit), audit records, and a
// pending sign-in link of another person. After the swap:
//   - the app is healthy (SwapImage waits for /healthz), logs no error and no new first-start link,
//     and leaves the key check as it was;
//   - the token works on /mcp, and call_tool delivers the right credential to each upstream (the
//     shared secret, the member's token, the stored OAuth access token), and an access token that is
//     about to expire is refreshed with the STORED refresh token;
//   - search returns the same refs with the same scores, and search.documents is byte for byte the
//     same (a digest of every row, the embedding included): nothing was reindexed;
//   - the call log, the quota counters and the audit rows of before are unchanged (a digest of every
//     row), the quota counter goes on counting;
//   - the pending link still signs in, a NEW sign-in works (the old web session is not asserted: sessions
//     may be lost); schema_migrations and the key check are unchanged;
//   - the other direction: a person, a token and a server created by the new image work.
// The app re-synchronises every server at start; the scenario waits for that to finish (and for the
// outbox to drain) before it compares the index, so a sync that rewrote documents would be seen.

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// upgradeSnapshot is what is recorded before the swap and compared after it.
type upgradeSnapshot struct {
	docs       map[string]string // search.documents: ref -> digest of the whole row (embedding included)
	calls      map[string]string // gateway.calls: id -> digest of the row
	audit      map[string]string // platform.audit_log: id -> digest of the row
	auditMax   int64
	quota      string // gateway.quota, every row, in a fixed order
	migrations []string
	keyCheck   []byte
	hits       map[string][]catalogHit
}

func upgradeDigests(t *testing.T, s *harness.Stack, query string, args ...any) map[string]string {
	t.Helper()
	rows, err := s.DB.Query(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

var upgradeQueries = []string{"create an issue", "search tickets by text", "echo the arguments", "list things", "manage work items"}

func upgradeTake(t *testing.T, s *harness.Stack, token string) upgradeSnapshot {
	t.Helper()
	snap := upgradeSnapshot{hits: map[string][]catalogHit{}}
	snap.docs = upgradeDigests(t, s, `select ref, md5(d::text) from search.documents d`)
	snap.calls = upgradeDigests(t, s, `select id::text, md5(c::text) from gateway.calls c`)
	snap.audit = upgradeDigests(t, s, `select id::text, md5(a::text) from platform.audit_log a`)
	if err := s.DB.QueryRow(t.Context(), `select coalesce(max(id), 0) from platform.audit_log`).Scan(&snap.auditMax); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(t.Context(), `select coalesce(string_agg(q::text, E'\n' order by user_id, server, day), '') from gateway.quota q`).Scan(&snap.quota); err != nil {
		t.Fatal(err)
	}
	rows, err := s.DB.Query(t.Context(), `select name from platform.schema_migrations order by name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		snap.migrations = append(snap.migrations, n)
	}
	rows.Close()
	snap.keyCheck = keyCheckBytes(t, s)
	sess := s.MCP(t, token)
	for _, q := range upgradeQueries {
		snap.hits[q] = catalogSearch(t, sess, q, 10, nil)
	}
	return snap
}

// upgradeExpectSame compares what must not change with what the old image left.
func upgradeExpectSame(t *testing.T, before, after upgradeSnapshot) {
	t.Helper()
	if len(before.docs) == 0 {
		t.Fatal("no documents were indexed before the swap: the comparison proves nothing")
	}
	if len(after.docs) != len(before.docs) {
		t.Errorf("search.documents: %d rows after, %d before", len(after.docs), len(before.docs))
	}
	for ref, digest := range before.docs {
		if after.docs[ref] != digest {
			t.Errorf("search.documents row %s differs from before the swap (the embedding or another column)", ref)
		}
	}
	for _, c := range []struct {
		what          string
		before, after map[string]string
	}{
		{"gateway.calls", before.calls, after.calls},
		{"platform.audit_log", before.audit, after.audit},
	} {
		for id, digest := range c.before {
			if c.after[id] != digest {
				t.Errorf("%s row %s is missing or changed", c.what, id)
			}
		}
	}
	if after.quota != before.quota {
		t.Errorf("gateway.quota changed:\nbefore\n%s\nafter\n%s", before.quota, after.quota)
	}
	if !slices.Equal(after.migrations, before.migrations) {
		t.Errorf("schema_migrations: %v after, %v before", after.migrations, before.migrations)
	}
	if string(after.keyCheck) != string(before.keyCheck) {
		t.Error("the key check was rewritten")
	}
	found := 0
	for _, q := range upgradeQueries {
		b, a := before.hits[q], after.hits[q]
		found += len(b)
		if len(a) != len(b) {
			t.Errorf("search %q: %d hits after, %d before", q, len(a), len(b))
			continue
		}
		for i := range b {
			if a[i].Tool != b[i].Tool || a[i].Score != b[i].Score {
				t.Errorf("search %q, hit %d: %s %v after, %s %v before", q, i, a[i].Tool, a[i].Score, b[i].Tool, b[i].Score)
			}
		}
	}
	if found < 5 {
		t.Errorf("only %d search hits in all: the comparison proves little", found)
	}
}

func TestUpgradeKeepsEverything(t *testing.T) {
	opts := harness.Options{}
	if from := os.Getenv("COMPAT_UPGRADE_FROM"); from != "" {
		opts.Image, opts.CLI = from, harness.DefaultCLI()
		if cli := os.Getenv("COMPAT_UPGRADE_FROM_CLI"); cli != "" {
			opts.CLI = cli
		}
	}
	newCLI := harness.DefaultCLI()
	s := harness.Start(t, opts)
	led := newMCPLedger(t, s)

	// ---- the old image creates the state ----
	ownerSub := harness.Unique("up-owner")
	admin := cliSignIn(t, s, ownerSub, "Owner", true)
	member := gwNewPerson(t, s, led)

	sharedSecret, memberPAT := harness.Unique("up-shared-key"), harness.Unique("up-member-pat")
	led.note(sharedSecret, memberPAT)
	sharedTools := append(fakes.Tools("echo"), fakes.FixtureTools("youtrack")...)
	shared := gwNewServer(t, led, admin, sharedTools, "shared_secret", fakes.WithSecret(sharedSecret), fakes.WithDailyLimit(100))
	personal := gwNewServer(t, led, admin, fakes.Tools("echo", "list_things"), "pat_user", fakes.WithHeader("X-Token", "pat {secret}"), fakes.WithDailyLimit(100))
	if msg := savePAT(t, member.c, personal.slug, memberPAT); msg != "" {
		t.Fatal(msg)
	}
	oauth := connNewOAuth(t, s, admin, connOpts{server: []fakes.ServerOpt{fakes.WithDailyLimit(100)}})
	issued := connConnect(t, oauth, member.c)
	led.note(issued.AccessToken, issued.RefreshToken)
	connNote(issued.AccessToken, issued.RefreshToken)
	oauth.up.Reset()

	catalogWaitDocs(t, s, shared.slug, len(sharedTools))
	catalogWaitDocs(t, s, personal.slug, 2)
	catalogWaitDocs(t, s, oauth.slug, 1)

	args := map[string]any{"text": "hi"}
	callEcho := func(what, slug string) {
		t.Helper()
		res := member.call(led, slug+".echo", args)
		if res.IsError || catalogText(res) != `{"text":"hi"}` {
			t.Fatalf("%s: call on %s: isError %v %q", what, slug, res.IsError, catalogText(res))
		}
	}
	for _, slug := range []string{shared.slug, shared.slug, personal.slug, oauth.slug} {
		callEcho("before the swap", slug)
	}
	if c := shared.up.Calls(); len(c) != 2 || c[0].Header.Get("Authorization") != "Bearer "+sharedSecret {
		t.Fatalf("before the swap the shared-secret upstream got %d call(s)", len(c))
	}

	// A pending sign-in link of another person (valid 15 minutes).
	linkSub := harness.Unique("up-link")
	cliOK(t, s, "", "create-user", "--sub", linkSub, "--name", "Link Person")
	linkCode := identityLinkFor(t, s, linkSub)
	led.note(linkCode)

	before := upgradeTake(t, s, member.token)
	if len(before.calls) < 4 || before.quota == "" || before.auditMax == 0 {
		t.Fatalf("the state to preserve is too small: %d calls, quota %q, audit max %d", len(before.calls), before.quota, before.auditMax)
	}
	var swapAt string
	if err := s.DB.QueryRow(t.Context(), `select now()::text`).Scan(&swapAt); err != nil {
		t.Fatal(err)
	}
	quotaBefore := gwQuotaCount(t, s, member.id, shared.slug)

	// ---- the swap ----
	s.SwapImage(t, platformImage(), newCLI)

	if n := logCountFrom(t, s, "error"); n != 0 {
		t.Fatalf("%d error-level or fatal records in the log after the swap", n)
	}
	if links := linkRecords(t, s.Logs(t)); len(links) != 0 {
		t.Fatalf("the swapped app announced a first-start link (%d records)", len(links))
	}

	// The app synchronises every server at start; wait for that and for the outbox before the
	// index is compared.
	harness.Eventually(t, "every server to be synchronised after the swap", func() bool {
		var n int
		err := s.DB.QueryRow(t.Context(), `select count(*) from catalog.servers where last_sync_at is null or last_sync_at < $1::timestamptz`, swapAt).Scan(&n)
		return err == nil && n == 0
	})
	catalogWaitDelivered(t, s)

	after := upgradeTake(t, s, member.token)
	upgradeExpectSame(t, before, after)
	if n := countSQL(t, s, `select count(*) from search.documents where embedding is null`); n != 0 {
		t.Errorf("%d documents without an embedding after the swap", n)
	}

	t.Run("the token works and the person sees their connections", func(t *testing.T) {
		sess := led.session(member.token)
		if names := cliToolNames(t, sess); !slices.Equal(names, cliMetaTools) {
			t.Fatalf("tools of the old token's session: %v", names)
		}
		listed := catalogListServers(t, sess)
		for slug, want := range map[string]string{shared.slug: "not_required", personal.slug: "ready", oauth.slug: "ready"} {
			i := slices.IndexFunc(listed, func(l catalogListed) bool { return l.Slug == slug })
			if i < 0 || listed[i].Connection != want {
				t.Errorf("list_servers: %s is %+v, want connection %s", slug, listed, want)
			}
		}
		// A web session of before the swap may or may not survive (the compatibility requirements
		// allow it to be lost, so it is not asserted); what must work is a NEW sign-in of the same person.
		var sub string
		if err := s.DB.QueryRow(t.Context(), `select oidc_sub from identity.users where id = $1::uuid`, member.id).Scan(&sub); err != nil {
			t.Fatal(err)
		}
		code := identityLinkFor(t, s, sub)
		led.note(code)
		fresh := s.Anonymous()
		if r := identityPostLink(t, fresh, code); r.Status != 204 {
			t.Fatalf("a new sign-in after the swap: %d %s", r.Status, r.Body)
		}
		if me := whoami(t, fresh); me.UserID != member.id {
			t.Errorf("the new session after the swap: %+v", me)
		}
		// Control: a token that never existed is refused.
		if r := identityMCPInitialize(t, s.Anonymous(), "sk_"+strings.Repeat("A", 43)); r.Status != 401 {
			t.Errorf("an unknown token: %d", r.Status)
		}
	})

	t.Run("call_tool delivers the stored credentials: the shared secret, the personal token, the OAuth access token", func(t *testing.T) {
		shared.up.Reset()
		personal.up.Reset()
		oauth.up.Reset()
		for _, slug := range []string{shared.slug, personal.slug, oauth.slug} {
			callEcho("after the swap", slug)
		}
		if c := shared.up.Calls(); len(c) != 1 || c[0].Header.Get("Authorization") != "Bearer "+sharedSecret {
			t.Errorf("the shared-secret upstream got %d call(s) with the wrong credential", len(c))
		}
		if c := personal.up.Calls(); len(c) != 1 || c[0].Header.Get("X-Token") != "pat "+memberPAT {
			t.Errorf("the personal-token upstream got %d call(s) with the wrong credential", len(c))
		}
		if c := oauth.up.Calls(); len(c) != 1 || c[0].Header.Get("Authorization") != "Bearer "+issued.AccessToken {
			t.Errorf("the OAuth upstream got %d call(s) with the wrong credential", len(c))
		}
		if got := gwQuotaCount(t, s, member.id, shared.slug); got != quotaBefore+1 {
			t.Errorf("the quota counter of the shared server: %d, want %d (it goes on counting)", got, quotaBefore+1)
		}
	})

	t.Run("an access token about to expire is refreshed with the stored refresh token", func(t *testing.T) {
		requests := len(oauth.provider.Requests())
		connExpireIn(t, s, member.id, oauth.slug, 10)
		oauth.up.Reset()
		callEcho("refresh after the swap", oauth.slug)
		var refreshes []string
		for _, r := range oauth.provider.Requests()[requests:] {
			if r.GrantType == "refresh_token" {
				refreshes = append(refreshes, r.Form.Get("refresh_token"))
			}
		}
		if len(refreshes) != 1 || refreshes[0] != issued.RefreshToken {
			t.Fatalf("refresh requests after the swap: %d, presenting the stored refresh token: %v", len(refreshes), len(refreshes) == 1 && refreshes[0] == issued.RefreshToken)
		}
		c := oauth.up.Calls()
		if len(c) != 1 || c[0].Header.Get("Authorization") == "Bearer "+issued.AccessToken || !strings.HasPrefix(c[0].Header.Get("Authorization"), "Bearer access-") {
			t.Fatalf("the upstream after the refresh: %d call(s)", len(c))
		}
		connNote(strings.TrimPrefix(c[0].Header.Get("Authorization"), "Bearer "))
		if row := connRowOf(t, s, member.id, oauth.slug); row.Status != "ready" {
			t.Fatalf("connection after the refresh: %s", row.Status)
		}
	})

	t.Run("the pending sign-in link still signs in, once", func(t *testing.T) {
		c := s.Anonymous()
		if r := identityPostLink(t, c, linkCode); r.Status != 204 {
			t.Fatalf("the link of before the swap: %d %s", r.Status, r.Body)
		}
		if me := whoami(t, c); me.Name != "Link Person" {
			t.Fatalf("me: %+v", me)
		}
		if r := identityPostLink(t, s.Anonymous(), linkCode); r.Status != 401 {
			t.Fatalf("the link used twice: %d", r.Status)
		}
	})

	t.Run("what the new image creates works too: a person, a token, a server", func(t *testing.T) {
		newSub := harness.Unique("up-new")
		cliOK(t, s, "", "create-user", "--sub", newSub, "--name", "New Person")
		out := cliOK(t, s, "", "create-token", newSub, "--name", "after")
		token := strings.SplitN(out.Stdout, "\n", 2)[0]
		led.note(token)
		sess := led.session(token)
		if names := cliToolNames(t, sess); !slices.Equal(names, cliMetaTools) {
			t.Fatalf("tools of the new token: %v", names)
		}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		slug := harness.Unique("up-after")
		secret := harness.Unique("up-after-key")
		led.note(secret)
		file := cliServerFile(t, s, up, slug, "shared_secret")
		created := cliOK(t, s, secret+"\n", "create-server", "--json", file, "--as", ownerSub)
		if !strings.HasPrefix(created.Stdout, "created server \""+slug+"\" (shared_secret, secret set); 1 tool(s)") {
			t.Fatalf("create-server: %q", created.Stdout)
		}
		catalogWaitDocs(t, s, slug, 1)
		if hits := catalogHitRefs(catalogSearch(t, sess, "Test tool echo", 10, []string{slug})); !slices.Contains(hits, slug+".echo") {
			t.Fatalf("search finds %v", hits)
		}
		res := led.call(sess, "call_tool", map[string]any{"tool": slug + ".echo", "arguments": args})
		if res.IsError || catalogText(res) != `{"text":"hi"}` {
			t.Fatalf("call on the new server: %q", catalogText(res))
		}
		if c := up.Calls(); len(c) != 1 || c[0].Header.Get("Authorization") != "Bearer "+secret {
			t.Fatalf("the new server's upstream got %d call(s) with the wrong credential", len(c))
		}
		// The old and the new rows live side by side: the audit trail grew, it was not rewritten.
		if n := countSQL(t, s, `select count(*) from platform.audit_log where id <= $1`, before.auditMax); n != len(before.audit) {
			t.Errorf("audit rows up to the old maximum: %d, was %d", n, len(before.audit))
		}
	})
}
