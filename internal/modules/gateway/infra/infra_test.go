package infra

import (
	"context"
	"errors"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
)

type fakeCatalog struct {
	toolErr error
	view    catalog.ToolView
	target  *catalog.ServerCallTarget
	secret  string
	found   catalog.SecretLookup
	secErr  error
	asked   *catalog.ServerCallTarget // the target GetServerSecretFor was given
}

func (f fakeCatalog) GetToolFor(context.Context, catalog.Principal, string) (catalog.ToolView, error) {
	return f.view, f.toolErr
}

func (f fakeCatalog) GetServerForUpstream(context.Context, string) (*catalog.ServerCallTarget, error) {
	return f.target, nil
}

func (f fakeCatalog) GetServerSecretFor(_ context.Context, target catalog.ServerCallTarget) (string, catalog.SecretLookup, error) {
	if f.asked != nil {
		*f.asked = target
	}
	return f.secret, f.found, f.secErr
}

func TestToolForMapsBusinessRefusalsToNotFoundAndKeepsUnexpectedErrors(t *testing.T) {
	boom := errors.New("database down")
	forbidden := kernel.NewDomainError(catalog.CodeForbidden, "x")
	for name, tc := range map[string]struct {
		err     error
		wantOK  bool
		wantErr error
	}{
		"found":                {nil, true, nil},
		"not found":            {kernel.NewDomainError(catalog.CodeNotFound, "x"), false, nil},
		"invalid ref":          {kernel.NewDomainError(catalog.CodeInvalidToolRef, "x"), false, nil},
		"wrapped not_found":    {errors.Join(errors.New("ctx"), kernel.NewDomainError(catalog.CodeNotFound, "x")), false, nil},
		"another domain error": {forbidden, false, forbidden},
		"unexpected":           {boom, false, boom},
	} {
		t.Run(name, func(t *testing.T) {
			c := toolCatalog{api: fakeCatalog{toolErr: tc.err, view: catalog.ToolView{Name: "t"}}}
			view, ok, err := c.ToolFor(context.Background(), app.Principal{}, "a.t")
			if ok != tc.wantOK || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if ok && view.Name != "t" {
				t.Fatalf("view %+v", view)
			}
		})
	}
}

func TestServerForAndSecretPassThrough(t *testing.T) {
	target := &catalog.ServerCallTarget{Slug: "gitea"}
	c := toolCatalog{api: fakeCatalog{target: target}}
	got, err := c.ServerFor(context.Background(), "gitea")
	if err != nil || got != target {
		t.Fatalf("%v %v", got, err)
	}
	var asked catalog.ServerCallTarget
	c = toolCatalog{api: fakeCatalog{secret: "", found: catalog.SecretFound, asked: &asked}}
	secret, found, err := c.ServerSecret(context.Background(), *target)
	if err != nil || found != catalog.SecretFound || secret != "" || asked != *target {
		t.Fatalf("an empty secret that is set stays set, for the target given: %q %v %v %+v", secret, found, err, asked)
	}
	moved := toolCatalog{api: fakeCatalog{found: catalog.SecretMoved}}
	if _, found, _ := moved.ServerSecret(context.Background(), *target); found != catalog.SecretMoved {
		t.Fatal("moved passes through")
	}
	boom := errors.New("key mismatch")
	if _, _, err := (toolCatalog{api: fakeCatalog{secErr: boom}}).ServerSecret(context.Background(), *target); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}

// The connections service is the port as it stands: the gateway can only resolve a credential
// together with the configuration it is sent to.
var _ app.PersonalCredentials = NewPersonalCredentials((*connections.Service)(nil))

func TestConstantTimeSecrets(t *testing.T) {
	var c ConstantTimeSecrets
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"token", "token", true},
		{"token", "tokeN", false},
		{"token", "token2", false},
		{"", "", true},
		{"", "x", false},
		{"a longer secret value", "a", false},
	} {
		if got := c.Equal(tc.a, tc.b); got != tc.want {
			t.Errorf("Equal(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
