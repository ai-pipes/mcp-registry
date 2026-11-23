package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

func wantOK(t *testing.T, r app.CredentialResult, err error, secret string) {
	t.Helper()
	if err != nil || r.Failure != "" || r.Secret != secret {
		t.Fatalf("got (failure %q, err %v), want the secret %q", r.Failure, err, secret)
	}
}

func wantFailure(t *testing.T, r app.CredentialResult, err error, f app.CredentialFailure) {
	t.Helper()
	if err != nil || r.Failure != f || r.Secret != "" {
		t.Fatalf("got (failure %q, secret set %v, err %v), want failure %q", r.Failure, r.Secret != "", err, f)
	}
}

func (h *harness) status(user, server string) domain.Status {
	h.t.Helper()
	s, ok := h.store.snap(user, server)
	if !ok {
		h.t.Fatalf("no connection %s/%s", user, server)
	}
	return s.Status
}

func TestResolveNoConnection(t *testing.T) {
	h := newHarness(t)
	r, err := h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureNeedsConnection)
}

func TestResolveFreshTokenIsUsedWithoutRefreshing(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
	r, err := h.resolve("alice", "gitea")
	wantOK(t, r, err, "A1")
	if got := h.oauth.refreshed(); len(got) != 0 {
		t.Fatalf("refreshed: %v", got)
	}
	// A "use" plan takes neither the lock nor the single flight.
	if slices.Contains(h.j.all(), "lock.begin") {
		t.Fatalf("a fresh token took the lock: %v", h.j.all())
	}
}

func TestResolveRefreshesAheadAndStoresSealed(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(30_000)), "R1", "alice")
	h.oauth.next = app.TokenSet{AccessToken: "A2", ExpiresAt: tp(at(3_600_000))}
	r, err := h.resolve("alice", "gitea")
	wantOK(t, r, err, "A2")
	if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1"}) {
		t.Fatalf("refreshed: %v", got)
	}
	row, _ := h.store.snap("alice", "gitea")
	if bytes.Contains(row.Access, []byte("A2")) {
		t.Fatal("the new access token is stored in plain text")
	}
	if got, err := h.box.Open(row.Access); err != nil || got != "A2" {
		t.Fatalf("stored access: %q %v", got, err)
	}
	// Hub and Gitea do not rotate by default: the old refresh token is kept.
	if got, err := h.box.Open(row.Refresh); err != nil || got != "R1" {
		t.Fatalf("stored refresh: %q %v", got, err)
	}
	if row.ExpiresAt == nil || !row.ExpiresAt.Equal(at(3_600_000)) {
		t.Fatalf("expiry: %v", row.ExpiresAt)
	}
	// Fresh now: no second refresh.
	r2, err := h.resolve("alice", "gitea")
	wantOK(t, r2, err, "A2")
	if len(h.oauth.refreshed()) != 1 {
		t.Fatal("refreshed twice")
	}
}

