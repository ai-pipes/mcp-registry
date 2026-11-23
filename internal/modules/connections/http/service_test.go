package http_test

// Tests of the module's root package (connections.Service). They live here and not next to it
// because the architecture rules keep the root away from the catalog and identity packages, which
// the fakes of the ports need.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

var ctx = context.Background()

func (e *env) service(t testing.TB) *connections.Service {
	t.Helper()
	svc, err := connections.New(e.deps)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func fingerprint(c app2Config) string {
	o := domain.ServerConfigOrigin{URL: c.URL, AuthKind: string(c.AuthKind)}
	if c.OAuth != nil {
		o.OAuth = &domain.OAuthEndpoints{AuthorizeURL: c.OAuth.AuthorizeURL, TokenURL: c.OAuth.TokenURL, ClientID: c.OAuth.ClientID}
	}
	return domain.ServerFingerprint(o)
}

type app2Config = connections.ServerConfig

func (e *env) seedPAT(user, server, token string) {
	sealed, _ := box{1}.Seal(token)
	cfg, _ := e.dir.Config(ctx, server)
	c := domain.ConnectPAT(domain.PATConnect{UserID: user, Server: server, Token: domain.Sealed(sealed), IssuedFor: fingerprint(*cfg), Now: t0})
	e.store.rows[user+"\x00"+server] = c.Snapshot()
}

func (e *env) seedOAuth(user string, expiresIn time.Duration) {
	a, _ := box{1}.Seal("ACCESS-OLD")
	r, _ := box{1}.Seal("REFRESH-OLD")
	exp := t0.Add(expiresIn)
	cfg, _ := e.dir.Config(ctx, "gitea")
	c := domain.ConnectOAuth(domain.OAuthConnect{
		UserID: user, Server: "gitea", Tokens: domain.SealedTokens{Access: domain.Sealed(a), Refresh: domain.Sealed(r), ExpiresAt: &exp},
		IssuedFor: fingerprint(*cfg), Now: t0,
	})
	e.store.rows[user+"\x00gitea"] = c.Snapshot()
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	for name, mutate := range map[string]func(*connections.Deps){
		"Connections": func(d *connections.Deps) { d.Connections = nil },
		"Pending":     func(d *connections.Deps) { d.Pending = nil },
		"Servers":     func(d *connections.Deps) { d.Servers = nil },
		"OAuth":       func(d *connections.Deps) { d.OAuth = nil },
		"Pkce":        func(d *connections.Deps) { d.Pkce = nil },
		"Box":         func(d *connections.Deps) { d.Box = nil },
		"Clock":       func(d *connections.Deps) { d.Clock = nil },
		"Audit":       func(d *connections.Deps) { d.Audit = nil },
		"RedirectURI": func(d *connections.Deps) { d.RedirectURI = "" },
	} {
		d := newEnv(t).deps
		mutate(&d)
		svc, err := connections.New(d)
		if err == nil || svc != nil || !strings.Contains(err.Error(), "Deps."+name+" ") {
			t.Errorf("%s: got %v, %v; want an error naming the field", name, svc, err)
		}
	}
	if _, err := connections.New(newEnv(t).deps); err != nil {
		t.Fatalf("complete deps: %v", err)
	}
}

func TestServiceResolveCredential(t *testing.T) {
	e := newEnv(t)
	svc := e.service(t)
	got, err := svc.ResolveWithoutSending(ctx, connections.ResolveQuery{UserID: "alice", Server: "tracker"})
	if err != nil || got != (connections.CredentialResult{Failure: connections.FailureNeedsConnection}) {
		t.Fatalf("never connected: %v %v", got, err)
	}
	e.seedPAT("alice", "tracker", "pat-1")
	got, err = svc.ResolveWithoutSending(ctx, connections.ResolveQuery{UserID: "alice", Server: "tracker"})
	if err != nil || !got.Ok() || got.Secret != "pat-1" {
		t.Fatalf("stored PAT: %v %v", got, err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", got, got, got), "pat-1") {
		t.Fatal("the result prints its secret")
	}
	// A secret issued for another configuration of the server is never handed out.
	e.dir.servers[1].Config.URL = "https://elsewhere.example.com/mcp"
	got, err = svc.ResolveWithoutSending(ctx, connections.ResolveQuery{UserID: "alice", Server: "tracker"})
	if err != nil || got.Failure != connections.FailureExpired {
		t.Fatalf("changed configuration: %v %v", got, err)
	}
}

// The method the gateway uses: the secret goes only to the configuration the caller read. A stale
// target gets `unavailable` and nothing is handed out, stored or expired.
func TestServiceResolveForSendingIsPinnedToTheTarget(t *testing.T) {
	e := newEnv(t)
	e.seedPAT("alice", "tracker", "pat-1")
	svc := e.service(t)
	current := catalog.ServerCallTarget{Slug: "tracker", Name: "Tracker", URL: "https://tracker.example.com/mcp", Auth: catalog.AuthMode{Kind: catalog.AuthPATUser}}
	got, err := svc.ResolveForSending(ctx, "alice", current, false)
	if err != nil || !got.Ok() || got.Secret != "pat-1" {
		t.Fatalf("current target: %v %v", got, err)
	}
	before := e.store.rows["alice\x00tracker"]
	stale := current
	stale.URL = "https://elsewhere.example.com/mcp"
	got, err = svc.ResolveForSending(ctx, "alice", stale, false)
	if err != nil || got != (connections.CredentialResult{Failure: connections.FailureUnavailable}) {
		t.Fatalf("stale target: %v %v", got, err)
	}
	if !reflect.DeepEqual(before, e.store.rows["alice\x00tracker"]) {
		t.Fatal("the stored connection changed")
	}
}

func TestServiceResolveCredentialOfAnOAuthConnection(t *testing.T) {
	for _, c := range []struct {
		name      string
		expiresIn time.Duration
		err       error
		want      connections.CredentialResult
	}{
		{"fresh", time.Hour, nil, connections.CredentialResult{Secret: "ACCESS-OLD"}},
		{"renewed ahead of expiry", 10 * time.Second, nil, connections.CredentialResult{Secret: "ACCESS-NEW"}},
		{"provider down, token still valid", 5 * time.Second, errors.New("connection refused"), connections.CredentialResult{Secret: "ACCESS-OLD"}},
		{"provider down, token expired", -time.Minute, errors.New("connection refused"), connections.CredentialResult{Failure: connections.FailureUnavailable}},
	} {
		e := newEnv(t)
		e.seedOAuth("alice", c.expiresIn)
		e.oauth.refreshErr = c.err
		got, err := e.service(t).ResolveWithoutSending(ctx, connections.ResolveQuery{UserID: "alice", Server: "gitea"})
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
}

func TestOneSingleFlightIsSharedByAllCallers(t *testing.T) {
	e := newEnv(t)
	e.oauth.entered, e.oauth.gate = make(chan struct{}), make(chan struct{})
	e.seedOAuth("alice", 10*time.Second) // inside the 60 s refresh window
	svc := e.service(t)

	results := make(chan connections.CredentialResult, 2)
	run := func() {
		got, err := svc.ResolveWithoutSending(ctx, connections.ResolveQuery{UserID: "alice", Server: "gitea"})
		if err != nil {
			t.Error(err)
		}
		results <- got
	}
	go run()
	<-e.oauth.entered
	_, locksBefore := e.store.counts()
	go run() // joins the renewal under way instead of starting another one
	// Both callers wait in the flight's Do: the second one joined (a second flight of its own
	// would not hold it there, its provider call is not gated).
	deadline := time.After(5 * time.Second)
	for waitingInDo() < 2 {
		select {
		case <-deadline:
			t.Fatal("the second caller never joined the renewal")
		default:
			runtime.Gosched()
		}
	}
	close(e.oauth.gate)
	for range 2 {
		if got := <-results; !got.Ok() || got.Secret != "ACCESS-NEW" {
			t.Fatalf("got %v", got)
		}
	}
	if _, locks := e.store.counts(); locks != locksBefore || e.oauth.refreshes != 1 {
		t.Fatalf("renewals: %d row locks (was %d), %d provider calls; want one renewal for both callers", locks, locksBefore, e.oauth.refreshes)
	}
}

// waitingInDo counts the goroutines blocked in SingleFlight.Do.
func waitingInDo() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "SingleFlight") && strings.Contains(g, ").Do(") {
			n++
		}
	}
	return n
}

