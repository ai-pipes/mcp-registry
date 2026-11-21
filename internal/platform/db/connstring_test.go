package db

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	testUser = "app"
	testWord = "ZZ-pw"
)

var base = "postgres://" + testUser + ":" + testWord + "@db.example:5433/appdb"

// tlsShape describes how pgx will try TLS for a parsed configuration.
func tlsShape(t *testing.T, url string) string {
	t.Helper()
	cfg, err := parseConfig(url)
	if err != nil {
		t.Fatalf("%v", err)
	}
	c := cfg.ConnConfig
	verified := func(t *tls.Config) bool { return t != nil && !t.InsecureSkipVerify }
	switch {
	case c.TLSConfig == nil && len(c.Fallbacks) == 0:
		return "plain"
	case c.TLSConfig == nil:
		return "plain, then tls"
	case len(c.Fallbacks) > 0 && c.Fallbacks[len(c.Fallbacks)-1].TLSConfig == nil:
		return "tls unverified, then plain"
	case verified(c.TLSConfig):
		return "tls verified"
	default:
		return "tls unverified"
	}
}

func TestParseConfigTranslatesWhatTheNodeClientAccepts(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		check       func(t *testing.T, c *pgx.ConnConfig)
	}{
		{"prepare=false is the unnamed-statement mode", "?prepare=false", func(t *testing.T, c *pgx.ConnConfig) {
			if c.DefaultQueryExecMode != pgx.QueryExecModeExec {
				t.Fatalf("mode %v", c.DefaultQueryExecMode)
			}
		}},
		{"prepare=disable too", "?prepare=disable", func(t *testing.T, c *pgx.ConnConfig) {
			if c.DefaultQueryExecMode != pgx.QueryExecModeExec {
				t.Fatalf("mode %v", c.DefaultQueryExecMode)
			}
		}},
		{"prepare=true keeps the default", "?prepare=true", func(t *testing.T, c *pgx.ConnConfig) {
			if c.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
				t.Fatalf("mode %v", c.DefaultQueryExecMode)
			}
		}},
		{"connect_timeout in seconds", "?connect_timeout=7", func(t *testing.T, c *pgx.ConnConfig) {
			if c.ConnectTimeout != 7*time.Second {
				t.Fatalf("timeout %v", c.ConnectTimeout)
			}
		}},
		{"a fractional connect_timeout is rounded down", "?connect_timeout=2.9", func(t *testing.T, c *pgx.ConnConfig) {
			if c.ConnectTimeout != 2*time.Second {
				t.Fatalf("timeout %v", c.ConnectTimeout)
			}
		}},
		{"a server parameter stays a server parameter", "?application_name=mine&prepare=false", func(t *testing.T, c *pgx.ConnConfig) {
			if c.RuntimeParams["application_name"] != "mine" {
				t.Fatalf("params %v", c.RuntimeParams)
			}
		}},
		{"the address is untouched", "?max=5&idle_timeout=20", func(t *testing.T, c *pgx.ConnConfig) {
			if c.Host != "db.example" || c.Port != 5433 || c.User != testUser || c.Password != testWord || c.Database != "appdb" {
				t.Fatalf("connection: %s:%d %s %s", c.Host, c.Port, c.User, c.Database)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig(base + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, cfg.ConnConfig)
		})
	}
}

func TestParseConfigDropsEveryClientOnlyParameter(t *testing.T) {
	for k := range droppedByName {
		cfg, err := parseConfig(base + "?" + k + "=5&application_name=mine")
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if _, leaked := cfg.ConnConfig.RuntimeParams[k]; leaked {
			t.Errorf("%s would be sent to the server as a runtime parameter", k)
		}
		if cfg.ConnConfig.RuntimeParams["application_name"] != "mine" {
			t.Errorf("%s: the other parameters must survive: %v", k, cfg.ConnConfig.RuntimeParams)
		}
	}
	// The usual combination from a PgBouncer setup, whole.
	cfg, err := parseConfig(base + "?prepare=false&max=5&idle_timeout=20&max_lifetime=1800&fetch_types=false&keep_alive=30&backoff=1&connect_timeout=10&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ConnConfig.RuntimeParams) != 0 {
		t.Fatalf("runtime parameters left: %v", cfg.ConnConfig.RuntimeParams)
	}
}

func TestParseConfigSSLValuesOfTheNodeClient(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{"", "tls unverified, then plain"}, // pgx's own default: prefer
		{"?ssl=false", "plain"},
		{"?ssl=disable", "plain"},
		{"?sslmode=disable", "plain"},
		{"?ssl=", "plain"},
		{"?ssl=prefer", "tls unverified, then plain"},
		{"?sslmode=prefer", "tls unverified, then plain"},
		{"?ssl=allow", "tls unverified, then plain"}, // the Node client tries TLS first for allow too
		{"?ssl=require", "tls unverified"},
		{"?sslmode=require", "tls unverified"},
		{"?ssl=true", "tls verified"},
		{"?ssl=verify-full", "tls verified"},
		{"?sslmode=verify-ca", "tls verified"},
		{"?ssl=disable&sslmode=require", "tls unverified"}, // sslmode wins, as in the Node client
		{"?ssl=require&sslmode=", "tls unverified"},
		{"?sslrootcert=system", "tls verified"},
	} {
		if got := tlsShape(t, base+tc.query); got != tc.want {
			t.Errorf("%q: %s, want %s", tc.query, got, tc.want)
		}
	}
}

func TestParseConfigPoolSizeIsTheConstant(t *testing.T) {
	for _, q := range []string{"", "?max=3", "?pool_max_conns=3", "?max=3&pool_max_conns=4"} {
		cfg, err := parseConfig(base + q)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MaxConns != PoolMaxConns {
			t.Errorf("%q: pool size %d", q, cfg.MaxConns)
		}
	}
}

func TestParseConfigLeavesOtherFormsAlone(t *testing.T) {
	for _, s := range []string{base, base + "?application_name=x", "host=h user=u dbname=d"} {
		if out, noPrepare := translateQuery(s); out != s || noPrepare {
			t.Errorf("%q became %q", s, out)
		}
	}
	// An unreadable segment is left for pgx to refuse; the others are still processed.
	out, _ := translateQuery(base + "?x=%zz&max=5")
	if out != base+"?x=%zz" {
		t.Errorf("got %q", out)
	}
}