func TestResolveSequenceConfigBeforeLockProviderInsideAuditAfter(t *testing.T) {
	h := newHarness(t)

	// success: config read, then the secret, the config read again (it must not have moved), then the lock with the provider call and the write
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.j.reset()
	h.oauth.next = app.TokenSet{AccessToken: "A2", RefreshToken: "R2", ExpiresAt: tp(at(3_600_000))}
	if r, err := h.resolve("alice", "gitea"); err != nil || r.Failure != "" {
		t.Fatal(r.Failure, err)
	}
	want := []string{"store.get", "servers.config", "servers.client_secret", "servers.config", "lock.begin", "oauth.refresh", "lock.write", "lock.end"}
	if got := h.j.all(); !slices.Equal(got, want) {
		t.Fatalf("success sequence:\n got %v\nwant %v", got, want)
	}

	// refresh rejected: the audit record follows the end of the lock
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.j.reset()
	h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) {
		return app.TokenSet{}, &app.TokenEndpointError{Status: 400, Code: "invalid_grant"}
	}
	if r, err := h.resolve("alice", "gitea"); err != nil || r.Failure != app.FailureExpired {
		t.Fatal(r.Failure, err)
	}
	want = []string{"store.get", "servers.config", "servers.client_secret", "servers.config", "lock.begin", "oauth.refresh", "lock.write", "lock.end", "audit:connection.expired"}
	if got := h.j.all(); !slices.Equal(got, want) {
		t.Fatalf("rejected sequence:\n got %v\nwant %v", got, want)
	}

	// config changed: nothing is sent to the provider, the expiry is audited after the lock
	h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
	h.moveGitea()
	h.oauth.mu.Lock()
	h.oauth.refreshCalls = nil
	h.oauth.mu.Unlock()
	h.j.reset()
	if r, err := h.resolve("alice", "gitea"); err != nil || r.Failure != app.FailureExpired {
		t.Fatal(r.Failure, err)
	}
	want = []string{"store.get", "servers.config", "servers.client_secret", "servers.config", "lock.begin", "lock.write", "lock.end", "audit:connection.expired"}
	if got := h.j.all(); !slices.Equal(got, want) {
		t.Fatalf("config-changed sequence:\n got %v\nwant %v", got, want)
	}
	h.noLockViolations()
}

func TestResolveNothingButTheRowAndTheProviderUnderTheLock(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.oauth.next = app.TokenSet{AccessToken: "A2", ExpiresAt: tp(at(3_600_000))}
	if r, err := h.resolve("alice", "gitea"); err != nil || r.Failure != "" {
		t.Fatal(r.Failure, err)
	}
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) {
		return app.TokenSet{}, &app.TokenEndpointError{Status: 400, Code: "invalid_grant"}
	}
	if r, err := h.resolve("alice", "gitea"); err != nil || r.Failure != app.FailureExpired {
		t.Fatal(r.Failure, err)
	}
	h.seedOAuth(tp(at(-1)), "", "alice")
	if r, err := h.resolve("alice", "gitea"); err != nil || r.Failure != app.FailureExpired {
		t.Fatal(r.Failure, err)
	}
	if _, err := h.resolve("alice", "gitea"); err != nil {
		t.Fatal(err)
	}
	if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "yt"}); err != nil {
		t.Fatal(err)
	}
	h.oauth.refreshFn = nil
	h.connect(alice, "gitea")
	if err := app.Disconnect(ctx, h.d, app.DisconnectCommand{Principal: alice, Server: "gitea"}); err != nil {
		t.Fatal(err)
	}
	h.noLockViolations()
	if n := len(h.audit.expired()); n != 3 {
		t.Fatalf("connection.expired records: %d, want 3", n)
	}
}

func TestResolveParallelCallsOneOneTimeRefreshToken(t *testing.T) {
	const n = 12
	h := newHarness(t)
	h.v.setStrict(false) // calls overlap on purpose
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	// A provider whose refresh tokens are one-time: R1 works once, then it is gone.
	var mu sync.Mutex
	used := map[string]bool{}
	h.oauth.refreshFn = func(_ context.Context, rt string) (app.TokenSet, error) {
		mu.Lock()
		defer mu.Unlock()
		if used[rt] {
			return app.TokenSet{}, &app.TokenEndpointError{Status: 400, Code: "invalid_grant"}
		}
		used[rt] = true
		return app.TokenSet{AccessToken: "A2", RefreshToken: "R2", ExpiresAt: tp(at(3_600_000))}, nil
	}
	gate := make(chan struct{})
	h.oauth.gate = gate
	h.oauth.entered = make(chan string, n)
	configs := make(chan struct{}, n)
	h.dir.onConfig = func() { configs <- struct{}{} }

	results := make([]app.CredentialResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = h.resolve("alice", "gitea")
		}()
	}
	for range n {
		<-configs // every caller has read the connection and the config
	}
	<-h.oauth.entered // the one refresh is under way
	close(gate)
	wg.Wait()

	for i := range n {
		wantOK(t, results[i], errs[i], "A2")
	}
	if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1"}) {
		t.Fatalf("provider calls: %v, want exactly one with R1", got)
	}
	row, _ := h.store.snap("alice", "gitea")
	if got, _ := h.box.Open(row.Refresh); got != "R2" {
		t.Fatalf("stored refresh token: %q", got)
	}
	if h.status("alice", "gitea") != domain.StatusReady || len(h.audit.expired()) != 0 {
		t.Fatal("the connection was expired by a lost race")
	}
}

