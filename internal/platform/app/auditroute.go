package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditEntry is one record of AuditList (packages/contracts/src/audit.ts).
type auditEntry struct {
	ID      string          `json:"id"`
	At      string          `json:"at"`
	ActorID *string         `json:"actorId"`
	Action  string          `json:"action"`
	Target  *string         `json:"target"`
	Meta    json.RawMessage `json:"meta"`
}

// auditHandler is `GET /api/audit` (v1.0.1:apps/server/src/platform/http/audit-route.ts): what admins did, newest first. It reads
// platform's own table; meta carries no secret values. The session and admin guards are the prefix
// middleware of the platform module, so they run before the query is parsed.
func auditHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var f httpx.Fields
		before, hasBefore := f.QueryDigits("before", q, "before", 18)
		limit := f.QueryInt("limit", q, "limit", 1, 500, 100)
		if err := f.Err(); err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
		// Qualified audit_log.id: the order is the numeric column, not the text alias of the same
		// name (the 1.0.1 fix).
		var beforeArg *string
		if hasBefore {
			beforeArg = &before
		}
		rows, err := pool.Query(r.Context(), `select id::text as id, at, actor_id::text, action, target, meta::text
			from platform.audit_log
			where ($1::text is null or id < $1::text::bigint)
			order by audit_log.id desc
			limit $2`, beforeArg, limit)
		if err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
		defer rows.Close()
		out := []auditEntry{}
		for rows.Next() {
			var (
				e    auditEntry
				at   time.Time
				meta *string
			)
			if err := rows.Scan(&e.ID, &at, &e.ActorID, &e.Action, &e.Target, &meta); err != nil {
				httpx.WriteDomainError(w, r, err)
				return
			}
			e.At = at.UTC().Format("2006-01-02T15:04:05.000Z")
			e.Meta = json.RawMessage("{}")
			if meta != nil && len(*meta) > 0 && (*meta)[0] == '{' {
				e.Meta = json.RawMessage(*meta)
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
	})
}