func TestServiceMarkExpired(t *testing.T) {
	e := newEnv(t)
	e.seedPAT("alice", "tracker", "pat-1")
	svc := e.service(t)
	if err := svc.MarkExpired(ctx, connections.MarkExpiredCommand{UserID: "alice", Server: "tracker"}); err != nil {
		t.Fatal(err)
	}
	if e.store.rows["alice\x00tracker"].Status != domain.StatusExpired {
		t.Fatal("not expired")
	}
	got, _ := svc.ResolveWithoutSending(ctx, connections.ResolveQuery{UserID: "alice", Server: "tracker"})
	if got.Failure != connections.FailureExpired {
		t.Fatalf("got %v", got)
	}
	if err := svc.MarkExpired(ctx, connections.MarkExpiredCommand{UserID: "nobody", Server: "tracker"}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceListConnections(t *testing.T) {
	e := newEnv(t)
	e.seedPAT("alice", "tracker", "pat-1")
	list, err := e.service(t).ListConnections(ctx, people["alice"])
	if err != nil {
		t.Fatal(err)
	}
	want := []connections.ConnectionSummary{
		{Server: "gitea", Name: "Gitea", AuthKind: catalog.AuthOAuthUser, Status: connections.SummaryMissing},
		{Server: "tracker", Name: "Tracker", AuthKind: catalog.AuthPATUser, Status: connections.SummaryReady},
	}
	if len(list) != 2 || list[0] != want[0] || list[1] != want[1] {
		t.Fatalf("got %+v", list)
	}
}

func TestServiceStoreFailuresAreErrors(t *testing.T) {
	e := newEnv(t)
	e.store.failGet = errors.New("store down")
	svc := e.service(t)
	if _, err := svc.ListConnections(ctx, people["alice"]); err == nil {
		t.Fatal("a store failure was swallowed")
	}
}

func TestServiceResealConnections(t *testing.T) {
	e := newEnv(t)
	e.seedPAT("alice", "tracker", "pat-1")
	svc := e.service(t)
	sum, err := svc.ResealConnections(ctx, box{1}, box{2})
	if err != nil || sum.Resealed != 1 || len(sum.Failed) != 0 {
		t.Fatalf("%+v %v", sum, err)
	}
	sum, err = svc.ResealConnections(ctx, box{1}, box{2})
	if err != nil || sum.Current != 1 || sum.Resealed != 0 {
		t.Fatalf("repeat: %+v %v", sum, err)
	}
}

func TestClientSecretConstructorKeepsTheSecretOutOfPrints(t *testing.T) {
	s := connections.NewClientSecret("hunter2")
	if v, ok := s.Value(); !ok || v != "hunter2" {
		t.Fatal("value lost")
	}
	if out := fmt.Sprintf("%v %+v %#v %s", s, s, s, s); strings.Contains(out, "hunter2") {
		t.Fatalf("printed: %s", out)
	}
	if _, ok := (connections.ClientSecret{}).Value(); ok {
		t.Fatal("the zero value is a public client")
	}
}