func TestResolveTwoProcessesTheRowLockLetsOneRefresh(t *testing.T) {
	h := newHarness(t)
	h.v.setStrict(false) // calls overlap on purpose
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.oauth.next = app.TokenSet{AccessToken: "A2", RefreshToken: "R2", ExpiresAt: tp(at(3_600_000))}
	gate := make(chan struct{})
	h.oauth.gate = gate
	h.oauth.entered = make(chan string, 4)
	h.j.reset()
	replica := h.replica()

	var wg sync.WaitGroup
	results := make([]app.CredentialResult, 2)
	errs := make([]error, 2)
	for i, d := range []app.Deps{h.d, replica} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = app.ResolveWithoutSending(ctx, d, app.ResolveQuery{UserID: "alice", Server: "gitea"})
		}()
	}
	// Both read the stored row before the first one (held in the provider call) writes. A caller
	// may read the configuration more than once, so wait on the row reads, not on those.
	<-h.oauth.entered
	for deadline := time.Now().Add(10 * time.Second); countOf(h.j.all(), "store.get") < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the second caller never read the row")
		}
		runtime.Gosched()
	}
	close(gate)
	wg.Wait()
	wantOK(t, results[0], errs[0], "A2")
	wantOK(t, results[1], errs[1], "A2")
	if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1"}) {
		t.Fatalf("provider calls: %v", got)
	}
}

func TestResolveForceRefresh(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
	h.oauth.next = app.TokenSet{AccessToken: "A2", ExpiresAt: tp(at(3_600_000))}
	r, err := h.resolveForced("alice", "gitea")
	wantOK(t, r, err, "A2")
	if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1"}) {
		t.Fatalf("refreshed: %v", got)
	}

	h.v.setStrict(false) // the second half overlaps calls on purpose
	// Another process read A2's row before, then waits on the lock while this one refreshes again:
	// it finds the row changed after it read it and takes the new token instead of refreshing a
	// third time.
	h.clock.set(at(1000))
	h.oauth.next = app.TokenSet{AccessToken: "A3", ExpiresAt: tp(at(3_600_000))}
	gate := make(chan struct{})
	h.oauth.gate = gate
	h.oauth.entered = make(chan string, 4)
	h.dir.onConfig = nil
	h.j.reset()
	replica := h.replica()
	var wg sync.WaitGroup
	results := make([]app.CredentialResult, 2)
	errs := make([]error, 2)
	for i, d := range []app.Deps{h.d, replica} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = app.ResolveWithoutSending(ctx, d, app.ResolveQuery{UserID: "alice", Server: "gitea", ForceRefresh: true})
		}()
	}
	// Both read the stored row before the first one (held in the provider call) writes A3. Each
	// caller reads the configuration twice, so waiting on those reads would let one caller supply
	// both; the row reads are one per caller (journalled once the snapshot is taken).
	<-h.oauth.entered
	for deadline := time.Now().Add(10 * time.Second); countOf(h.j.all(), "store.get") < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the second caller never read the row")
		}
		runtime.Gosched()
	}
	close(gate)
	wg.Wait()
	wantOK(t, results[0], errs[0], "A3")
	wantOK(t, results[1], errs[1], "A3")
	// The first forced refresh plus one more: the loser of the race did not refresh again.
	if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1", "R1"}) {
		t.Fatalf("provider calls: %v", got)
	}
}

