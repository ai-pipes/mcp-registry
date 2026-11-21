package db

import (
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// nodeClientOptions are the query parameters of DATABASE_URL that the Node client (postgres.js
// 4.x, `defaults` in src/index.js `parseOptions`) reads for itself and never sends to the server.
// pgx would pass the ones it does not know on as server runtime parameters, and Postgres refuses
// an unknown one (`unrecognized configuration parameter`), so a URL that works with the TypeScript
// server would stop the Go one. Those without an equivalent here are dropped; the ones with a
// clear meaning are translated (see translateQuery).
//
//   - dropped: max, min, idle_timeout, max_lifetime, max_pipeline, queue_timeout, query_timeout,
//     cancel_timeout, backoff, keep_alive, scheduler, debug, fetch_types, infer, publications, slot.
//   - translated: ssl / sslmode, prepare, connect_timeout, sslrootcert=system.
//   - left to pgx, which has the same parameter: host, port, sslrootcert, sslcert, sslkey,
//     sslnegotiation, channel_binding, target_session_attrs.
//
// Everything else (application_name, options, search_path, ...) is a server parameter in both.
var droppedByName = map[string]bool{
	"max": true, "min": true, "idle_timeout": true, "max_lifetime": true, "max_pipeline": true,
	"queue_timeout": true, "query_timeout": true, "cancel_timeout": true, "backoff": true,
	"keep_alive": true, "scheduler": true, "debug": true, "fetch_types": true, "infer": true,
	"publications": true, "slot": true,
}

// parseConfig is pgxpool.ParseConfig for a connection string written for the Node client: the
// client-only parameters are removed or translated first, `prepare=false` selects pgx's
// extended-protocol mode without prepared statements (what PgBouncer in transaction mode needs),
// and the pool size is the constant PoolMaxConns. The error is pgx's and may quote the string:
// callers must not log it.
func parseConfig(databaseURL string) (*pgxpool.Config, error) {
	translated, noPrepare := translateQuery(databaseURL)
	cfg, err := pgxpool.ParseConfig(translated)
	if err != nil {
		return nil, err
	}
	if noPrepare {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}
	cfg.MaxConns = PoolMaxConns
	return cfg, nil
}

// translateQuery rewrites the query of a postgres:// URL (other forms are returned unchanged) and
// reports whether prepare was switched off. Segments it does not touch are kept byte for byte.
func translateQuery(raw string) (out string, noPrepare bool) {
	if !strings.Contains(raw, "://") {
		return raw, false
	}
	base, query, ok := strings.Cut(raw, "?")
	if !ok || query == "" {
		return raw, false
	}
	var kept []string
	var ssl, sslmode, prepare, connectTimeout, rootCert string
	var hasSSL, hasSSLMode, hasPrepare, hasTimeout, hasFiles bool
	for _, seg := range strings.Split(query, "&") {
		key, val, _ := strings.Cut(seg, "=")
		k, kerr := url.QueryUnescape(key)
		v, verr := url.QueryUnescape(val)
		if kerr != nil || verr != nil {
			kept = append(kept, seg) // pgx will reject it, with its own (unlogged) message
			continue
		}
		switch {
		case droppedByName[k]:
		case k == "ssl":
			ssl, hasSSL = v, true
		case k == "sslmode":
			sslmode, hasSSLMode = v, v != "" // an empty value is unset, as in the Node client
		case k == "prepare":
			prepare, hasPrepare = v, true
		case k == "connect_timeout":
			connectTimeout, hasTimeout = v, true
		case k == "sslrootcert" && v == "system":
			rootCert = v // the Node client turns this into ssl=verify-full
		default:
			if k == "sslrootcert" || k == "sslcert" || k == "sslkey" {
				hasFiles = true
			}
			kept = append(kept, seg)
		}
	}
	// sslmode wins over ssl, as in the Node client; certificate files without either mean require.
	switch {
	case rootCert != "":
		kept = append(kept, "sslmode=verify-full")
	case hasSSLMode:
		kept = append(kept, "sslmode="+pgxSSLMode(sslmode))
	case hasSSL:
		kept = append(kept, "sslmode="+pgxSSLMode(ssl))
	case hasFiles:
		kept = append(kept, "sslmode=require")
	}
	if hasTimeout {
		// Seconds in both; the Node client takes any number, pgx an integer. Anything else is dropped.
		if f, err := strconv.ParseFloat(connectTimeout, 64); err == nil && f >= 0 && f <= math.MaxInt32 {
			kept = append(kept, "connect_timeout="+strconv.Itoa(int(f)))
		}
	}
	noPrepare = hasPrepare && (prepare == "false" || prepare == "disable")
	if len(kept) == 0 {
		return base, noPrepare
	}
	return base + "?" + strings.Join(kept, "&"), noPrepare
}

// pgxSSLMode maps a value of the Node client's ssl option. There, false and "disable" mean no TLS;
// "prefer" and "allow" try TLS and go on without it, without verifying the server; "require" needs
// TLS without verifying; any other value (true, "verify-full", "verify-ca", ...) is TLS with
// the default verification of the certificate and the host name.
func pgxSSLMode(v string) string {
	switch v {
	case "", "false", "disable":
		return "disable"
	case "prefer", "allow":
		return "prefer"
	case "require":
		return "require"
	}
	return "verify-full"
}
