package crypto

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Verdicts of the key check. Created is returned only by Verify; Absent only by Judge, Inspect
// and ResealCheck.
const (
	KeyCheckCreated       = "created"
	KeyCheckOK            = "ok"
	KeyCheckNeedsRotation = "needs_rotation"
	KeyCheckMismatch      = "mismatch"
	KeyCheckAbsent        = "absent"
)

// keyCheckPlain is the text sealed into platform.key_check; the same literal as PLAIN in
// apps/server/src/platform/crypto/key-check.ts (pinned by a test that reads that file).
const keyCheckPlain = "mcp-registry key check v1"

// Keys are the configured secrets keys: SECRETS_KEY and, while rotating, SECRETS_KEY_OLD. Each key
// is a kernel.SecretBytes, which keeps its bytes two unexported pointers deep and prints as
// "[Redacted]" however it is held, and the type itself prints, logs and encodes as "[Redacted]"
// through every verb. The zero value is "no keys"; the type is not comparable on purpose. Copies
// of a Keys share the same immutable bytes; NewKeys copies its inputs.
type Keys struct {
	_        [0]func() // not comparable: == would compare addresses, never the keys
	cur, prv kernel.SecretBytes
}

// NewKeys wraps copies of current and previous (nil or empty previous: no previous key).
func NewKeys(current, previous []byte) Keys {
	return Keys{cur: kernel.NewSecretBytes(current), prv: kernel.NewSecretBytes(previous)}
}

// current is the key new secrets are sealed with, nil for the zero value.
func (k Keys) current() []byte { return k.cur.Reveal() }

// previous is the key being rotated away from, nil when there is none.
func (k Keys) previous() []byte { return k.prv.Reveal() }

const keysRedacted = "[Redacted]"

// Format implements fmt.Formatter for every verb (without it %d or %x would print the bytes).
func (Keys) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(keysRedacted)) }

// String implements fmt.Stringer.
func (Keys) String() string { return keysRedacted }

// GoString implements fmt.GoStringer (the %#v verb).
func (Keys) GoString() string { return keysRedacted }

// LogValue implements slog.LogValuer.
func (Keys) LogValue() slog.Value { return slog.StringValue(keysRedacted) }

// MarshalJSON implements encoding/json.Marshaler (also honoured by encoding/json/v2).
func (Keys) MarshalJSON() ([]byte, error) { return []byte(`"` + keysRedacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Keys) MarshalText() ([]byte, error) { return []byte(keysRedacted), nil }

func opensToPlain(key, sealed []byte) bool {
	box, err := NewAESGCMBox(key, nil)
	if err != nil {
		return false
	}
	plain, err := box.Open(sealed)
	return err == nil && plain == keyCheckPlain
}

// Judge says what a stored check value (nil when there is none) tells about the configured
// keys: "absent", "ok", "needs_rotation" or "mismatch". Pure.
func Judge(sealed []byte, k Keys) string {
	if sealed == nil {
		return KeyCheckAbsent
	}
	if opensToPlain(k.current(), sealed) {
		return KeyCheckOK
	}
	if k.previous() != nil && opensToPlain(k.previous(), sealed) {
		return KeyCheckNeedsRotation
	}
	return KeyCheckMismatch
}

func settled(sealed []byte, k Keys) string {
	if v := Judge(sealed, k); v != KeyCheckAbsent {
		return v
	}
	return KeyCheckMismatch
}

// readSealed returns the stored value, or nil when the table is empty.
func readSealed(ctx context.Context, pool *pgxpool.Pool) ([]byte, error) {
	var sealed []byte
	err := pool.QueryRow(ctx, `select sealed from platform.key_check`).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return sealed, err
}

// Verify reads the check value and creates it when absent; it never overwrites an existing one.
// A concurrent first start that lost the insert judges what the winner stored.
func Verify(ctx context.Context, pool *pgxpool.Pool, k Keys) (string, error) {
	existing, err := readSealed(ctx, pool)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	if existing != nil {
		return settled(existing, k), nil
	}
	box, err := NewAESGCMBox(k.current(), nil)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	sealed, err := box.Seal(keyCheckPlain)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	tag, err := pool.Exec(ctx,
		`insert into platform.key_check (sealed) values ($1) on conflict (id) do nothing`, sealed)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return KeyCheckCreated, nil
	}
	stored, err := readSealed(ctx, pool)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	return settled(stored, k), nil
}

// Inspect is the read-only judgement: it never creates or changes the stored value.
func Inspect(ctx context.Context, pool *pgxpool.Pool, k Keys) (string, error) {
	sealed, err := readSealed(ctx, pool)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	return Judge(sealed, k), nil
}

// ResealCheck re-seals the check value with the current key (rotate-secrets-key). Outcomes:
// "current", "resealed", "unreadable", "absent". The write is a compare-and-set on the old
// bytes, so a concurrent run or start is never overwritten with stale bytes.
func ResealCheck(ctx context.Context, pool *pgxpool.Pool, k Keys) (string, error) {
	existing, err := readSealed(ctx, pool)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	if existing == nil {
		return KeyCheckAbsent, nil
	}
	if k.previous() == nil {
		if opensToPlain(k.current(), existing) {
			return kernel.ResealCurrent, nil
		}
		return kernel.ResealUnreadable, nil
	}
	from, err := NewAESGCMBox(k.previous(), nil)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	to, err := NewAESGCMBox(k.current(), nil)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	r := kernel.Reseal(existing, from, to)
	if r.Kind != kernel.ResealResealed {
		return r.Kind, nil
	}
	tag, err := pool.Exec(ctx,
		`update platform.key_check set sealed = $1 where sealed = $2`, r.Sealed.Reveal(), existing)
	if err != nil {
		return "", fmt.Errorf("key check: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return kernel.ResealResealed, nil
	}
	return kernel.ResealCurrent, nil
}

// Remove deletes the stored check value, so the next start records the configured key. It
// reports whether one existed.
func Remove(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	tag, err := pool.Exec(ctx, `delete from platform.key_check`)
	if err != nil {
		return false, fmt.Errorf("key check: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
