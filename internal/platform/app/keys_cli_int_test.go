//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	keysPlain      = "sekret-keys-0123456789" // a made-up value the tests look for in output
	keysAccess     = "access-token-ZZ-123"
	keysRefresh    = "refresh-token-ZZ-456"
	keysPAT        = "pat-token-ZZ-789"
	keyCheckPlain  = "mcp-registry key check v1"
	keyUserA       = "11111111-1111-1111-1111-111111111111"
	keyUserB       = "22222222-2222-2222-2222-222222222222"
	notUnderAnyKey = "0123456789abcdef0123456789abcdef0123456789abcdef" // 48 bytes: neither key opens it
)

// keysEnv is a database whose data was sealed with the OLD key (the CLI ran with it), and the
// configuration of the rotation: SECRETS_KEY the new key, SECRETS_KEY_OLD the old one.
type keysEnv struct {
	*serverEnv
	old, next []byte
	oldBox    *crypto.AESGCMBox
	nextBox   *crypto.AESGCMBox
	rotate    config.Config
}

func newKeysEnv(t *testing.T) *keysEnv {
	t.Helper()
	s := newServerEnv(t) // the admin "owner"; e.key is the key the data is sealed with
	e := &keysEnv{serverEnv: s, old: s.key, next: newKey(t)}
	var err error
	if e.oldBox, err = crypto.NewAESGCMBox(e.old, nil); err != nil {
		t.Fatal(err)
	}
	if e.nextBox, err = crypto.NewAESGCMBox(e.next, nil); err != nil {
		t.Fatal(err)
	}
	e.rotate = s.cfg
	e.rotate.SecretsKey = config.NewSecretBytes(e.next)
	e.rotate.SecretsKeyOld = config.NewSecretBytes(e.old)
	return e
}

func (e *keysEnv) sql(sqlText string, args ...any) {
	e.t.Helper()
	exec(e.t, e.url, sqlText, args...)
}

func (e *keysEnv) bytes(sqlText string, args ...any) []byte {
	e.t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var b []byte
	if err := c.QueryRow(ctx, sqlText, args...).Scan(&b); err != nil {
		e.t.Fatal(err)
	}
	return b
}

// addServer creates a server through the CLI (sealed with the old key). secret "" is none auth.
func (e *keysEnv) addServer(slug, secret string) {
	e.t.Helper()
	over := map[string]any{"slug": slug}
	if secret == "" {
		over["auth"] = map[string]any{"kind": "none"}
	}
	r := e.create(secret+"\n", nil, "--json", e.file(slug+".json", over), "--as", "owner")
	if r.code != 0 {
		e.t.Fatalf("create %s: %+v", slug, r)
	}
}

func (e *keysEnv) seal(box *crypto.AESGCMBox, plain string) []byte {
	e.t.Helper()
	b, err := box.Seal(plain)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// addConnection inserts a connection as the app would have stored it (sealed with the old key).
func (e *keysEnv) addConnection(user, server, kind, status string, access string, refresh string, expires bool) {
	e.t.Helper()
	var refreshed any
	if refresh != "" {
		refreshed = e.seal(e.oldBox, refresh)
	}
	var expiry any
	if expires {
		expiry = "2031-01-01T00:00:00Z"
	}
	e.sql(`insert into connections.connections (user_id, server, kind, access, refresh, expires_at, status, issued_for, updated_at)
		values ($1::uuid, $2, $3, $4, $5, $6::timestamptz, $7, 'fp', '2030-05-05T05:05:05Z')`,
		user, server, kind, e.seal(e.oldBox, access), refreshed, expiry, status)
}

// seed: three servers (an active one with a secret, a disabled one with a secret, one without),
// three connections (an OAuth one with a refresh token, a PAT, an expired OAuth one).
func (e *keysEnv) seed() {
	e.t.Helper()
	e.addServer("alpha", keysPlain)
	e.addServer("beta", keysPlain+"-b")
	e.addServer("gamma", "")
	e.sql(`update catalog.servers set status = 'disabled' where slug = 'beta'`)
	e.addConnection(keyUserA, "alpha", "oauth", "ready", keysAccess, keysRefresh, true)
	e.addConnection(keyUserA, "beta", "pat", "ready", keysPAT, "", false)
	e.addConnection(keyUserB, "alpha", "oauth", "expired", keysAccess+"-2", "", true)
}

func (e *keysEnv) run(cfg config.Config, args ...string) cliRun {
	e.t.Helper()
	return e.runIn(cfg, "", nil, args...)
}

// rows is a snapshot of what the re-seal must not touch.
func (e *keysEnv) untouched() string {
	e.t.Helper()
	return e.text(`select coalesce((select string_agg(slug || '|' || status || '|' || version || '|' || updated_at::text, ',' order by slug) from catalog.servers), '')
		|| ';' || coalesce((select string_agg(user_id || '|' || server || '|' || kind || '|' || status || '|' || coalesce(expires_at::text, '') || '|' || updated_at::text || '|' || (refresh is not null), ',' order by user_id, server) from connections.connections), '')
		|| ';' || (select count(*) from platform.outbox)`)
}

func (e *keysEnv) audits(action string) []string {
	e.t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, `select meta::text from platform.audit_log where action = $1 and actor_id is null and target is null order by id`, action)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// sameJSON compares two JSON documents as values (jsonb does not keep the key order).
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Fatalf("%q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if !bytes.Equal(ga, gb) {
		t.Errorf("audit meta %s, want %s", got, want)
	}
}

