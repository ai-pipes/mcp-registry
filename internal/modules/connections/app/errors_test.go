package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

func TestTokenEndpointErrorText(t *testing.T) {
	if got := (&app.TokenEndpointError{Status: 503}).Error(); got != "token endpoint answered HTTP 503" {
		t.Fatal(got)
	}
	if got := (&app.TokenEndpointError{Status: 400, Code: "invalid_grant"}).Error(); got != "token endpoint answered HTTP 400 (invalid_grant)" {
		t.Fatal(got)
	}
}

func TestCredentialResultPrinting(t *testing.T) {
	r := app.CredentialResult{Secret: "SECRET"}
	if !r.Ok() || (app.CredentialResult{Failure: app.FailureExpired}).Ok() {
		t.Fatal("Ok")
	}
	for _, s := range []string{r.String(), r.GoString(), fmt.Sprintf("%v %+v %#v %s", r, r, r, r)} {
		if strings.Contains(s, "SECRET") {
			t.Fatal(s)
		}
	}
	if got := (app.CredentialResult{Failure: app.FailureUnavailable}).String(); got != "CredentialResult{failure:unavailable}" {
		t.Fatal(got)
	}
	ts, cs := app.TokenSet{AccessToken: "SECRET"}, app.NewClientSecret("SECRET")
	for _, s := range []string{ts.String(), ts.GoString(), cs.String(), cs.GoString()} {
		if s != "[redacted]" {
			t.Fatal(s)
		}
	}
}

func TestPortErrorsPropagate(t *testing.T) {
	boom := errors.New("boom")

	t.Run("the directory", func(t *testing.T) {
		h := newHarness(t)
		h.dir.listErr = boom
		if _, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if _, err := app.ListConnections(ctx, h.d, app.ListQuery{Principal: alice}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		h.dir.listErr = nil
		h.dir.configErr = boom
		h.seedOAuth(tp(at(3_600_000)), "R1", "alice")
		if _, err := h.resolve("alice", "gitea"); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if err := app.Disconnect(ctx, h.d, app.DisconnectCommand{Principal: alice, Server: "gitea"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})

	t.Run("the store", func(t *testing.T) {
		h := newHarness(t)
		h.store.failGet = boom
		if _, err := app.ListConnections(ctx, h.d, app.ListQuery{Principal: alice}); err != nil {
			t.Fatal(err) // listing does not use Get
		}
		if _, err := h.resolve("alice", "gitea"); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})

	t.Run("the pending store", func(t *testing.T) {
		h := newHarness(t)
		failing := &failingPending{fakePending: h.pending, err: boom}
		h.d.Pending = failing
		if _, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: alice, Server: "gitea"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if _, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: "s", Code: "c"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
}

type failingPending struct {
	*fakePending
	err error
}

func (f *failingPending) Insert(context.Context, domain.PendingConnect) error { return f.err }

func (f *failingPending) Consume(context.Context, string, string) (*domain.PendingConnect, error) {
	return nil, f.err
}

func TestSealFailuresAreErrors(t *testing.T) {
	h := newHarness(t)
	h.box.sealErr = errors.New("no key")
	if err := app.SavePAT(ctx, h.d, app.SavePATCommand{Principal: alice, Server: "yt", Token: "p"}); err == nil {
		t.Fatal("savePat")
	}
	state := stateOf(startURL(t, h, "gitea"))
	if _, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: alice, State: state, Code: "c"}); err == nil {
		t.Fatal("complete (access)")
	}
	h2 := newHarness(t)
	h2.seedOAuth(tp(at(-1000)), "R1", "alice")
	h2.box.sealErr = errors.New("no key")
	if _, err := h2.resolve("alice", "gitea"); err == nil {
		t.Fatal("refresh")
	}
	if h2.status("alice", "gitea") != "ready" {
		t.Fatal("changed")
	}
	// A provider answering without an access token is no answer.
	h3 := newHarness(t)
	h3.seedOAuth(tp(at(-1000)), "R1", "alice")
	h3.oauth.next = app.TokenSet{}
	r, err := h3.resolve("alice", "gitea")
	wantFailure(t, r, err, app.FailureUnavailable)
	if !slices.Equal(h3.oauth.refreshed(), []string{"R1"}) {
		t.Fatal("not asked")
	}
}