func TestResolveRefreshRejectedExpiresAndStaysExpired(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) {
		return app.TokenSet{}, &app.TokenEndpointError{Status: 400, Code: "invalid_grant"}
	}
	r, err := h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureExpired)
	if h.status("alice", "gitea") != domain.StatusExpired {
		t.Fatal("not expired")
	}
	recs := h.audit.expired()
	if len(recs) != 1 || *recs[0].ActorID != "alice" || *recs[0].Target != "gitea" || recs[0].Meta["reason"] != "refresh_rejected" || len(recs[0].Meta) != 1 {
		t.Fatalf("audit: %+v", h.audit.text())
	}
	h.oauth.refreshFn = nil
	r, err = h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureExpired)
	if len(h.oauth.refreshed()) != 1 || len(h.audit.expired()) != 1 {
		t.Fatal("an expired connection was refreshed or audited again")
	}
}

func TestResolveExpiredWithoutRefreshTokenNeedsNoProvider(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1)), "", "alice")
	r, err := h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureExpired)
	if h.status("alice", "gitea") != domain.StatusExpired || len(h.oauth.refreshed()) != 0 {
		t.Fatal("wrong state or a provider call")
	}
	if recs := h.audit.expired(); len(recs) != 1 || recs[0].Meta["reason"] != "token_expired" {
		t.Fatalf("audit: %s", h.audit.text())
	}
}

func TestResolveDisabledServerLeavesTheConnectionAlone(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1)), "R1", "alice")
	h.gitea().disabled = true
	r, err := h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureNeedsConnection)
	if h.status("alice", "gitea") != domain.StatusReady {
		t.Fatal("the connection was touched")
	}
}

func TestResolveConnectionGoneWhileWaitingForTheLock(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1)), "R1", "alice")
	h.store.beforeRead = func(userID, server string) {
		h.store.mu.Lock()
		delete(h.store.row, key(userID, server))
		h.store.mu.Unlock()
	}
	r, err := h.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureNeedsConnection)
}

func TestResolveConnectionsArePersonal(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
	r, err := h.resolve("bob", "gitea")
	wantFailure(t, r, err, app.FailureNeedsConnection)
}

func TestResolveProviderAnswersAndNoAnswers(t *testing.T) {
	tokenErr := func(status int, code string) error { return &app.TokenEndpointError{Status: status, Code: code} }

	t.Run("no answer: unavailable, nothing saved, retried later with the same token", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(-1000)), "R1", "alice")
		before, _ := h.store.snap("alice", "gitea")
		h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) {
			return app.TokenSet{}, errors.New("fetch failed")
		}
		r, err := h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureUnavailable)
		after, _ := h.store.snap("alice", "gitea")
		if !snapshotsEqual(before, after) {
			t.Fatal("the connection changed")
		}
		if len(h.audit.expired()) != 0 {
			t.Fatal("expiry audited")
		}
		h.oauth.refreshFn = nil
		h.oauth.next = app.TokenSet{AccessToken: "A2", ExpiresAt: tp(at(3_600_000))}
		r, err = h.resolve("alice", "gitea")
		wantOK(t, r, err, "A2")
		if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1", "R1"}) {
			t.Fatalf("calls: %v", got)
		}
	})

	t.Run("provider down inside the refresh-ahead window: the still-valid token is used", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(30_000)), "R1", "alice")
		h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) { return app.TokenSet{}, tokenErr(503, "") }
		r, err := h.resolve("alice", "gitea")
		wantOK(t, r, err, "A1")
		// Once it has run out, or when upstream has just rejected it, there is nothing to fall back on.
		h.clock.set(at(30_000))
		r, err = h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureUnavailable)
		h.clock.set(t0)
		r, err = h.resolveForced("alice", "gitea")
		wantFailure(t, r, err, app.FailureUnavailable)
		if h.status("alice", "gitea") != domain.StatusReady {
			t.Fatal("not ready")
		}
	})

	t.Run("the gateway's own client credentials refused: unavailable, nobody expires", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(-1000)), "R1", "alice")
		before, _ := h.store.snap("alice", "gitea")
		for _, e := range []error{tokenErr(401, "invalid_client"), tokenErr(400, "invalid_client"), tokenErr(401, "unauthorized_client")} {
			h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) { return app.TokenSet{}, e }
			r, err := h.resolve("alice", "gitea")
			wantFailure(t, r, err, app.FailureUnavailable)
		}
		after, _ := h.store.snap("alice", "gitea")
		if !snapshotsEqual(before, after) || len(h.audit.expired()) != 0 {
			t.Fatal("changed or audited")
		}
		h.oauth.refreshFn = nil
		h.oauth.next = app.TokenSet{AccessToken: "A2", ExpiresAt: tp(at(3_600_000))}
		r, err := h.resolve("alice", "gitea")
		wantOK(t, r, err, "A2")
	})

	t.Run("a 5xx without an OAuth error is unavailable; 401 expires", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(-1000)), "R1", "alice")
		h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) { return app.TokenSet{}, tokenErr(503, "") }
		r, err := h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureUnavailable)
		if h.status("alice", "gitea") != domain.StatusReady {
			t.Fatal("expired on a 503")
		}
		h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) { return app.TokenSet{}, tokenErr(401, "") }
		r, err = h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureExpired)
		if h.status("alice", "gitea") != domain.StatusExpired {
			t.Fatal("not expired")
		}
		if recs := h.audit.expired(); len(recs) != 1 || recs[0].Meta["reason"] != "refresh_rejected" {
			t.Fatalf("audit: %s", h.audit.text())
		}
	})

	t.Run("a wrapped token endpoint error counts as an answer", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(-1000)), "R1", "alice")
		h.oauth.refreshFn = func(context.Context, string) (app.TokenSet, error) {
			return app.TokenSet{}, fmt.Errorf("refresh: %w", tokenErr(400, "invalid_grant"))
		}
		r, err := h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureExpired)
	})
}

