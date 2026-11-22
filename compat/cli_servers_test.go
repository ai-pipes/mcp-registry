//go:build compat

package compat_test

// create-server, sync and reindex scenarios; see the header of cli_users_test.go for the conventions.

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestCLIServers(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	ownerSub := harness.Unique("cli-owner")
	memberSub := harness.Unique("cli-member")
	owner := cliSignIn(t, s, ownerSub, "Owner", true)
	cliOK(t, s, "", "create-user", "--sub", memberSub, "--name", "Member")
	ownerID := cliMustUser(t, s, ownerSub).ID

	t.Run("create-server reads the secret from stdin, syncs, and never prints it", func(t *testing.T) {
		secret := harness.Unique("sekret-stdin")
		slug := harness.Unique("cli-stdin")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
		file := cliServerFile(t, s, up, slug, "shared_secret")
		r := s.CLI(t, secret+"\n", "create-server", "--json", file, "--as", ownerSub)
		cliGolden(t, s, "create-server-stdin", r, 0, slug, "<SLUG>")
		if want := fmt.Sprintf("created server %q (shared_secret, secret set); 1 tool(s)\n", slug); r.Stdout != want || r.Stderr != "" {
			t.Fatalf("output %q stderr %q, want %q", r.Stdout, r.Stderr, want)
		}
		v := catalogGet(t, owner, slug)
		if !v.SecretSet || v.ToolCount != 1 || v.LastSyncError != nil || v.Status != "active" {
			t.Fatalf("server after create-server: %+v", v)
		}
		reqs := up.Requests()
		if len(reqs) == 0 {
			t.Fatal("the upstream was not contacted: the creation did not sync")
		}
		for _, q := range reqs {
			if got := q.Header.Get("Authorization"); got != "Bearer "+secret {
				t.Errorf("an upstream request carried %q, want the secret from stdin without its newline", harness.Redact(got))
			}
		}
		cliExpectNoSecret(t, s, secret, r)
		var actions []string
		for _, e := range auditRows(t, s, "where target = $1 order by id", slug) {
			actions = append(actions, e.Action)
			if !ptrIs(e.ActorID, ownerID) {
				t.Errorf("audit %s: actor %v, want the --as person %s", e.Action, e.ActorID, ownerID)
			}
		}
		if !slices.Equal(actions, []string{"server.create", "server.secret_set", "server.sync"}) {
			t.Fatalf("audit actions of the server: %v", actions)
		}
		syncs := cliSyncAudit(t, s, slug)
		cliMetaIs(t, syncs[0], `{"added":1,"changed":0,"removed":0}`)

		// Control: a wrong secret is refused by the same upstream, so the success above proves that
		// the secret from stdin is the one that reached it.
		wrongSlug := harness.Unique("cli-wrong")
		wrongFile := cliServerFile(t, s, up, wrongSlug, "shared_secret")
		wrong := s.CLI(t, "not-the-secret\n", "create-server", "--json", wrongFile, "--as", ownerSub)
		cliFirstSyncFailed(t, s, owner, wrong, "create-server-sync-401", wrongSlug, true)
		if v := catalogGet(t, owner, wrongSlug); v.ToolCount != 0 || v.LastSyncError == nil {
			t.Fatalf("server with a wrong secret: %+v", v)
		}
	})

	t.Run("create-server takes the secret from --secret-env, which wins over stdin", func(t *testing.T) {
		secret := harness.Unique("sekret-env")
		stdinSecret := harness.Unique("sekret-ignored")
		slug := harness.Unique("cli-env")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
		file := cliServerFile(t, s, up, slug, "shared_secret")
		r := s.CLIWithEnv(t, map[string]string{"CLI_SECRET": secret}, stdinSecret+"\n",
			"create-server", "--json", file, "--as", ownerSub, "--secret-env", "CLI_SECRET")
		cliGolden(t, s, "create-server-env", r, 0, slug, "<SLUG>")
		if v := catalogGet(t, owner, slug); !v.SecretSet || v.ToolCount != 1 || v.LastSyncError != nil {
			t.Fatalf("server after create-server --secret-env: %+v (the upstream accepts only the variable's value)", v)
		}
		cliExpectNoSecret(t, s, secret, r)
		cliExpectNoSecret(t, s, stdinSecret, r)
		// Control: the variable must exist; without it nothing is created.
		before := cliServerCount(t, s)
		missing := s.CLIWithEnv(t, nil, "", "create-server", "--json", file, "--as", ownerSub, "--secret-env", "CLI_SECRET_UNSET")
		cliExpectUsage(t, "an unset --secret-env", missing)
		cliGolden(t, s, "create-server-secret-env-unset", missing, 2)
		if n := cliServerCount(t, s); n != before {
			t.Fatalf("an unset --secret-env created a server: %d -> %d", before, n)
		}
	})

	t.Run("a mode without a secret ignores stdin and stores nothing", func(t *testing.T) {
		ignored := harness.Unique("sekret-none")
		slug := harness.Unique("cli-none")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		file := cliServerFile(t, s, up, slug, "none")
		r := s.CLI(t, ignored+"\n", "create-server", "--json", file, "--as", ownerSub)
		cliGolden(t, s, "create-server-none", r, 0, slug, "<SLUG>")
		if v := catalogGet(t, owner, slug); v.SecretSet || v.ToolCount != 1 {
			t.Fatalf("server: %+v", v)
		}
		if n := countSQL(t, s, `select count(*) from catalog.servers where slug = $1 and secret is null`, slug); n != 1 {
			t.Fatal("a secret is stored for a server without one")
		}
		cliExpectNoSecret(t, s, ignored, r)
	})

	t.Run("an empty stdin leaves the server without a secret; a failed first sync does not undo the creation", func(t *testing.T) {
		slug := harness.Unique("cli-nosecret")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		file := cliServerFile(t, s, up, slug, "shared_secret")
		r := s.CLI(t, "", "create-server", "--json", file, "--as", ownerSub)
		// 1.0.0: exit 0, the server exists and the failed sync is part of the report, not of the exit
		// code. Decided (create-server-sync-exit): exit 1, the server still exists.
		cliFirstSyncFailed(t, s, owner, r, "create-server-no-secret", slug, false)
		v := catalogGet(t, owner, slug)
		if v.SecretSet || v.ToolCount != 0 || v.syncError() != "The server secret is not set" {
			t.Fatalf("server: %+v (sync error %q)", v, v.syncError())
		}

		failing := harness.Unique("cli-failing")
		fup := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		fup.FailWith(500)
		ffile := cliServerFile(t, s, fup, failing, "shared_secret")
		secret := harness.Unique("sekret-fail")
		fr := s.CLI(t, secret+"\n", "create-server", "--json", ffile, "--as", ownerSub)
		cliFirstSyncFailed(t, s, owner, fr, "create-server-sync-failed", failing, true)
		fv := catalogGet(t, owner, failing)
		if !fv.SecretSet || fv.ToolCount != 0 || fv.syncError() != catalogMsgUnreachable {
			t.Fatalf("server after a failed first sync: %+v (sync error %q)", fv, fv.syncError())
		}
		cliExpectNoSecret(t, s, secret, fr)
		// Control: once the upstream works, a sync fills the tools and clears the error.
		fup.Reset()
		cliOK(t, s, "", "sync", failing)
		if fv := catalogGet(t, owner, failing); fv.ToolCount != 1 || fv.LastSyncError != nil {
			t.Fatalf("server after the upstream recovered: %+v", fv)
		}
	})

	t.Run("an administrator is required; invalid input is refused with exit 1 or 2 and creates nothing", func(t *testing.T) {
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		slug := harness.Unique("cli-refused")
		file := cliServerFile(t, s, up, slug, "none")
		before := cliServerCount(t, s)
		subs := []string{slug, "<SLUG>"}

		member := s.CLI(t, "", "create-server", "--json", file, "--as", memberSub)
		cliGolden(t, s, "create-server-member", member, 1, subs...)
		ghost := harness.Unique("cli-ghost")
		unknown := s.CLI(t, "", "create-server", "--json", file, "--as", ghost)
		cliGolden(t, s, "create-server-unknown-sub", unknown, 1, ghost, "<SUB>")

		missing := s.CLI(t, "", "create-server", "--json", "/tmp/compat-no-such-file.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-missing-file", missing, 1)
		s.PutFile(t, "/tmp/compat-not-json.json", []byte("{nope"))
		notJSON := s.CLI(t, "", "create-server", "--json", "/tmp/compat-not-json.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-not-json", notJSON, 1)

		s.PutFile(t, "/tmp/compat-bad-url.json", []byte(mustJSON(t, fakes.ServerInput(up, slug, "none", fakes.WithURL("ftp://nope")))))
		badURL := s.CLI(t, "", "create-server", "--json", "/tmp/compat-bad-url.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-invalid-url", badURL, 1, subs...)

		// A file with several missing fields: the issues are listed by field path; their wording is
		// the validator's and is not pinned.
		s.PutFile(t, "/tmp/compat-empty-def.json", []byte(`{"slug":"`+slug+`"}`))
		empty := s.CLI(t, "", "create-server", "--json", "/tmp/compat-empty-def.json", "--as", ownerSub)
		if empty.Code != 1 || empty.Stdout != "" || !strings.HasPrefix(empty.Stderr, "error: /tmp/compat-empty-def.json is not a valid server:\n") {
			t.Fatalf("an incomplete server file: %+v", empty)
		}
		var paths []string
		for _, m := range cliIssueLine.FindAllStringSubmatch(empty.Stderr, -1) {
			paths = append(paths, m[1])
		}
		if !slices.Equal(paths, []string{"name", "description", "url", "auth", "access"}) {
			t.Errorf("fields named by the issues: %v", paths)
		}
		emptyMasked := empty
		emptyMasked.Stderr = cliIssueLine.ReplaceAllString(empty.Stderr, "  $1: <issue>")
		cliGolden(t, s, "create-server-incomplete", emptyMasked, 1, subs...)

		s.PutFile(t, "/tmp/compat-bad-slug.json", []byte(mustJSON(t, fakes.ServerInput(up, "Not A Slug", "none"))))
		badSlug := s.CLI(t, "", "create-server", "--json", "/tmp/compat-bad-slug.json", "--as", ownerSub)
		cliGolden(t, s, "create-server-invalid-slug", badSlug, 1)

		noJSON := s.CLI(t, "", "create-server", "--as", ownerSub)
		cliExpectUsage(t, "create-server without --json", noJSON)
		cliGolden(t, s, "create-server-missing-json", noJSON, 2)
		noAs := s.CLI(t, "", "create-server", "--json", file)
		cliExpectUsage(t, "create-server without --as", noAs)
		cliGolden(t, s, "create-server-missing-as", noAs, 2)

		if n := cliServerCount(t, s); n != before {
			t.Fatalf("refused runs created servers: %d -> %d", before, n)
		}
		if r := owner.Do(t, "GET", "/api/servers/"+slug, nil); r.Status != 404 {
			t.Fatalf("GET /api/servers/%s after the refusals: %d", slug, r.Status)
		}
		// Control: the same file as an administrator creates the server.
		ok := cliOK(t, s, "", "create-server", "--json", file, "--as", ownerSub)
		if ok.Stdout != fmt.Sprintf("created server %q (none); 1 tool(s)\n", slug) {
			t.Fatalf("the control: %q", ok.Stdout)
		}

		// A slug that exists is refused and the first server stays as it was.
		dupSecret := harness.Unique("sekret-dup")
		dupSlug := harness.Unique("cli-dup")
		dup := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		dupFile := cliServerFile(t, s, dup, dupSlug, "shared_secret")
		cliOK(t, s, dupSecret+"\n", "create-server", "--json", dupFile, "--as", ownerSub)
		sealed := cliSealed(t, s, dupSlug)
		again := s.CLI(t, harness.Unique("sekret-other")+"\n", "create-server", "--json", dupFile, "--as", ownerSub)
		cliGolden(t, s, "create-server-duplicate", again, 1, dupSlug, "<SLUG>")
		if !bytes.Equal(sealed, cliSealed(t, s, dupSlug)) {
			t.Fatal("a refused duplicate changed the stored secret")
		}
	})

	t.Run("sync re-reads the tools; with --as it is audited and records the administrator", func(t *testing.T) {
		slug := harness.Unique("cli-sync")
		subs := []string{slug, "<SLUG>"}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, up, slug, "none"), "--as", ownerSub)
		creation := len(cliSyncAudit(t, s, slug)) // the first sync of create-server is audited too
		if creation != 1 {
			t.Fatalf("server.sync audit rows after create-server: %d", creation)
		}
		up.Reset()

		// As the scheduler does: no administrator, no audit record.
		r := s.CLI(t, "", "sync", slug)
		cliGolden(t, s, "sync", r, 0, subs...)
		if up.ListRequests() == 0 {
			t.Fatal("sync did not contact the upstream")
		}
		if n := len(cliSyncAudit(t, s, slug)); n != creation {
			t.Fatalf("a sync without --as wrote %d audit record(s)", n-creation)
		}
		if id := catalogSyncUser(t, s, slug); id != ownerID {
			t.Fatalf("sync_user_id %s, want the administrator of the creation %s", id, ownerID)
		}

		up.SetTools(fakes.Tools("echo", "other"))
		added := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-added", added, 0, subs...)
		if _, tools := catalogTools(t, owner, slug); !slices.Equal(catalogToolNames(tools), []string{"echo", "other"}) {
			t.Fatalf("tools after the sync: %v", catalogToolNames(tools))
		}
		syncs := cliSyncAudit(t, s, slug)
		if len(syncs) != creation+1 || !ptrIs(syncs[1].ActorID, ownerID) {
			t.Fatalf("server.sync audit after --as: %+v", syncs)
		}
		cliMetaIs(t, syncs[1], `{"added":1,"changed":0,"removed":0}`)

		changed := fakes.Tool{Name: "echo", Description: "A new description.", InputSchema: map[string]any{"type": "object"}}
		up.SetTools([]fakes.Tool{changed})
		both := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-changed-removed", both, 0, subs...)
		_, tools := catalogTools(t, owner, slug)
		if !slices.Equal(catalogToolNames(tools), []string{"echo"}) || tools[0].Description != "A new description." {
			t.Fatalf("tools after the second sync: %+v", tools)
		}
	})

	t.Run("sync of a personal-auth server uses the connection of the administrator it names", func(t *testing.T) {
		slug := harness.Unique("cli-pat")
		subs := []string{slug, "<SLUG>"}
		pat := harness.Unique("pat-owner")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		file := cliServerFile(t, s, up, slug, "pat_user", fakes.WithHeader("X-Token", "pat {secret}"))
		created := s.CLI(t, "", "create-server", "--json", file, "--as", ownerSub)
		cliFirstSyncFailed(t, s, owner, created, "create-server-pat-needs-connection", slug, false)

		// No connection yet: refused, by the administrator and by the scheduler alike.
		denied := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-needs-connection", denied, 1, subs...)
		if sch := s.CLI(t, "", "sync", slug); sch.Code != 1 || sch.Stderr != denied.Stderr {
			t.Fatalf("the scheduler's sync without a connection: %+v", sch)
		}
		failed := cliSyncAudit(t, s, slug)
		cliMetaIs(t, failed[len(failed)-1], `{"error":"needs_connection"}`)
		if v := catalogGet(t, owner, slug); v.syncError() != catalogMsgNeedsConnection || v.ToolCount != 0 {
			t.Fatalf("server after the refused sync: %+v", v)
		}

		if r := owner.Do(t, "PUT", "/api/connections/"+slug+"/pat", map[string]string{"token": pat}); r.Status != 204 {
			t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
		}
		up.Reset()
		ok := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-pat", ok, 0, subs...)
		if reqs := up.Requests(); len(reqs) == 0 || slices.ContainsFunc(reqs, func(q fakes.Request) bool { return q.Header.Get("X-Token") != "pat "+pat }) {
			t.Fatalf("the upstream did not get the administrator's token on every request: %d request(s)", len(reqs))
		}
		if v := catalogGet(t, owner, slug); v.ToolCount != 1 || v.LastSyncError != nil {
			t.Fatalf("server after the sync: %+v", v)
		}
		if id := catalogSyncUser(t, s, slug); id != ownerID {
			t.Fatalf("sync_user_id %s, want %s", id, ownerID)
		}

		// The scheduler's sync now lends the last successful administrator's connection.
		up.Reset()
		sched := s.CLI(t, "", "sync", slug)
		if sched.Code != 0 || sched.Stdout != fmt.Sprintf("synced %q: 0 added, 0 changed, 0 removed\n", slug) {
			t.Fatalf("sync without --as after an administrator synced: %+v", sched)
		}
		if reqs := up.Requests(); len(reqs) == 0 || reqs[0].Header.Get("X-Token") != "pat "+pat {
			t.Fatalf("the scheduler's sync did not use the last administrator's token")
		}
		// Control: another administrator without a connection of their own gets none.
		otherSub := harness.Unique("cli-admin2")
		cliOK(t, s, "", "create-user", "--sub", otherSub, "--name", "Other Admin", "--admin")
		other := s.CLI(t, "", "sync", slug, "--as", otherSub)
		cliGolden(t, s, "sync-other-admin-needs-connection", other, 1, subs...)
	})

	t.Run("sync failures and refusals: upstream down, unknown, disabled, not an administrator", func(t *testing.T) {
		slug := harness.Unique("cli-syncerr")
		subs := []string{slug, "<SLUG>"}
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, up, slug, "none"), "--as", ownerSub)

		up.FailWith(500)
		down := s.CLI(t, "", "sync", slug, "--as", ownerSub)
		cliGolden(t, s, "sync-upstream-failed", down, 1, subs...)
		if v := catalogGet(t, owner, slug); v.syncError() != catalogMsgUnreachable || v.ToolCount != 1 {
			t.Fatalf("server after a failed sync: %+v (the tools must stay)", v)
		}
		syncs := cliSyncAudit(t, s, slug)
		last := syncs[len(syncs)-1]
		if !ptrIs(last.ActorID, ownerID) || last.Meta["error"] == nil {
			t.Fatalf("audit of the failed sync: %+v", last)
		}
		up.Reset()
		cliOK(t, s, "", "sync", slug)
		if v := catalogGet(t, owner, slug); v.LastSyncError != nil {
			t.Fatalf("the error stays after a good sync: %q", v.syncError())
		}

		ghost := harness.Unique("cli-noserver")
		unknown := s.CLI(t, "", "sync", ghost)
		cliGolden(t, s, "sync-unknown", unknown, 1, ghost, "<SLUG>")
		asGhost := harness.Unique("cli-ghost")
		noPerson := s.CLI(t, "", "sync", slug, "--as", asGhost)
		cliGolden(t, s, "sync-as-unknown", noPerson, 1, asGhost, "<SUB>")
		asMember := s.CLI(t, "", "sync", slug, "--as", memberSub)
		cliGolden(t, s, "sync-as-member", asMember, 1)
		missing := s.CLI(t, "", "sync")
		cliExpectUsage(t, "sync without a slug", missing)
		cliGolden(t, s, "sync-missing-slug", missing, 2)

		if r := owner.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]string{"status": "disabled"}); r.Status != 204 {
			t.Fatalf("disable: %d", r.Status)
		}
		off := s.CLI(t, "", "sync", slug)
		cliGolden(t, s, "sync-disabled-server", off, 1, subs...)
		if r := owner.Do(t, "POST", "/api/servers/"+slug+"/status", map[string]string{"status": "active"}); r.Status != 204 {
			t.Fatalf("enable: %d", r.Status)
		}
		if on := s.CLI(t, "", "sync", slug); on.Code != 0 {
			t.Fatalf("sync after enabling: %+v", on)
		}
	})

	t.Run("reindex rebuilds the search index and prints the number of indexed tools", func(t *testing.T) {
		slug := harness.Unique("cli-index")
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo", "ping")})
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, up, slug, "none"), "--as", ownerSub)
		catalogWaitDocs(t, s, slug, 2)
		catalogWaitDelivered(t, s)

		type docs map[string]string // ref -> embedding digest
		snapshot := func() (docs, int) {
			rows, err := s.DB.Query(t.Context(), `select ref, md5(embedding::text) from search.documents order by ref`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			d := docs{}
			for rows.Next() {
				var ref string
				var digest *string
				if err := rows.Scan(&ref, &digest); err != nil {
					t.Fatal(err)
				}
				if digest == nil {
					t.Fatalf("%s has no embedding", ref)
				}
				d[ref] = *digest
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			return d, len(d)
		}
		before, total := snapshot()
		// The searches go through /mcp: they are nested subtests, in the order of the steps, so that
		// the SQL and CLI steps around them can be judged without that route.
		token := owner.Token(t, "cli-reindex")
		t.Run("search finds the tool before the documents are deleted", func(t *testing.T) {
			sess := s.MCP(t, token)
			if got := catalogHitRefs(catalogSearch(t, sess, "ping", 10, []string{slug})); !slices.Contains(got, slug+".ping") {
				t.Fatalf("search before: %v", got)
			}
		})

		execSQL(t, s, `delete from search.documents where server = $1`, slug)
		t.Run("search finds nothing once the documents are deleted", func(t *testing.T) {
			sess := s.MCP(t, token)
			if got := catalogHitRefs(catalogSearch(t, sess, "ping", 10, []string{slug})); len(got) != 0 {
				t.Fatalf("search finds %v after the documents were deleted: the control proves nothing", got)
			}
		})

		r := s.CLI(t, "", "reindex")
		// The number is the count of indexed TOOLS (rows of search.documents). 1.0.0 words it
		// "server(s)"; decided (reindex-wording): "tool(s)".
		if s.Fixed(harness.DevReindexWording) {
			if want := fmt.Sprintf("reindexed %d tool(s)\n", total); r.Code != 0 || r.Stdout != want {
				t.Fatalf("reindex printed %q (exit %d), want %q (the number of rows of search.documents)", r.Stdout, r.Code, want)
			}
		} else {
			if want := fmt.Sprintf("reindexed %d server(s)\n", total); r.Stdout != want {
				t.Fatalf("reindex printed %q, want %q (the number of rows of search.documents)", r.Stdout, want)
			}
			// The count depends on what else is indexed on the stack: the golden holds a placeholder.
			cliGolden(t, s, "reindex", r, 0, fmt.Sprintf("reindexed %d server(s)", total), "reindexed <DOCS> server(s)")
		}
		after, _ := snapshot()
		if len(after) != total {
			t.Fatalf("documents after reindex: %d, before %d", len(after), total)
		}
		for ref, digest := range before {
			if after[ref] != digest {
				t.Errorf("%s: the embedding after reindex differs from the one before", ref)
			}
		}
		t.Run("search finds the tool again after the reindex", func(t *testing.T) {
			sess := s.MCP(t, token)
			if got := catalogHitRefs(catalogSearch(t, sess, "ping", 10, []string{slug})); !slices.Contains(got, slug+".ping") {
				t.Fatalf("search after reindex: %v", got)
			}
		})

		extra := s.CLI(t, "", "reindex", "extra")
		cliExpectUsage(t, "reindex with an argument", extra)
		cliGolden(t, s, "reindex-extra-argument", extra, 2)
	})
}