func rotateStdout(servers [2]int, conns [2]int, check string) string {
	return "server secrets: " + itoa(servers[0]) + " re-sealed, " + itoa(servers[1]) + " already under the new key\n" +
		"connections: " + itoa(conns[0]) + " re-sealed, " + itoa(conns[1]) + " already under the new key\n" +
		"key check: " + check + "\n"
}

func itoa(n int) string { return strconv.Itoa(n) }

func (e *keysEnv) opens(box *crypto.AESGCMBox, sealed []byte) (string, bool) {
	p, err := box.Open(sealed)
	return p, err == nil
}

func TestRotateSecretsKeyReSealsServersAndConnections(t *testing.T) {
	e := newKeysEnv(t)
	e.seed()
	before := e.untouched()
	alphaOld := e.bytes(`select secret from catalog.servers where slug = 'alpha'`)
	checkRows := e.count(`select count(*) from platform.key_check`)

	r := e.run(e.rotate, "rotate-secrets-key")
	if r.code != 0 || r.stdout != rotateStdout([2]int{2, 0}, [2]int{3, 0}, "none stored") || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	// Servers: the disabled one too, none for the server without a secret; the new key opens them,
	// the old one opens nothing.
	for slug, want := range map[string]string{"alpha": keysPlain, "beta": keysPlain + "-b"} {
		sealed := e.bytes(`select secret from catalog.servers where slug = $1`, slug)
		if p, ok := e.opens(e.nextBox, sealed); !ok || p != want {
			t.Errorf("%s: the new key does not open it", slug)
		}
		if _, ok := e.opens(e.oldBox, sealed); ok {
			t.Errorf("%s: the old key still opens it", slug)
		}
	}
	if bytes.Equal(alphaOld, e.bytes(`select secret from catalog.servers where slug = 'alpha'`)) {
		t.Error("the secret was not rewritten")
	}
	if e.count(`select count(*) from catalog.servers where slug = 'gamma' and secret is null`) != 1 {
		t.Error("the server without a secret changed")
	}
	// Connections: every part.
	for _, c := range []struct {
		user, server, col, want string
	}{
		{keyUserA, "alpha", "access", keysAccess},
		{keyUserA, "alpha", "refresh", keysRefresh},
		{keyUserA, "beta", "access", keysPAT},
		{keyUserB, "alpha", "access", keysAccess + "-2"},
	} {
		sealed := e.bytes(`select `+c.col+` from connections.connections where user_id = $1::uuid and server = $2`, c.user, c.server)
		if p, ok := e.opens(e.nextBox, sealed); !ok || p != c.want {
			t.Errorf("%s/%s %s: the new key does not open it", c.user, c.server, c.col)
		}
		if _, ok := e.opens(e.oldBox, sealed); ok {
			t.Errorf("%s/%s %s: the old key still opens it", c.user, c.server, c.col)
		}
	}
	// updated_at, version, status, expiry, refresh presence, outbox: unchanged.
	if after := e.untouched(); after != before {
		t.Errorf("a column that must stay changed:\n%s\n%s", before, after)
	}
	// The CLI never creates the key check.
	if e.count(`select count(*) from platform.key_check`) != checkRows {
		t.Error("the CLI created a key check")
	}
	logs := e.audits("secrets.rotate")
	if len(logs) != 1 {
		t.Fatalf("%d audit rows", len(logs))
	}
	sameJSON(t, logs[0], `{"via":"cli","servers":{"resealed":2,"current":0,"failed":0},"connections":{"resealed":3,"current":0,"failed":0},"keyCheck":"absent"}`)

	// A second run finds everything under the new key.
	r = e.run(e.rotate, "rotate-secrets-key")
	if r.code != 0 || r.stdout != rotateStdout([2]int{0, 2}, [2]int{0, 3}, "none stored") || r.stderr != "" {
		t.Fatalf("second run: %+v", r)
	}
	if e.untouched() != before {
		t.Error("the second run changed something")
	}
	logs = e.audits("secrets.rotate")
	if len(logs) != 2 {
		t.Fatalf("%d audit rows", len(logs))
	}
	sameJSON(t, logs[1], `{"via":"cli","servers":{"resealed":0,"current":2,"failed":0},"connections":{"resealed":0,"current":3,"failed":0},"keyCheck":"absent"}`)
	e.noKeyMaterial(r, alphaOld)
}

