// Package db opens the Postgres connection pool.
package db

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"syscall"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolMaxConns is the pool size.
const PoolMaxConns = 10

// connectError replaces the driver's error: pgx messages can quote the connection string, so
// callers get a fixed text and a content-free label (kernel.ErrorLabel), never the original.
type connectError struct{ msg string }

func (e *connectError) Error() string { return e.msg }

// cause classifies a connect failure without any content from it (no host, user, database or
// address): refused, dns, timeout, canceled, tls, or the operation of a network error.
func cause(err error) string {
	var (
		dnsErr  *net.DNSError
		opErr   *net.OpError
		alert   tls.AlertError
		verify  *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		record  tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.As(err, &alert), errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &record):
		return "tls"
	case errors.As(err, &opErr):
		return "net " + opErr.Op
	}
	return ""
}

// connectLabel is kernel.ErrorLabel plus the cause when there is one.
func connectLabel(err error) string {
	if c := cause(err); c != "" {
		return kernel.ErrorLabel(err) + "; " + c
	}
	return kernel.ErrorLabel(err)
}

// Open builds the pool from databaseURL and pings it. A non-nil password (from PGPASSWORD_FILE)
// overrides the one in the address and in PGPASSWORD; without it pgx's own rules apply. Errors
// never contain the address or any password.
func Open(ctx context.Context, databaseURL string, password *string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, &connectError{"invalid database configuration (" + kernel.ErrorLabel(err) + ")"}
	}
	if password != nil {
		cfg.ConnConfig.Password = *password
	}
	cfg.MaxConns = PoolMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, &connectError{"cannot open the database pool (" + connectLabel(err) + ")"}
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, &connectError{"cannot connect to the database (" + connectLabel(err) + ")"}
	}
	return pool, nil
}
