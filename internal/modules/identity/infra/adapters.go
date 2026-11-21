package infra

import (
	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Adapters is every port implementation of the module that needs no identity provider.
type Adapters struct {
	Users           app.UserRepository
	Tokens          app.TokenRepository
	Sessions        app.SessionRepository
	PendingLogins   app.PendingLoginRepository
	LoginLinks      app.LoginLinkRepository
	Hasher          app.Hasher
	Random          app.Random
	TokenLimiter    app.RateLimiter
	LinkLimiter     app.RateLimiter
	CallbackLimiter app.RateLimiter
}

// NewPg builds the adapters on one pool, with the limiters set up as compose.ts does: token checks
// 30 per 60 s (lock 60 s), link attempts 10 per 60 s (lock 60 s), failed callbacks 20 per 60 s (lock 60 s).
func NewPg(pool *pgxpool.Pool, clock kernel.Clock) Adapters {
	return Adapters{
		Users:           &pgUserRepository{pool},
		Tokens:          &pgTokenRepository{pool},
		Sessions:        &pgSessionRepository{pool},
		PendingLogins:   &pgPendingLoginRepository{pool},
		LoginLinks:      &pgLoginLinkRepository{pool},
		Hasher:          NewSHA256Hasher(),
		Random:          NewCryptoRandom(),
		TokenLimiter:    NewLimiter(30, 60, 60, clock),
		LinkLimiter:     NewLimiter(10, 60, 60, clock),
		CallbackLimiter: NewLimiter(domain.FailedCallbackAuditLimit, domain.FailedCallbackWindowSec, domain.FailedCallbackWindowSec, clock),
	}
}