// Trap 1: the process's own box opens with the old key as well; had the command used it as the
// "new" side, every old secret would count as already current and nothing would be re-sealed.
func TestRotateSecretsKeyReallyReSealsWithTheSingleNewKey(t *testing.T) {
	e := newKeysEnv(t)
	e.addServer("alpha", keysPlain)
	r := e.run(e.rotate, "rotate-secrets-key")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "server secrets: 1 re-sealed, 0 already under the new key\n") {
		t.Fatalf("%+v", r)
	}
	onlyNew, _ := crypto.NewAESGCMBox(e.next, nil)
	if p, ok := e.opens(onlyNew, e.bytes(`select secret from catalog.servers where slug = 'alpha'`)); !ok || p != keysPlain {
		t.Error("the stored secret is not under the new key alone")
	}
}

func (e *keysEnv) setCheck(box *crypto.AESGCMBox) {
	e.t.Helper()
	e.sql(`insert into platform.key_check (sealed) values ($1) on conflict (id) do update set sealed = excluded.sealed`, e.seal(box, keyCheckPlain))
}

func TestRotateSecretsKeyKeyCheckVerdicts(t *testing.T) {
	t.Run("re-sealed, then the new key alone opens it", func(t *testing.T) {
		e := newKeysEnv(t)
		e.setCheck(e.oldBox)
		r := e.run(e.rotate, "rotate-secrets-key")
		if r.code != 0 || r.stdout != rotateStdout([2]int{0, 0}, [2]int{0, 0}, "re-sealed") || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
		keys := func(cur, prev []byte) string {
			v, err := crypto.Inspect(context.Background(), e.pool(), crypto.Keys{Current: cur, Previous: prev})
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		if keys(e.next, nil) != crypto.KeyCheckOK || keys(e.old, nil) != crypto.KeyCheckMismatch {
			t.Error("the check is not under the new key alone")
		}
		sameJSON(t, e.audits("secrets.rotate")[0], `{"via":"cli","servers":{"resealed":0,"current":0,"failed":0},"connections":{"resealed":0,"current":0,"failed":0},"keyCheck":"resealed"}`)
		r = e.run(e.rotate, "rotate-secrets-key")
		if r.code != 0 || r.stdout != rotateStdout([2]int{0, 0}, [2]int{0, 0}, "already current") {
			t.Fatalf("again: %+v", r)
		}
	})
	t.Run("unreadable: partial failure, stdout whole, exit 1, bytes unchanged", func(t *testing.T) {
		e := newKeysEnv(t)
		e.sql(`insert into platform.key_check (sealed) values ($1)`, []byte(notUnderAnyKey))
		r := e.run(e.rotate, "rotate-secrets-key")
		if r.code != 1 || r.stdout != rotateStdout([2]int{0, 0}, [2]int{0, 0}, "cannot be read with either key") ||
			r.stderr != "error: neither key opens (left as they were):\n  key check\n" {
			t.Fatalf("%+v", r)
		}
		if string(e.bytes(`select sealed from platform.key_check`)) != notUnderAnyKey {
			t.Error("the check changed")
		}
		sameJSON(t, e.audits("secrets.rotate")[0], `{"via":"cli","servers":{"resealed":0,"current":0,"failed":0},"connections":{"resealed":0,"current":0,"failed":0},"keyCheck":"unreadable"}`)
	})
}

func (e *keysEnv) pool() *pgxpool.Pool {
	e.t.Helper()
	p, err := pgxpool.New(context.Background(), e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(p.Close)
	return p
}

func TestRotateSecretsKeyUnreadableItemsAreListedAndLeftAlone(t *testing.T) {
	e := newKeysEnv(t)
	e.seed()
	// A server and a connection that neither key opens, and the first server stays good.
	e.sql(`update catalog.servers set secret = $1 where slug = 'beta'`, []byte(notUnderAnyKey))
	e.sql(`update connections.connections set access = $1 where user_id = $2::uuid and server = 'alpha'`, []byte(notUnderAnyKey), keyUserB)
	before := e.untouched()
	r := e.run(e.rotate, "rotate-secrets-key")
	wantErr := "error: neither key opens (left as they were):\n  server beta\n  connection " + keyUserB + "/alpha\n"
	if r.code != 1 || r.stdout != rotateStdout([2]int{1, 0}, [2]int{2, 0}, "none stored") || r.stderr != wantErr {
		t.Fatalf("%+v", r)
	}
	if string(e.bytes(`select secret from catalog.servers where slug = 'beta'`)) != notUnderAnyKey ||
		string(e.bytes(`select access from connections.connections where user_id = $1::uuid and server = 'alpha'`, keyUserB)) != notUnderAnyKey {
		t.Error("an unreadable value was changed")
	}
	if e.untouched() != before {
		t.Error("a column that must stay changed")
	}
	sameJSON(t, e.audits("secrets.rotate")[0], `{"via":"cli","servers":{"resealed":1,"current":0,"failed":1},"connections":{"resealed":2,"current":0,"failed":1},"keyCheck":"absent"}`)
	e.noKeyMaterial(r)
}

func TestRotateSecretsKeyWithAStrangerAsTheOldKey(t *testing.T) {
	e := newKeysEnv(t)
	e.addServer("alpha", keysPlain)
	e.addServer("zeta", keysPlain)
	e.addConnection(keyUserA, "alpha", "pat", "ready", keysPAT, "", false)
	cfg := e.rotate
	cfg.SecretsKeyOld = config.NewSecretBytes(newKey(t))
	r := e.run(cfg, "rotate-secrets-key")
	want := "error: neither key opens (left as they were):\n  server alpha\n  server zeta\n  connection " + keyUserA + "/alpha\n"
	if r.code != 1 || r.stdout != rotateStdout([2]int{0, 0}, [2]int{0, 0}, "none stored") || r.stderr != want {
		t.Fatalf("%+v", r)
	}
	if _, ok := e.opens(e.oldBox, e.bytes(`select secret from catalog.servers where slug = 'alpha'`)); !ok {
		t.Error("the secret was changed")
	}
}

func TestRotateSecretsKeyNeedsTheOldKey(t *testing.T) {
	e := newKeysEnv(t)
	e.seed()
	before := e.untouched()
	r := e.run(e.cfg, "rotate-secrets-key")
	if r.code != 1 || r.stdout != "" || r.stderr != "error: SECRETS_KEY_OLD is not set: put the previous key there, the new one in SECRETS_KEY\n" {
		t.Fatalf("%+v", r)
	}
	if len(e.audits("secrets.rotate")) != 0 || e.untouched() != before {
		t.Error("something happened")
	}
	e.noKeyMaterial(r)
}

func TestRotateSecretsKeyBadArguments(t *testing.T) {
	e := newKeysEnv(t)
	for _, args := range [][]string{{"rotate-secrets-key", "x"}, {"rotate-secrets-key", "--unknown"}} {
		for name, cfg := range map[string]config.Config{"with the old key": e.rotate, "without": e.cfg} {
			r := e.run(cfg, args...)
			if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "error: ") || !strings.Contains(r.stderr, "\nUsage: ") ||
				strings.Contains(r.stderr, "SECRETS_KEY_OLD is not set") {
				t.Errorf("%v %s: %+v", args, name, r)
			}
		}
	}
	if len(e.audits("secrets.rotate")) != 0 {
		t.Error("audited")
	}
}

func TestResetKeyCheck(t *testing.T) {
	e := newKeysEnv(t)
	e.setCheck(e.oldBox)

	// Without --yes: exit 2, the usage, nothing removed, nothing audited.
	r := e.run(e.cfg, "reset-key-check")
	if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "error: --yes is required: reset-key-check removes the stored key check, so the next start records the configured SECRETS_KEY\nUsage: ") {
		t.Fatalf("%+v", r)
	}
	// A parse error wins over the missing --yes.
	r = e.run(e.cfg, "reset-key-check", "x")
	if r.code != 2 || !strings.HasPrefix(r.stderr, "error: expected 0 argument(s), got 1\n") {
		t.Fatalf("%+v", r)
	}
	if e.count(`select count(*) from platform.key_check`) != 1 || len(e.audits("secrets.key_check_reset")) != 0 {
		t.Fatal("something happened without --yes")
	}

	r = e.run(e.cfg, "reset-key-check", "--yes")
	if r.code != 0 || r.stdout != "key check removed: the next start records the configured SECRETS_KEY\n" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	if e.count(`select count(*) from platform.key_check`) != 0 {
		t.Error("the check is still there")
	}
	// The next start records the configured key again.
	if v, err := crypto.Verify(context.Background(), e.pool(), crypto.Keys{Current: e.next}); err != nil || v != crypto.KeyCheckCreated {
		t.Errorf("verify: %q %v", v, err)
	}
	e.sql(`delete from platform.key_check`)

	r = e.run(e.cfg, "reset-key-check", "--yes")
	if r.code != 0 || r.stdout != "key check: nothing to remove\n" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	logs := e.audits("secrets.key_check_reset")
	if len(logs) != 2 {
		t.Fatalf("%d audit rows", len(logs))
	}
	sameJSON(t, logs[0], `{"via":"cli","removed":true}`)
	sameJSON(t, logs[1], `{"via":"cli","removed":false}`)
}

