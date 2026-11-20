package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/db"
)

// A malformed address is refused before any connection is tried, so this needs no database.
func TestOpenParseErrorNeverQuotesTheAddress(t *testing.T) {
	const secret = "pa55w0rd-Zq"
	for _, url := range []string{
		"postgres://user:" + secret + "@host:notaport/db",
		"postgres://user:" + secret + "@host/db?sslmode=bogus-mode",
		"postgres://user:%zz" + secret + "@host/db",
	} {
		pool, err := db.Open(context.Background(), url, nil)
		if err == nil {
			pool.Close()
			t.Fatalf("%q: want an error", url)
		}
		for _, leak := range []string{secret, "postgres://", "host", "bogus-mode", "notaport"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error %q contains %q", err, leak)
			}
		}
	}
}
