//go:build compat

package compat_test

// reset-key-check, rotate-secrets-key, the key guard and the refusal of a database from a newer
// version; see the header of cli_users_test.go for the conventions.

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// TestCLIKeys is ONE scenario in sequential steps (the subtests hand the app and its keys on to one
// another: the key check is reset, the app is restarted, stopped with a wrong key, brought back,
// rotated): run the test as a whole. Nothing in its goldens depends on how much data the earlier
// steps created: counts are read from the database and masked.
func TestCLIKeys(t *testing.T) {
	k1, k2, k3 := newKey(t), newKey(t), newKey(t)
	s := harness.Start(t, harness.Options{Env: map[string]string{"SECRETS_KEY": k1}})
	ownerSub := harness.Unique("cli-keys-owner")
	owner := cliSignIn(t, s, ownerSub, "Owner", true)
	ownerID := cliMustUser(t, s, ownerSub).ID
	keyed := func(key string) map[string]string { return map[string]string{"SECRETS_KEY": key} }

	t.Run("reset-key-check needs --yes; with it the check goes, and the next start records the key again", func(t *testing.T) {
		before := keyCheckBytes(t, s)
		no := s.CLI(t, "", "reset-key-check")
		cliExpectUsage(t, "reset-key-check without --yes", no)
		cliGolden(t, s, "reset-key-check-no-yes", no, 2)
		if !bytes.Equal(before, keyCheckBytes(t, s)) {
			t.Fatal("a refused reset-key-check changed the check")
		}
		if n := countSQL(t, s, `select count(*) from platform.audit_log where action = 'secrets.key_check_reset'`); n != 0 {
			t.Fatalf("a refused reset-key-check was audited (%d)", n)
		}

		r := s.CLI(t, "", "reset-key-check", "--yes")
		cliGolden(t, s, "reset-key-check", r, 0)
		if n := rowCount(t, s, "select count(*) from platform.key_check"); n != 0 {
			t.Fatalf("key_check rows after the reset: %d", n)
		}
		resets := auditRows(t, s, "where action = 'secrets.key_check_reset' order by id")
		if len(resets) != 1 || resets[0].ActorID != nil || resets[0].Target != nil {
			t.Fatalf("audit of the reset: %+v", resets)
		}
		cliMetaIs(t, resets[0], `{"removed":true,"via":"cli"}`)

		// The CLI never creates the check, with the right key or any other.
		cliOK(t, s, "", "create-user", "--sub", harness.Unique("cli-nocheck"), "--name", "x")
		silent := s.CLIWithEnv(t, keyed(k3), "", "create-user", "--sub", harness.Unique("cli-nocheck"), "--name", "x")
		if silent.Code != 0 || silent.Stderr != "" {
			t.Fatalf("a command with another key while no check is stored: %+v", silent)
		}
		if n := rowCount(t, s, "select count(*) from platform.key_check"); n != 0 {
			t.Fatalf("a CLI command created the check (%d rows)", n)
		}

		nothing := s.CLI(t, "", "reset-key-check", "--yes")
		cliGolden(t, s, "reset-key-check-nothing", nothing, 0)
		resets = auditRows(t, s, "where action = 'secrets.key_check_reset' order by id")
		if len(resets) != 2 {
			t.Fatalf("audit rows after the second reset: %d", len(resets))
		}
		cliMetaIs(t, resets[1], `{"removed":false,"via":"cli"}`)

		// With no check stored rotate-secrets-key says so. The stack's key stays the current key and an
		// unrelated one is the previous key: everything already opens with the current key, so
		// nothing is re-sealed and the rotation leaves the data alone.
		secrets, conns := cliSecretCounts(t, s)
		none := s.CLIWithEnv(t, map[string]string{"SECRETS_KEY_OLD": k3}, "", "rotate-secrets-key")
		if want := cliRotateStdout([2]int{0, secrets}, [2]int{0, conns}, "none stored"); none.Stdout != want || none.Stderr != "" {
			t.Fatalf("rotate-secrets-key without a stored check: %v, want stdout %q", none, want)
		}
		noneSubs := append(cliRotateSubs("server secrets", 0, secrets, "0", "<SECRETS>"), cliRotateSubs("connections", 0, conns, "0", "<CONNECTIONS>")...)
		cliGolden(t, s, "rotate-secrets-key-none-stored", none, 0, noneSubs...)
		if n := rowCount(t, s, "select count(*) from platform.key_check"); n != 0 {
			t.Fatalf("rotate-secrets-key created the check (%d rows)", n)
		}
		rotations := auditRows(t, s, "where action = 'secrets.rotate' order by id")
		if len(rotations) != 1 || rotations[0].ActorID != nil || rotations[0].Target != nil {
			t.Fatalf("secrets.rotate audit: %+v", rotations)
		}
		cliMetaIs(t, rotations[0], fmt.Sprintf(`{"connections":{"current":%d,"failed":0,"resealed":0},"keyCheck":"absent","servers":{"current":%d,"failed":0,"resealed":0},"via":"cli"}`, conns, secrets))

		cliSwap(t, s, k1) // the next start records the configured key
		if n := rowCount(t, s, "select count(*) from platform.key_check"); n != 1 {
			t.Fatalf("key_check rows after the restart: %d", n)
		}
	})

	t.Run("on a mismatching key the CLI warns and proceeds, except create-server; it runs with the app down", func(t *testing.T) {
		// The app refuses to start with another key: its container is gone, and the CLI still runs in
		// a one-off container of the same image.
		if code, logs := s.SwapImageExpectingExit(t, platformImage(), "", keyed(k3)); code != 1 {
			t.Fatalf("the app with another key: exit %d: %s", code, logs)
		}
		// Control: with the right key the CLI is silent.
		subOK := harness.Unique("cli-guard-ok")
		ok := s.CLIWithEnv(t, keyed(k1), "", "create-user", "--sub", subOK, "--name", "Right Key")
		if ok.Code != 0 || ok.Stderr != "" {
			t.Fatalf("create-user with the right key: %+v", ok)
		}

		sub := harness.Unique("cli-guard")
		warn := s.CLIWithEnv(t, keyed(k3), "", "create-user", "--sub", sub, "--name", "Wrong Key")
		cliGolden(t, s, "key-guard-warning", warn, 0, sub, "<SUB>")
		cliExpectWarning(t, "create-user", warn)
		if _, found := cliUser(t, s, sub); !found {
			t.Fatal("create-user did not proceed after the warning")
		}
		var guardToken, guardCode string // the effects are checked once the app is back
		for _, args := range [][]string{
			{"login-link", sub},
			{"create-token", sub, "--name", "guard"},
			{"reindex"},
			{"sync", harness.Unique("cli-noserver")},
		} {
			r := s.CLIWithEnv(t, keyed(k3), "", args...)
			cliExpectWarning(t, args[0], r)
			switch args[0] {
			case "login-link":
				if m := cliLinkRe.FindStringSubmatch(r.Stdout); m != nil {
					guardCode = m[2]
				}
			case "create-token":
				guardToken = strings.SplitN(r.Stdout, "\n", 2)[0]
			}
			if args[0] != "sync" && r.Code != 0 {
				t.Errorf("%s with another key: exit %d, want it to proceed", args[0], r.Code)
			}
			if args[0] == "sync" && (r.Code != 1 || !strings.Contains(r.Stderr, "not_found")) {
				t.Errorf("sync with another key: %+v, want the command's own error after the warning", r)
			}
		}

		// create-server is the one command that seals a new secret: refused, nothing created.
		up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
		slug := harness.Unique("cli-guard-srv")
		file := cliServerFile(t, s, up, slug, "shared_secret")
		secret := harness.Unique("sekret-guard")
		before := cliServerCount(t, s)
		refused := s.CLIWithEnv(t, keyed(k3), secret+"\n", "create-server", "--json", file, "--as", ownerSub)
		cliGolden(t, s, "key-guard-refused", refused, 1)
		if refused.Stderr != cliKeyRefusal+"\n" || refused.Stdout != "" {
			t.Fatalf("create-server with another key: %+v", refused)
		}
		if n := cliServerCount(t, s); n != before || len(up.Requests()) != 0 {
			t.Fatalf("a refused create-server created %d server(s) and contacted the upstream %d time(s)", n-before, len(up.Requests()))
		}
		// Control: the same command with the right key works.
		created := s.CLIWithEnv(t, keyed(k1), secret+"\n", "create-server", "--json", file, "--as", ownerSub)
		if created.Code != 0 || created.Stderr != "" {
			t.Fatalf("create-server with the right key: %+v", created)
		}

		// rotate-secrets-key and reset-key-check handle the key themselves: no warning.
		rot := s.CLIWithEnv(t, keyed(k3), "", "rotate-secrets-key")
		cliGolden(t, s, "rotate-secrets-key-no-old", rot, 1)
		reset := s.CLIWithEnv(t, keyed(k3), "", "reset-key-check")
		if reset.Code != 2 || strings.Contains(reset.Stderr, "warning:") {
			t.Fatalf("reset-key-check with another key: %+v", reset)
		}
		// A previous key that opens the check is not a mismatch: the rotation is pending, nothing to warn about.
		pending := s.CLIWithEnv(t, map[string]string{"SECRETS_KEY": k3, "SECRETS_KEY_OLD": k1}, "", "create-user", "--sub", harness.Unique("cli-pending"), "--name", "x")
		if pending.Code != 0 || pending.Stderr != "" {
			t.Fatalf("create-user with the previous key configured: %+v", pending)
		}
		cliSwap(t, s, k1)
		cliExpectNoSecret(t, s, secret, refused, created) // the app is back, so its log can be read
		// The guard only warns for these commands: what they printed works.
		if !strings.HasPrefix(guardToken, "sk_") || guardCode == "" {
			t.Fatalf("create-token or login-link printed no credential under the guard (token %q, code found %v)", harness.Redact(guardToken), guardCode != "")
		}
		if names := cliToolNames(t, s.MCP(t, guardToken)); !slices.Equal(names, cliMetaTools) {
			t.Errorf("tools of the token created under the guard: %v", names)
		}
		gc := s.Anonymous()
		if r := identityPostLink(t, gc, guardCode); r.Status != 204 {
			t.Errorf("the link issued under the guard: %d %s", r.Status, r.Body)
		} else if me := whoami(t, gc); me.UserID != cliMustUser(t, s, sub).ID {
			t.Errorf("the link signed in %+v", me)
		}
	})

	t.Run("a database from a newer version: the message and exit 1, nothing applied; --version still works", func(t *testing.T) {
		sub := harness.Unique("cli-newer")
		migrations := rowCount(t, s, "select count(*) from platform.schema_migrations")
		execSQL(t, s, `insert into platform.schema_migrations (name) values ($1)`, newerDBMigrName)
		r := s.CLIWithEnv(t, nil, "", "create-user", "--sub", sub, "--name", "Newer")
		cliGolden(t, s, "newer-database", r, 1)
		if _, found := cliUser(t, s, sub); found {
			t.Fatal("a command ran against a database from a newer version")
		}
		if v := s.CLIWithEnv(t, nil, "", "--version"); v.Code != 0 {
			t.Errorf("--version with a newer database: %+v", v)
		}
		execSQL(t, s, `delete from platform.schema_migrations where name = $1`, newerDBMigrName)
		if n := rowCount(t, s, "select count(*) from platform.schema_migrations"); n != migrations {
			t.Fatalf("migration rows %d, want %d", n, migrations)
		}
		// Control: without the unknown migration the same command works.
		if r := s.CLIWithEnv(t, nil, "", "create-user", "--sub", sub, "--name", "Newer"); r.Code != 0 {
			t.Fatalf("create-user after restoring: %+v", r)
		}
	})

	t.Run("rotate-secrets-key re-seals what the app then opens with the new key", func(t *testing.T) {
		secret := harness.Unique("sekret-rotate")
		pat := harness.Unique("pat-rotate")
		sharedUp := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"Authorization", "Bearer " + secret}})
		patUp := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo"), RequireHeader: [2]string{"X-Token", "pat " + pat}})
		shared, personal := harness.Unique("cli-rot-shared"), harness.Unique("cli-rot-pat")
		cliOK(t, s, secret+"\n", "create-server", "--json", cliServerFile(t, s, sharedUp, shared, "shared_secret"), "--as", ownerSub)
		cliOK(t, s, "", "create-server", "--json", cliServerFile(t, s, patUp, personal, "pat_user", fakes.WithHeader("X-Token", "pat {secret}")), "--as", ownerSub)
		if r := owner.Do(t, "PUT", "/api/connections/"+personal+"/pat", map[string]string{"token": pat}); r.Status != 204 {
			t.Fatalf("PUT pat: %d %s", r.Status, r.Body)
		}
		cliOK(t, s, "", "sync", personal, "--as", ownerSub)
		token := owner.Token(t, "cli-rotate")

		callBoth := func(what string) {
			t.Helper()
			sharedUp.Reset()
			patUp.Reset()
			sess := s.MCP(t, token)
			for _, slug := range []string{shared, personal} {
				res := catalogCallMCP(t, sess, "call_tool", map[string]any{"tool": slug + ".echo", "arguments": map[string]any{"text": "hi"}})
				if res.IsError || catalogText(res) != `{"text":"hi"}` {
					t.Fatalf("%s: call on %s: isError %v %q", what, slug, res.IsError, catalogText(res))
				}
			}
			if c := sharedUp.Calls(); len(c) != 1 || c[0].Header.Get("Authorization") != "Bearer "+secret {
				t.Fatalf("%s: the shared-secret upstream got %d call(s) without the secret", what, len(c))
			}
			if c := patUp.Calls(); len(c) != 1 || c[0].Header.Get("X-Token") != "pat "+pat {
				t.Fatalf("%s: the personal-token upstream got %d call(s) without the token", what, len(c))
			}
		}
		callBoth("before the rotation (control)")

		secretBefore := cliSealed(t, s, shared)
		connBefore := connRowOf(t, s, ownerID, personal).Access
		rotation := map[string]string{"SECRETS_KEY": k2, "SECRETS_KEY_OLD": k1}
		secrets, conns := cliSecretCounts(t, s) // read right before: other steps may have created some
		if secrets < 1 || conns < 1 {
			t.Fatalf("%d server secrets and %d connections: the rotation has nothing to re-seal", secrets, conns)
		}
		var maxAudit int64
		if err := s.DB.QueryRow(t.Context(), `select coalesce(max(id), 0) from platform.audit_log`).Scan(&maxAudit); err != nil {
			t.Fatal(err)
		}
		r := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		if want := cliRotateStdout([2]int{secrets, 0}, [2]int{conns, 0}, "re-sealed"); r.Stdout != want {
			t.Fatalf("rotate-secrets-key printed %q, want %q (the counts of the database)", r.Stdout, want)
		}
		firstSubs := append(cliRotateSubs("server secrets", secrets, 0, "<SECRETS>", "0"), cliRotateSubs("connections", conns, 0, "<CONNECTIONS>", "0")...)
		cliGolden(t, s, "rotate-secrets-key", r, 0, firstSubs...)
		if bytes.Equal(secretBefore, cliSealed(t, s, shared)) || bytes.Equal(connBefore, connRowOf(t, s, ownerID, personal).Access) {
			t.Fatal("the sealed values did not change")
		}
		for _, v := range []string{secret, pat} {
			if strings.Contains(r.Stdout, v) || strings.Contains(r.Stderr, v) {
				t.Errorf("the rotation printed %s", harness.Redact(v))
			}
		}
		cliExpectNoSecret(t, s, secret, r)

		// With the new key alone the app starts (the check was re-sealed) and opens both.
		cliSwap(t, s, k2)
		callBoth("after the rotation")

		again := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		if want := cliRotateStdout([2]int{0, secrets}, [2]int{0, conns}, "already current"); again.Stdout != want {
			t.Fatalf("the second rotate-secrets-key printed %q, want %q", again.Stdout, want)
		}
		againSubs := append(cliRotateSubs("server secrets", 0, secrets, "0", "<SECRETS>"), cliRotateSubs("connections", 0, conns, "0", "<CONNECTIONS>")...)
		cliGolden(t, s, "rotate-secrets-key-again", again, 0, againSubs...)
		runs := auditRows(t, s, "where action = 'secrets.rotate' and id > $1 order by id", maxAudit)
		if len(runs) != 2 || runs[0].ActorID != nil || runs[0].Target != nil {
			t.Fatalf("secrets.rotate audit: %+v", runs)
		}
		cliMetaIs(t, runs[0], fmt.Sprintf(`{"connections":{"current":0,"failed":0,"resealed":%d},"keyCheck":"resealed","servers":{"current":0,"failed":0,"resealed":%d},"via":"cli"}`, conns, secrets))
		cliMetaIs(t, runs[1], fmt.Sprintf(`{"connections":{"current":%d,"failed":0,"resealed":0},"keyCheck":"current","servers":{"current":%d,"failed":0,"resealed":0},"via":"cli"}`, conns, secrets))

		// A value that neither key opens is reported by name and left as it was; the exit code is 1.
		garbage := bytes.Repeat([]byte{0xab}, 48)
		goodSecret, goodConn := cliSealed(t, s, shared), connRowOf(t, s, ownerID, personal).Access
		execSQL(t, s, `update catalog.servers set secret = $2 where slug = $1`, shared, garbage)
		execSQL(t, s, `update connections.connections set access = $3 where user_id = $1::uuid and server = $2`, ownerID, personal, garbage)
		bad := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		if want := cliRotateStdout([2]int{0, secrets - 1}, [2]int{0, conns - 1}, "already current"); bad.Stdout != want {
			t.Fatalf("rotate-secrets-key with unreadable values printed %q, want %q", bad.Stdout, want)
		}
		if want := "error: neither key opens (left as they were):\n  server " + shared + "\n  connection " + ownerID + "/" + personal + "\n"; bad.Stderr != want {
			t.Fatalf("the unreadable values are named as %q, want %q", bad.Stderr, want)
		}
		badSubs := []string{ownerID, "<OWNER>", shared, "<SERVER-1>", personal, "<SERVER-2>"}
		badSubs = append(badSubs, cliRotateSubs("server secrets", 0, secrets-1, "0", "<SECRETS-1>")...)
		badSubs = append(badSubs, cliRotateSubs("connections", 0, conns-1, "0", "<CONNECTIONS-1>")...)
		cliGolden(t, s, "rotate-secrets-key-unreadable", bad, 1, badSubs...)
		if !bytes.Equal(cliSealed(t, s, shared), garbage) || !bytes.Equal(connRowOf(t, s, ownerID, personal).Access, garbage) {
			t.Fatal("a value that neither key opens was changed")
		}
		failedRun := auditRows(t, s, "where action = 'secrets.rotate' order by id desc limit 1")[0]
		cliMetaIs(t, failedRun, fmt.Sprintf(`{"connections":{"current":%d,"failed":1,"resealed":0},"keyCheck":"current","servers":{"current":%d,"failed":1,"resealed":0},"via":"cli"}`, conns-1, secrets-1))
		execSQL(t, s, `update catalog.servers set secret = $2 where slug = $1`, shared, goodSecret)
		execSQL(t, s, `update connections.connections set access = $3 where user_id = $1::uuid and server = $2`, ownerID, personal, goodConn)

		// The same for the key check.
		goodCheck := keyCheckBytes(t, s)
		execSQL(t, s, `update platform.key_check set sealed = $1`, garbage)
		badCheck := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key")
		if want := cliRotateStdout([2]int{0, secrets}, [2]int{0, conns}, "cannot be read with either key"); badCheck.Stdout != want || badCheck.Stderr != "error: neither key opens (left as they were):\n  key check\n" {
			t.Fatalf("rotate-secrets-key with an unreadable key check: %v", badCheck)
		}
		cliGolden(t, s, "rotate-secrets-key-unreadable-key-check", badCheck, 1, againSubs...)
		if !bytes.Equal(keyCheckBytes(t, s), garbage) {
			t.Fatal("an unreadable key check was changed")
		}
		execSQL(t, s, `update platform.key_check set sealed = $1`, goodCheck)
		if r := s.CLIWithEnv(t, rotation, "", "rotate-secrets-key"); r.Code != 0 {
			t.Fatalf("rotate-secrets-key after restoring the values: %+v", r)
		}
	})
}