// TestCLIReindexEmpty: with no server the index is empty and reindex says 0.
func TestCLIReindexEmpty(t *testing.T) {
	s := harness.Start(t, harness.Options{})
	if n := countSQL(t, s, `select count(*) from catalog.servers`); n != 0 {
		t.Fatalf("a fresh stack has %d servers", n)
	}
	r := s.CLI(t, "", "reindex")
	if s.Fixed(harness.DevReindexWording) {
		// Decided (reindex-wording): the unit is "tool(s)". No golden: it cannot be recorded from 1.0.0.
		if r.Code != 0 || r.Stdout != "reindexed 0 tool(s)\n" {
			t.Fatalf("reindex on an empty index: %v", r)
		}
		return
	}
	cliGolden(t, s, "reindex-empty", r, 0)
	if r.Stdout != "reindexed 0 server(s)\n" {
		t.Fatalf("reindex on an empty index: %v", r)
	}
}

// cliFirstSyncFailed judges a create-server whose first sync failed. 1.0.0: exit 0 and the golden
// (the failed sync is part of the report). Decided (create-server-sync-exit): exit 1, the server
// still exists with secretSet as given, and the output says the sync failed; no golden (it cannot
// be recorded from 1.0.0).
func cliFirstSyncFailed(t *testing.T, s *harness.Stack, owner *harness.Client, r harness.CLIResult, golden, slug string, secretSet bool) {
	t.Helper()
	if !s.Fixed(harness.DevCreateServerSyncExit) {
		cliGolden(t, s, golden, r, 0, slug, "<SLUG>")
		return
	}
	if problem := createServerSyncFailureProblem(r); problem != "" {
		t.Fatalf("create-server of %s: %s: %v", slug, problem, r)
	}
	if v := catalogGet(t, owner, slug); v.SecretSet != secretSet {
		t.Fatalf("the server after the failed first sync: secretSet %v, want %v", v.SecretSet, secretSet)
	}
}

// createServerSyncFailureProblem is the decided create-server outcome of a failed first sync:
// exit 1 and "sync failed" on stdout or stderr. It returns "" when r is that.
func createServerSyncFailureProblem(r harness.CLIResult) string {
	if r.Code != 1 {
		return fmt.Sprintf("exit code %d, want 1", r.Code)
	}
	if !strings.Contains(r.Stdout+r.Stderr, "sync failed") {
		return "neither stdout nor stderr says `sync failed`"
	}
	return ""
}
