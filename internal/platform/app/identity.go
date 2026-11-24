package app

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	idhttp "github.com/ai-pipes/mcp-registry/internal/modules/identity/http"
	idinfra "github.com/ai-pipes/mcp-registry/internal/modules/identity/infra"
	"github.com/ai-pipes/mcp-registry/internal/platform/config"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
	"github.com/ai-pipes/mcp-registry/internal/platform/scheduler"
)

// Operator-facing texts of the reference v1.0.1:apps/server/src/platform/{bootstrap,scheduler}.ts.
const (
	msgFirstStartLink       = "first start: open this one-time link within 15 minutes to sign in as admin"
	msgFirstStartFailed     = "first start: could not issue the sign-in link"
	msgSessionRetentionDone = "session retention: done"
	msgSessionRetentionFail = "session retention: run failed"
)

// sessionRetentionEvery is the period of the pruning job (DAY_MS in scheduler.ts).
const sessionRetentionEvery = 24 * time.Hour

// identityWiring is the identity module built from the runtime. Building it makes no request.
type identityWiring struct {
	rt      Runtime
	deps    idapp.Deps
	service *identity.Service // built once; identity's routes and the platform's routes share it
}

// unconfiguredOIDC is the provider of a server without OIDC_*: both methods fail with a plain
// error (container.ts unconfiguredOidc), which is not the module's idp_unavailable.
type unconfiguredOIDC struct{}

var errOIDCNotConfigured = errors.New("OIDC provider is not configured")

func (unconfiguredOIDC) AuthorizationURL(context.Context, idapp.OIDCAuthorizationInput) (*url.URL, error) {
	return nil, errOIDCNotConfigured
}

func (unconfiguredOIDC) Exchange(context.Context, idapp.OIDCExchangeInput) (domain.OIDCClaims, error) {
	return domain.OIDCClaims{}, errOIDCNotConfigured
}

// oidcConfigFor is what the provider adapter gets from the configuration: the client, the redirect
// URI (`new URL('/api/auth/callback', PUBLIC_URL)`), the scopes and the groups claim.
func oidcConfigFor(cfg config.Config, logger *slog.Logger) idinfra.OIDCConfig {
	return idinfra.OIDCConfig{
		Issuer:       cfg.OIDC.Issuer,
		ClientID:     cfg.OIDC.ClientID,
		ClientSecret: cfg.OIDC.ClientSecret,
		RedirectURI:  resolveURL(cfg.PublicURL, "/api/auth/callback"),
		Scopes:       cfg.OIDCScopes,
		GroupsClaim:  cfg.OIDCGroupsClaim,
		Logger:       logger,
	}
}

func newIdentityWiring(rt Runtime) identityWiring {
	cfg := rt.Config
	// The limiters count in wall-clock time, as the TypeScript ones do: the system clock.
	pg := idinfra.NewPg(rt.Pool, rt.Clock)
	var provider idapp.OIDCProvider = unconfiguredOIDC{}
	if cfg.OIDC != nil {
		provider = idinfra.NewOIDCProvider(oidcConfigFor(cfg, rt.Logger))
	}
	return identityWiringOver(rt, pg, provider)
}

// identityWiringOver builds the module over the given adapters and provider: the one place that
// decides which limiter serves which use case, and the one place the *identity.Service is built.
// Tests hand it in-memory adapters (with the real limiters of idinfra.NewPg).
func identityWiringOver(rt Runtime, pg idinfra.Adapters, provider idapp.OIDCProvider) identityWiring {
	cfg := rt.Config
	deps := idapp.Deps{
		Users: pg.Users, Tokens: pg.Tokens, Sessions: pg.Sessions, PendingLogins: pg.PendingLogins,
		LoginLinks: pg.LoginLinks, OIDC: provider,
		Policy:       domain.GroupPolicy{LoginGroup: cfg.OIDCLoginGroup, AdminGroup: cfg.OIDCAdminGroup},
		Hasher:       pg.Hasher,
		Random:       pg.Random,
		TokenLimiter: pg.TokenLimiter, LinkLimiter: pg.LinkLimiter, CallbackLimiter: pg.CallbackLimiter,
		Audit: rt.Audit, Clock: rt.Clock,
	}
	return identityWiring{rt: rt, deps: deps, service: identity.New(deps)}
}

// module is what identity adds to the server: its routes with the session guards injected, the
// pruning job and the first-start admin.
func (w identityWiring) module() Module {
	routes, prefixes := idhttp.Routes(w.service, w.deps, idhttp.Options{
		CookieSecure:   w.rt.Config.CookieSecure,
		OIDCConfigured: w.rt.Config.OIDC != nil,
		Logger:         w.rt.Logger,
	})
	return Module{
		Routes:   routes,
		Prefixes: prefixes,
		Jobs: []scheduler.Job{{
			Name: "session-retention", Every: sessionRetentionEvery, Run: w.pruneSessions,
		}},
		FirstStart: w.firstStart,
		Commands:   w.commands(),
	}
}

// firstStart is announceFirstStart (bootstrap.ts): without OIDC, BootstrapLocalAdmin decides
// whether a sign-in link is due and this only logs it. The one-time code appears in this single
// record (field url), never in audit records, errors or other log lines. With OIDC configured
// nobody is created and nothing is printed. A failure is logged without its text and the server
// still starts; the next start tries again while local-admin has never signed in.
func (w identityWiring) firstStart(ctx context.Context) {
	if w.rt.Config.OIDC != nil {
		return
	}
	code, issued, err := idapp.BootstrapLocalAdmin(ctx, w.deps)
	if err != nil {
		w.rt.Logger.Error(msgFirstStartFailed, logging.ErrAttr(err))
		return
	}
	if !issued {
		return
	}
	w.rt.Logger.Warn(msgFirstStartLink, "url", domain.LoginLinkURL(w.rt.Config.PublicURL, code))
}

// pruneSessions is the job of scheduler.ts pruneSessions: expired login links and dead sessions,
// at start and then daily. A failure is logged here and does not stop the schedule; one caused by
// the shutdown is left to the scheduler, which does not log it.
func (w identityWiring) pruneSessions(ctx context.Context) error {
	n, err := idapp.PruneExpiredSessions(ctx, w.deps)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		w.rt.Logger.Error(msgSessionRetentionFail, logging.ErrAttr(err))
		return nil
	}
	w.rt.Logger.Info(msgSessionRetentionDone, "sessions", n)
	return nil
}
