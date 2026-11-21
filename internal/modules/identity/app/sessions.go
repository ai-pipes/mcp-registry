package app

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// Logout ends a session. An unknown session is a no-op and is not audited.
func Logout(ctx context.Context, deps Deps, sessionID string) error {
	idHash := deps.Hasher.Hex(sessionID)
	session, err := deps.Sessions.FindByHash(ctx, idHash)
	if err != nil || session == nil {
		return err
	}
	if err := deps.Sessions.Delete(ctx, idHash); err != nil {
		return err
	}
	return deps.Audit.Record(ctx, kernel.AuditEntry{ActorID: ptr(session.UserID), Action: "auth.logout"})
}

// AuthenticateSession returns the principal of a live session, or nil when the session is unknown,
// dead (its row is then deleted), or its person is gone or disabled (the row is kept). Every
// success refreshes the session's last-seen time.
func AuthenticateSession(ctx context.Context, deps Deps, sessionID string) (*domain.Principal, error) {
	idHash := deps.Hasher.Hex(sessionID)
	session, err := deps.Sessions.FindByHash(ctx, idHash)
	if err != nil || session == nil {
		return nil, err
	}
	now := deps.Clock.Now()
	if !domain.IsSessionAlive(session.LastSeenAt, session.ExpiresAt, now) {
		return nil, deps.Sessions.Delete(ctx, idHash)
	}
	user, err := deps.Users.FindByID(ctx, session.UserID)
	if err != nil || user == nil || user.Disabled {
		return nil, err
	}
	if err := deps.Sessions.Touch(ctx, idHash, now); err != nil {
		return nil, err
	}
	p := domain.PrincipalOf(*user, domain.ViaSession, "")
	return &p, nil
}

// PruneExpiredSessions is the daily housekeeping: it deletes expired login links, then dead
// sessions (12 h idle or the 30-day limit). A dead session or link is refused anyway; this only
// keeps the tables from growing. It returns the number of deleted sessions (links are not counted).
func PruneExpiredSessions(ctx context.Context, deps Deps) (int, error) {
	now := deps.Clock.Now()
	if _, err := deps.LoginLinks.DeleteExpired(ctx, now); err != nil {
		return 0, err
	}
	return deps.Sessions.DeleteDead(ctx, domain.DeadSessionCutoffs(now))
}