func snapshotsEqual(a, b domain.Snapshot) bool {
	return a.UserID == b.UserID && a.Server == b.Server && a.IssuedFor == b.IssuedFor && a.Kind == b.Kind && a.Status == b.Status &&
		bytes.Equal(a.Access, b.Access) && bytes.Equal(a.Refresh, b.Refresh) && a.UpdatedAt.Equal(b.UpdatedAt) &&
		((a.ExpiresAt == nil) == (b.ExpiresAt == nil)) && (a.ExpiresAt == nil || a.ExpiresAt.Equal(*b.ExpiresAt))
}

func TestResolveKeyMismatch(t *testing.T) {
	t.Run("an access token no key opens is an error", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
		h.box.failOpenOf("A1")
		r, err := h.resolve("alice", "gitea")
		if err == nil || r.Secret != "" || r.Failure != "" {
			t.Fatalf("got (%q, %q, %v), want an error", r.Failure, r.Secret, err)
		}
	})

	t.Run("a refresh token no key opens counts as no answer", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(-1000)), "R1", "alice")
		h.box.failOpenOf("R1")
		r, err := h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureUnavailable)
		if len(h.oauth.refreshed()) != 0 || h.status("alice", "gitea") != domain.StatusReady || len(h.audit.expired()) != 0 {
			t.Fatal("the provider was called or the connection changed")
		}
		// Within the window the valid access token still serves.
		h.seedOAuth(tp(at(30_000)), "R1", "alice")
		r, err = h.resolve("alice", "gitea")
		wantOK(t, r, err, "A1")
	})

	t.Run("a failing lock, store or directory read is an error", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(-1000)), "R1", "alice")
		h.store.failLock = errors.New("db down")
		if _, err := h.resolve("alice", "gitea"); err == nil {
			t.Fatal("no error")
		}
		h.store.failLock = nil
		h.dir.secretErr = errors.New("key mismatch")
		if _, err := h.resolve("alice", "gitea"); err == nil {
			t.Fatal("no error from the client secret")
		}
		h.dir.secretErr = nil
		h.store.failGet = errors.New("db down")
		if _, err := h.resolve("alice", "gitea"); err == nil {
			t.Fatal("no error from get")
		}
	})
}

