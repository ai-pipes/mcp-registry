//go:build integration

package db_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/ai-pipes/mcp-registry/internal/platform/pgtest"
	"github.com/jackc/pgx/v5"
)

// oddChars has the characters that break a naive connection string.
const oddChars = "p@ss:w/rd#1 %"

// roleURL creates a login role with oddChars in a fresh database and returns the address for
// that role with the given password percent-encoded into it.
func roleURL(t *testing.T, password string) string {
	t.Helper()
	ctx := context.Background()
	adminURL := pgtest.StartURL(t)
	u0, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// Roles belong to the cluster, not the database: a unique name per test.
	role := "odd_" + strings.TrimPrefix(strings.TrimPrefix(u0.Path, "/"), "t_")
	if _, err := conn.Exec(ctx, `create role `+role+` login password '`+strings.ReplaceAll(oddChars, `'`, `''`)+`'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), `drop role if exists `+role)
	})
	u := *u0
	u.User = url.UserPassword(role, password)
	return u.String()
}

func TestOpenAndPing(t *testing.T) {
	pool, err := db.Open(context.Background(), pgtest.StartURL(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != db.PoolMaxConns || db.PoolMaxConns != 10 {
		t.Fatalf("MaxConns = %d", got)
	}
	var one int
	if err := pool.QueryRow(context.Background(), `select 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("%d %v", one, err)
	}
}

func TestOpenPasswordWithSpecialCharacters(t *testing.T) {
	pool, err := db.Open(context.Background(), roleURL(t, oddChars), nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
}

func TestOpenPasswordArgumentOverridesTheAddress(t *testing.T) {
	right := oddChars
	pool, err := db.Open(context.Background(), roleURL(t, "wrong"), &right)
	if err != nil {
		t.Fatalf("the argument must win over the address: %v", err)
	}
	pool.Close()

	wrong := "also-wrong"
	if pool, err := db.Open(context.Background(), roleURL(t, oddChars), &wrong); err == nil {
		pool.Close()
		t.Fatal("a wrong argument must not fall back to the address's password")
	}
}

func TestOpenConnectionErrorHasNoAddressOrPassword(t *testing.T) {
	address := roleURL(t, "wrong-"+oddChars)
	_, err := db.Open(context.Background(), address, nil)
	if err == nil {
		t.Fatal("want an authentication error")
	}
	u, _ := url.Parse(address)
	encoded, _ := u.User.Password()
	for _, leak := range []string{oddChars, "wrong-p", url.QueryEscape(oddChars), encoded, address, u.Host, u.User.Username()} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q contains %q", err, leak)
		}
	}
	if !strings.Contains(err.Error(), "28P01") {
		t.Fatalf("the label should carry the SQLSTATE: %q", err)
	}
}

func TestOpenClassifiesARefusedConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // nothing listens there now
	_, err = db.Open(context.Background(), fmt.Sprintf("postgres://hidden_user:hidden_pw@127.0.0.1:%d/hidden_db?sslmode=disable", port), nil)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("want a refused classification, got %v", err)
	}
	for _, leak := range []string{"hidden_user", "hidden_pw", "hidden_db", "127.0.0.1", strconv.Itoa(port)} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q contains %q", err, leak)
		}
	}
}

func TestOpenClassifiesAnExceededDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := db.Open(ctx, pgtest.StartURL(t), nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want a timeout classification, got %v", err)
	}
}
