package app

import (
	"context"
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
)

// ServerCallTarget is the catalog's description of a server to call (an alias, so the root package
// can name it without importing the catalog).
type ServerCallTarget = catalog.ServerCallTarget

// ServerConfigOf is the configuration of a catalog server as far as personal credentials care: the
// one mapping from the catalog's call target to what the fingerprint is computed over (the
// directory's Config and a caller's expectation both use it). The OAuth client only for an
// oauth_user server; slices and the revoke URL are copied.
func ServerConfigOf(t ServerCallTarget) ServerConfig {
	cfg := ServerConfig{Slug: t.Slug, URL: t.URL, AuthKind: t.Auth.Kind}
	if t.Auth.Kind == catalog.AuthOAuthUser && t.Auth.OAuth != nil {
		o := *t.Auth.OAuth
		o.Scopes = slices.Clone(o.Scopes)
		o.ExtraAuthorizeParams = slices.Clone(o.ExtraAuthorizeParams)
		if o.RevokeURL != nil {
			v := *o.RevokeURL
			o.RevokeURL = &v
		}
		cfg.OAuth = &o
	}
	return cfg
}

// fingerprint is the ServerFingerprint of a server's configuration: the places a person's token or
// code is sent.
func fingerprint(c ServerConfig) string {
	o := domain.ServerConfigOrigin{URL: c.URL, AuthKind: string(c.AuthKind)}
	if c.OAuth != nil {
		o.OAuth = &domain.OAuthEndpoints{AuthorizeURL: c.OAuth.AuthorizeURL, TokenURL: c.OAuth.TokenURL, ClientID: c.OAuth.ClientID}
	}
	return domain.ServerFingerprint(o)
}

// usablePersonalServer is the server if the principal may use it and it takes this kind of
// personal credential. Unknown, shared_secret, none, closed to the person and disabled servers are
// all not_found; a server of the other personal kind is invalid_auth_kind.
func usablePersonalServer(ctx context.Context, servers ServerDirectory, p Principal, slug string, kind catalog.AuthKind) (PersonalServer, error) {
	list, err := servers.PersonalServersFor(ctx, p)
	if err != nil {
		return PersonalServer{}, err
	}
	for _, s := range list {
		if s.Slug != slug {
			continue
		}
		if s.AuthKind != kind {
			what := domain.WhatPAT
			if kind == catalog.AuthOAuthUser {
				what = domain.WhatOAuth
			}
			return PersonalServer{}, domain.ErrInvalidAuthKind(slug, what)
		}
		return s, nil
	}
	return PersonalServer{}, domain.ErrNotFound(slug)
}

// record writes an audit record with a context that outlives a cancelled request: the change it
// describes is already committed.
func record(ctx context.Context, log kernel.AuditLog, actor, action, target string, meta map[string]any) error {
	return log.Record(context.WithoutCancel(ctx), kernel.AuditEntry{ActorID: &actor, Action: action, Target: &target, Meta: meta})
}
