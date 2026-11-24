package infra

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgPendingLoginRepository is app.PendingLoginRepository on identity.oidc_pending.
type pgPendingLoginRepository struct{ pool *pgxpool.Pool }

var _ app.PendingLoginRepository = (*pgPendingLoginRepository)(nil)

// Insert first removes attempts expired for over a minute (oidc_pending_expires_idx), by the
// DATABASE clock: the minute absorbs a database clock ahead of the application's, and the use
// case's own expiry check stays the authority. Unexpired rows are never removed: evicting them
// would let an anonymous caller cancel other people's logins. Two statements, not a transaction.
func (r *pgPendingLoginRepository) Insert(ctx context.Context, p app.PendingLogin) error {
	if _, err := r.pool.Exec(ctx, `delete from identity.oidc_pending where expires_at < now() - interval '1 minute'`); err != nil {
		return err
	}
	_, err := r.pool.Exec(ctx, `insert into identity.oidc_pending (state, code_verifier, nonce, return_to, expires_at)
      values ($1, $2, $3, $4, $5)`, p.State.Reveal(), p.CodeVerifier.Reveal(), p.Nonce.Reveal(), p.ReturnTo, us(p.ExpiresAt))
	return err
}

func (r *pgPendingLoginRepository) Consume(ctx context.Context, state string) (*app.PendingLogin, error) {
	var p app.PendingLogin
	var st, verifier, nonce string
	var exp time.Time
	err := r.pool.QueryRow(ctx, `delete from identity.oidc_pending where state = $1
      returning state, code_verifier, nonce, return_to, expires_at`, state).
		Scan(&st, &verifier, &nonce, &p.ReturnTo, &exp)
	p.State, p.CodeVerifier, p.Nonce = kernel.NewSecret(st), kernel.NewSecret(verifier), kernel.NewSecret(nonce)
	p.ExpiresAt = exp.UTC()
	return one(p, err)
}