func TestResolveConfigChange(t *testing.T) {
	t.Run("the token is not handed out: the connection expires, nothing is refreshed", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
		h.moveGitea()
		r, err := h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureExpired)
		if h.status("alice", "gitea") != domain.StatusExpired || len(h.oauth.refreshed()) != 0 {
			t.Fatal("state or provider call")
		}
		recs := h.audit.expired()
		if len(recs) != 1 || *recs[0].ActorID != "alice" || *recs[0].Target != "gitea" || recs[0].Meta["reason"] != "config_changed" {
			t.Fatalf("audit: %s", h.audit.text())
		}
		// A second call writes no second record.
		if r, err = h.resolve("alice", "gitea"); err != nil || r.Failure != app.FailureExpired || len(h.audit.expired()) != 1 {
			t.Fatal("audited twice")
		}
	})

	t.Run("a new token URL, a new client id, or a PAT server switched to OAuth count the same", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
		c := *h.gitea().oauth
		c.TokenURL = "https://evil.example.com/token"
		h.gitea().oauth = &c
		r, err := h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureExpired)

		h2 := newHarness(t)
		h2.seedOAuth(tp(at(3_600_000)), "R1", "alice")
		c2 := *h2.gitea().oauth
		c2.ClientID = "other"
		h2.gitea().oauth = &c2
		r, err = h2.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureExpired)

		if err := app.SavePAT(ctx, h2.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err != nil {
			t.Fatal(err)
		}
		yt := h2.dir.find("yt")
		yt.kind = catalog.AuthOAuthUser
		o := giteaClient
		yt.oauth = &o
		r, err = h2.resolve("alice", "yt")
		wantFailure(t, r, err, app.FailureExpired)
	})

	t.Run("scopes or a revoke URL alone expire nobody", func(t *testing.T) {
		h := newHarness(t)
		h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
		c := *h.gitea().oauth
		c.Scopes = []string{"repo"}
		c.RevokeURL = sp("https://elsewhere.example.com/revoke")
		h.gitea().oauth = &c
		r, err := h.resolve("alice", "gitea")
		wantOK(t, r, err, "A1")
	})

	t.Run("an OAuth connection bound to a config without an OAuth client is left alone", func(t *testing.T) {
		h := newHarness(t)
		h.gitea().oauth = nil
		h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
			UserID: "alice", Server: "gitea", IssuedFor: h.fingerprintOf(h.gitea()), Now: t0,
			Tokens: domain.SealedTokens{Access: mustSeal(t, h.box, "A1"), Refresh: mustSeal(t, h.box, "R1"), ExpiresAt: tp(at(-1))},
		}))
		r, err := h.resolve("alice", "gitea")
		wantFailure(t, r, err, app.FailureNeedsConnection)
		if len(h.oauth.refreshed()) != 0 || h.status("alice", "gitea") != domain.StatusReady {
			t.Fatal("touched")
		}
	})
}

func TestResolvePAT(t *testing.T) {
	h := newHarness(t)
	if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: padded}); err != nil {
		t.Fatal(err)
	}
	r, err := h.resolve("alice", "yt")
	wantOK(t, r, err, "perm:xyz")
	// A PAT is never refreshed, also when forced; the plan is "use".
	r, err = h.resolveForced("alice", "yt")
	wantOK(t, r, err, "perm:xyz")
	if len(h.oauth.refreshed()) != 0 {
		t.Fatal("a PAT was refreshed")
	}
}

func TestMarkExpired(t *testing.T) {
	h := newHarness(t)
	if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err != nil {
		t.Fatal(err)
	}
	h.audit.mu.Lock()
	h.audit.list = nil
	h.audit.mu.Unlock()
	if err := app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "yt"}); err != nil {
		t.Fatal(err)
	}
	r, err := h.resolve("alice", "yt")
	wantFailure(t, r, err, app.FailureExpired)
	recs := h.audit.expired()
	if len(recs) != 1 || recs[0].Meta["reason"] != "rejected_upstream" || *recs[0].ActorID != "alice" || *recs[0].Target != "yt" {
		t.Fatalf("audit: %s", h.audit.text())
	}
	// Already expired and unknown connections change nothing and write nothing.
	if err := app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "yt"}); err != nil {
		t.Fatal(err)
	}
	if err := app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "bob", Server: "yt"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.snap("bob", "yt"); ok {
		t.Fatal("a connection appeared")
	}
	if len(h.audit.expired()) != 1 {
		t.Fatal("audited again")
	}
	// The expiry is stamped with the clock.
	row, _ := h.store.snap("alice", "yt")
	if !row.UpdatedAt.Equal(t0) {
		t.Fatalf("updatedAt %v", row.UpdatedAt)
	}
	h.noLockViolations()
}

