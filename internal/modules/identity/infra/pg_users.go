package infra

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgUserRepository is app.UserRepository on identity.users.
type pgUserRepository struct{ pool *pgxpool.Pool }

var _ app.UserRepository = (*pgUserRepository)(nil)

const userColumns = `id::text, oidc_sub, name, email, groups, role, disabled, created_at, last_login_at`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	var role string
	var groups []string
	var created time.Time
	err := row.Scan(&u.ID, &u.OIDCSub, &u.Name, &u.Email, &groups, &role, &u.Disabled, &created, &u.LastLoginAt)
	if err != nil {
		return domain.User{}, err
	}
	u.Role = domain.Role(role)
	u.Groups = domain.CopyGroups(groups)
	u.CreatedAt = created.UTC()
	u.LastLoginAt = utcPtr(u.LastLoginAt)
	return u, nil
}

func (r *pgUserRepository) FindByID(ctx context.Context, id string) (*domain.User, error) {
	if !isUUID(id) {
		return nil, nil
	}
	return one(scanUser(r.pool.QueryRow(ctx, `select `+userColumns+` from identity.users where id = $1`, id)))
}

func (r *pgUserRepository) FindBySub(ctx context.Context, sub string) (*domain.User, error) {
	return one(scanUser(r.pool.QueryRow(ctx, `select `+userColumns+` from identity.users where oidc_sub = $1`, sub)))
}

// UpsertFromLogin inserts or refreshes a person; a conflict on oidc_sub never touches disabled or created_at.
func (r *pgUserRepository) UpsertFromLogin(ctx context.Context, in app.UpsertLogin) (domain.User, error) {
	at := us(in.At)
	return scanUser(r.pool.QueryRow(ctx, `
      insert into identity.users (oidc_sub, name, email, groups, role, created_at, last_login_at)
      values ($1, $2, $3, $4, $5, $6, $7)
      on conflict (oidc_sub) do update
        set name = excluded.name, email = excluded.email, groups = excluded.groups,
            role = excluded.role, last_login_at = excluded.last_login_at
      returning `+userColumns,
		in.Claims.Sub, in.Claims.Name, in.Claims.Email, domain.CopyGroups(in.Claims.Groups), string(in.Role), at, at))
}

func (r *pgUserRepository) List(ctx context.Context) ([]domain.User, error) {
	rows, err := r.pool.Query(ctx, `select `+userColumns+` from identity.users order by created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *pgUserRepository) SetDisabled(ctx context.Context, id string, disabled bool) (bool, error) {
	if !isUUID(id) {
		return false, nil
	}
	tag, err := r.pool.Exec(ctx, `update identity.users set disabled = $1 where id = $2`, disabled, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *pgUserRepository) TouchLastLogin(ctx context.Context, id string, at time.Time) error {
	if !isUUID(id) {
		return nil
	}
	_, err := r.pool.Exec(ctx, `update identity.users set last_login_at = $1 where id = $2`, us(at), id)
	return err
}
