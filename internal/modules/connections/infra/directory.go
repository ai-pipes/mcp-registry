package infra

import (
	"context"
	"errors"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

// CatalogAPI is the part of the catalog's public API the directory reads (*catalog.Service).
type CatalogAPI interface {
	ListServersFor(ctx context.Context, p catalog.Principal) ([]catalog.MemberServerView, error)
	GetServerForUpstream(ctx context.Context, slug string) (*catalog.ServerCallTarget, error)
	GetServerSecretFor(ctx context.Context, target catalog.ServerCallTarget) (kernel.Secret, catalog.SecretLookup, error)
}

// serverDirectory is app.ServerDirectory over the catalog's public queries only
// (createCatalogServerDirectory of the TypeScript server).
type serverDirectory struct{ catalog CatalogAPI }

var _ app.ServerDirectory = (*serverDirectory)(nil)

// NewServerDirectory adapts the catalog. It keeps no plain text: the client secret is opened by
// the catalog on every call and handed on.
func NewServerDirectory(c CatalogAPI) app.ServerDirectory { return &serverDirectory{catalog: c} }

var errSecretUnreadable = errors.New("the server's client secret cannot be read")

// Config is the configuration of an active server (nil when unknown or disabled): URL as stored,
// the kind, and the OAuth client only for oauth_user.
func (d *serverDirectory) Config(ctx context.Context, slug string) (*app.ServerConfig, error) {
	t, err := d.catalog.GetServerForUpstream(ctx, slug)
	if err != nil || t == nil {
		return nil, err
	}
	cfg := app.ServerConfigOf(*t)
	return &cfg, nil
}

// PersonalServersFor lists the active servers the person may use that take a personal credential
// (the catalog applies the access rules and orders by slug), each with its configuration: an
// extra read per server, as in the reference. A server that has no configuration any more is
// dropped.
func (d *serverDirectory) PersonalServersFor(ctx context.Context, p app.Principal) ([]app.PersonalServer, error) {
	views, err := d.catalog.ListServersFor(ctx, p)
	if err != nil {
		return nil, err
	}
	out := []app.PersonalServer{}
	for _, v := range views {
		if v.AuthKind != catalog.AuthOAuthUser && v.AuthKind != catalog.AuthPATUser {
			continue
		}
		cfg, err := d.Config(ctx, v.Slug)
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			continue
		}
		out = append(out, app.PersonalServer{Slug: v.Slug, Name: v.Name, AuthKind: v.AuthKind, Config: *cfg})
	}
	return out, nil
}

// ClientSecretFor is the server's OAuth client secret for the configuration the caller read, from
// ONE read of the server by the catalog (any server status): moved when the secret no longer goes
// where cfg sends it, the zero value when none is set. An error carries a fixed text: the catalog's
// own error is dropped.
func (d *serverDirectory) ClientSecretFor(ctx context.Context, cfg app.ServerConfig) (app.ClientSecret, bool, error) {
	target := catalog.ServerCallTarget{Slug: cfg.Slug, URL: cfg.URL, Auth: catalog.AuthMode{Kind: cfg.AuthKind, OAuth: cfg.OAuth}}
	secret, lookup, err := d.catalog.GetServerSecretFor(ctx, target)
	if err != nil {
		if ctx.Err() != nil {
			return app.ClientSecret{}, false, ctx.Err()
		}
		return app.ClientSecret{}, false, errSecretUnreadable
	}
	switch lookup {
	case catalog.SecretFound:
		return app.NewClientSecret(secret.Reveal()), false, nil
	case catalog.SecretNotSet:
		return app.ClientSecret{}, false, nil
	default:
		return app.ClientSecret{}, true, nil
	}
}
