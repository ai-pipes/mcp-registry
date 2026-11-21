package infra

import (
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// ids come from the outside (URLs); a malformed one must be "not found", not a database error.
// The shape is the loose 8-4-4-4-12 hex check of the TypeScript repositories.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidShape.MatchString(s) }

// us is a time as Postgres stores it: UTC, microseconds.
func us(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func usPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := us(*t)
	return &v
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// one turns a single-row scan into (nil, nil) when there is no row.
func one[T any](v T, err error) (*T, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}