func TestMarkExpiredStoreError(t *testing.T) {
	h := newHarness(t)
	h.store.failLock = errors.New("db down")
	if err := app.MarkExpired(ctx, h.d, app.MarkExpiredCommand{UserID: "alice", Server: "yt"}); err == nil {
		t.Fatal("no error")
	}
}

func TestResolveAuditErrorPropagates(t *testing.T) {
	h := newHarness(t)
	h.seedOAuth(tp(at(-1)), "", "alice")
	h.audit.err = errors.New("audit down")
	if _, err := h.resolve("alice", "gitea"); err == nil {
		t.Fatal("no error")
	}
	// The expiry itself was committed before the audit.
	if h.status("alice", "gitea") != domain.StatusExpired {
		t.Fatal("not expired")
	}
}

func TestResolveDifferentPeopleDoNotBlockEachOther(t *testing.T) {
	h := newHarness(t)
	h.v.setStrict(false) // calls overlap on purpose
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.seedOAuth(tp(at(-1000)), "R1", "bob")
	aliceIn := make(chan struct{})
	aliceGo := make(chan struct{})
	h.oauth.refreshFn = func(_ context.Context, _ string) (app.TokenSet, error) {
		return app.TokenSet{AccessToken: "A2", ExpiresAt: tp(at(3_600_000))}, nil
	}
	// Hold the first refresh to arrive (alice's: bob starts after alice is inside).
	var first atomic.Bool
	held := h.oauth.refreshFn
	h.oauth.refreshFn = func(c context.Context, rt string) (app.TokenSet, error) {
		if first.CompareAndSwap(false, true) {
			close(aliceIn)
			<-aliceGo
		}
		return held(c, rt)
	}
	done := make(chan app.CredentialResult, 1)
	go func() {
		r, _ := h.resolve("alice", "gitea")
		done <- r
	}()
	<-aliceIn
	// alice is inside the provider call under her lock; bob is another key and must not wait.
	r, err := h.resolve("bob", "gitea")
	wantOK(t, r, err, "A2")
	close(aliceGo)
	wantOK(t, <-done, nil, "A2")
}

func TestResolveCancelledCallerReturnsWhileTheRefreshCompletes(t *testing.T) {
	h := newHarness(t)
	h.v.setStrict(false) // calls overlap on purpose
	h.seedOAuth(tp(at(-1000)), "R1", "alice")
	h.oauth.next = app.TokenSet{AccessToken: "A2", RefreshToken: "R2", ExpiresAt: tp(at(3_600_000))}
	gate := make(chan struct{})
	h.oauth.gate = gate
	h.oauth.entered = make(chan string, 4)

	cctx, cancel := context.WithCancel(ctx)
	returned := make(chan error, 1)
	go func() {
		_, err := app.ResolveWithoutSending(cctx, h.d, app.ResolveQuery{UserID: "alice", Server: "gitea"})
		returned <- err
	}()
	<-h.oauth.entered // the refresh is under way, started by the caller above
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller returned %v, want context.Canceled", err)
	}
	// The provider call was not cancelled with the caller: it finishes and the tokens are stored,
	// so the one-time refresh token it used is not lost.
	close(gate)
	var r app.CredentialResult
	var err error
	for {
		// The detached refresh settles on its own; resolving again either joins it or finds its row.
		r, err = h.resolve("alice", "gitea")
		if err != nil || r.Failure != "" {
			break
		}
		if row, _ := h.store.snap("alice", "gitea"); row.ExpiresAt != nil && row.ExpiresAt.Equal(at(3_600_000)) {
			break
		}
	}
	wantOK(t, r, err, "A2")
	if got := h.oauth.refreshed(); !slices.Equal(got, []string{"R1"}) {
		t.Fatalf("provider calls: %v", got)
	}
	row, _ := h.store.snap("alice", "gitea")
	if got, _ := h.box.Open(row.Refresh); got != "R2" {
		t.Fatalf("stored refresh token %q", got)
	}
}