// A mismatching key: the guard does not run for either command, so no `warning:` line, and the
// CLI does not create the check.
func TestKeyCommandsDoNotWarnOnAMismatch(t *testing.T) {
	e := newKeysEnv(t)
	e.setCheck(e.oldBox)
	stranger := e.cfg
	stranger.SecretsKey = config.NewSecretBytes(newKey(t))
	r := e.run(stranger, "reset-key-check")
	if r.code != 2 || strings.Contains(r.stderr, "warning:") {
		t.Errorf("reset without --yes: %+v", r)
	}
	stranger.SecretsKeyOld = config.NewSecretBytes(newKey(t))
	r = e.run(stranger, "rotate-secrets-key")
	if r.code != 1 || strings.Contains(r.stderr, "warning:") || !strings.HasPrefix(r.stderr, "error: neither key opens") {
		t.Errorf("rotate: %+v", r)
	}
	r = e.run(stranger, "reset-key-check", "--yes")
	if r.code != 0 || r.stderr != "" {
		t.Errorf("reset: %+v", r)
	}
	if e.count(`select count(*) from platform.key_check`) != 0 {
		t.Error("the check is still there, or the CLI created one")
	}
}

// noKeyMaterial: no key, secret or ciphertext in the output, the log (stderr) or the audit, in
// the clear, hex or base64.
func (e *keysEnv) noKeyMaterial(r cliRun, extraSealed ...[]byte) {
	e.t.Helper()
	audit := e.text(`select coalesce(string_agg(action || coalesce(target, '') || meta::text, ' '), '') from platform.audit_log`)
	haystack := r.stdout + r.stderr + audit
	needles := [][]byte{e.old, e.next, []byte(keysPlain), []byte(keysAccess), []byte(keysRefresh), []byte(keysPAT)}
	needles = append(needles, extraSealed...)
	for _, col := range []string{
		`select secret from catalog.servers where secret is not null`,
		`select access from connections.connections`,
	} {
		ctx := context.Background()
		c, err := pgx.Connect(ctx, e.url)
		if err != nil {
			e.t.Fatal(err)
		}
		rows, err := c.Query(ctx, col)
		if err != nil {
			e.t.Fatal(err)
		}
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				e.t.Fatal(err)
			}
			needles = append(needles, b)
		}
		rows.Close()
		_ = c.Close(ctx)
	}
	for _, n := range needles {
		for _, form := range []string{
			string(n), hex.EncodeToString(n), strings.ToUpper(hex.EncodeToString(n)),
			base64.StdEncoding.EncodeToString(n), base64.RawStdEncoding.EncodeToString(n),
			base64.URLEncoding.EncodeToString(n), base64.RawURLEncoding.EncodeToString(n),
		} {
			if len(form) >= 8 && strings.Contains(haystack, form) {
				e.t.Fatalf("key material or a secret reached the output, the log or the audit (%d characters)", len(form))
			}
		}
	}
}
