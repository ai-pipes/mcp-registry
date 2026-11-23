package infra

import (
	"context"
	"errors"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgPending is app.PendingConnects on connections.pending (PgPendingConnects of the TypeScript
// server).
type pgPending struct{ pool *pgxpool.Pool }

var _ app.PendingConnects = (*pgPending)(nil)

// Insert first drops attempts that expired more than an hour ago (abandoned attempts would pile
// up otherwise; the hour of slack keeps clock skew between the application and the database
// harmless), then inserts. Two statements, no transaction, as in the reference. A duplicate state
// is an error that does not quote it.
func (s *pgPending) Insert(ctx context.Context, p domain.PendingConnect) error {
	if _, err := s.pool.Exec(ctx, `delete from connections.pending where expires_at < now() - interval '1 hour'`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `insert into connections.pending
        (state, user_id, server, code_verifier, issued_for, expires_at)
      values ($1, $2, $3, $4, $5, $6)`,
		p.State(), p.UserID(), p.Server(), p.CodeVerifier(), p.IssuedFor(), ms(p.ExpiresAt()))
	if err != nil {
		return errPendingInsert
	}
	return nil
}

type fixedError string

func (e fixedError) Error() string { return string(e) }

const errPendingInsert = fixedError("the connect attempt could not be stored")

// Consume is ONE statement: the attempt is deleted and returned only when userID started it, so
// two concurrent callbacks of the starter cannot both get it and another person's callback
// removes nothing (the decided deviation oauth-state-kept). Expiry is not judged here: an expired
// attempt of the starter is removed and returned, and the use case refuses it, as the reference
// (which deletes before it checks) consumed it.
func (s *pgPending) Consume(ctx context.Context, state, userID string) (*domain.PendingConnect, error) {
	var (
		snap    domain.PendingSnapshot
		expires time.Time
	)
	err := s.pool.QueryRow(ctx, `delete from connections.pending where state = $1 and user_id = $2
      returning state, user_id::text, server, code_verifier, issued_for, expires_at`, state, userID).
		Scan(&snap.State, &snap.UserID, &snap.Server, &snap.CodeVerifier, &snap.IssuedFor, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap.ExpiresAt = ms(expires)
	p := domain.RestorePendingConnect(snap)
	return &p, nil
}