// addOAuthServer registers a second OAuth server and seeds an expired connection of user to it.
func (h *harness) seedOther(user, server, access, refresh string) {
	h.t.Helper()
	if h.dir.find(server) == nil {
		oa := giteaClient
		h.dir.servers = append(h.dir.servers, &fakeServer{slug: server, name: server, kind: catalog.AuthOAuthUser, url: "https://mcp.example.com/" + server, oauth: &oa, clientSecret: "s", hasSecret: true})
	}
	h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
		UserID: user, Server: server, IssuedFor: h.fingerprintOf(h.dir.find(server)), Now: t0,
		Tokens: domain.SealedTokens{Access: mustSeal(h.t, h.box, access), Refresh: mustSeal(h.t, h.box, refresh), ExpiresAt: tp(at(-1000))},
	}))
}

// bothInFlight runs two resolves at once, holds both provider calls until both are under way (a
// renewal key that merged them would never get the second one there) and returns the secrets.
func bothInFlight(t *testing.T, h *harness, a, b app.ResolveQuery) (string, string) {
	t.Helper()
	h.v.setStrict(false)
	h.oauth.entered = make(chan string, 4)
	gate := make(chan struct{})
	h.oauth.gate = gate
	h.oauth.refreshFn = func(_ context.Context, rt string) (app.TokenSet, error) {
		return app.TokenSet{AccessToken: "new-" + rt, ExpiresAt: tp(at(3_600_000))}, nil
	}
	type out struct {
		r   app.CredentialResult
		err error
	}
	ch := make([]chan out, 2)
	for i, q := range []app.ResolveQuery{a, b} {
		ch[i] = make(chan out, 1)
		go func() {
			r, err := app.ResolveWithoutSending(ctx, h.d, q)
			ch[i] <- out{r, err}
		}()
	}
	for range 2 {
		select {
		case <-h.oauth.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the two renewals were merged into one flight")
		}
	}
	close(gate)
	var got [2]string
	for i := range ch {
		o := <-ch[i]
		if o.err != nil || o.r.Failure != "" {
			t.Fatal(o.r.Failure, o.err)
		}
		got[i] = o.r.Secret
	}
	return got[0], got[1]
}

func TestResolveOnePersonTwoServersEachGetsItsOwnToken(t *testing.T) {
	h := newHarness(t)
	h.seedOther("alice", "s1", "a1", "r-s1")
	h.seedOther("alice", "s2", "a2", "r-s2")
	g1, g2 := bothInFlight(t, h, app.ResolveQuery{UserID: "alice", Server: "s1"}, app.ResolveQuery{UserID: "alice", Server: "s2"})
	if g1 != "new-r-s1" || g2 != "new-r-s2" {
		t.Fatalf("got %q and %q: a token was handed out for the wrong server", g1, g2)
	}
}

func TestResolveTwoPeopleOneServerEachGetsTheirOwnToken(t *testing.T) {
	h := newHarness(t)
	h.seedOther("alice", "s1", "a1", "r-alice")
	h.seedOther("bob", "s1", "b1", "r-bob")
	g1, g2 := bothInFlight(t, h, app.ResolveQuery{UserID: "alice", Server: "s1"}, app.ResolveQuery{UserID: "bob", Server: "s1"})
	if g1 != "new-r-alice" || g2 != "new-r-bob" {
		t.Fatalf("got %q and %q: a token was handed out to the wrong person", g1, g2)
	}
}
