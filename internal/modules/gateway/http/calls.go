package http

import (
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// callEntry is one entry of CallList (packages/contracts/src/calls.ts): never arguments or results.
type callEntry struct {
	ID         string  `json:"id"`
	UserID     string  `json:"userId"`
	TokenID    *string `json:"tokenId"`
	Server     string  `json:"server"`
	Tool       string  `json:"tool"`
	StartedAt  string  `json:"startedAt"`
	DurationMs int64   `json:"durationMs"`
	Outcome    string  `json:"outcome"`
}

// callsHandler is `GET /api/calls` (calls.ts): the call log for administrators, newest first. The
// session and admin guards are the prefix middleware, so they answer before the query is read.
type callsHandler struct {
	g  Guards
	gw Gateway
}

func outcomeNames() []string {
	all := gateway.AllOutcomes()
	out := make([]string, len(all))
	for i, o := range all {
		out[i] = string(o)
	}
	return out
}

func (h *callsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := h.g.Principal(r)
	if !ok {
		httpx.WriteError(w, "unauthenticated", "sign in required")
		return
	}
	// The query as the reference reads it (Hono: the first occurrence of a name, + a space, ; data),
	// checked in the order of the zod schema's keys; the first problem is the 422.
	q := httpx.QueryValues(r.URL.RawQuery)
	var f httpx.Fields
	user, hasUser := f.QueryUUID("user", q, "user")
	server := f.OptionalQueryString("server", q, "server", 1, 200)
	outcome, hasOutcome := f.QueryEnum("outcome", q, "outcome", outcomeNames())
	before, hasBefore := f.QueryOffsetDateTime("before", q, "before")
	beforeID, hasBeforeID := f.QueryDigits("beforeId", q, "beforeId", 18)
	limit := f.QueryInt("limit", q, "limit", 1, 500, gateway.DefaultCallsLimit)
	f.Check(!hasBeforeID || hasBefore, "beforeId", "needs before")
	if err := f.Err(); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	query := gateway.ListCallsQuery{Principal: p, Server: server, Limit: limit}
	if hasUser {
		query.UserID = &user
	}
	if hasOutcome {
		o, _ := gateway.ParseOutcome(outcome) // one of AllOutcomes
		query.Outcome = &o
	}
	if hasBefore {
		query.Before = &before
	}
	if hasBeforeID {
		query.BeforeID = &beforeID
	}
	rows, err := h.gw.ListCalls(r.Context(), query)
	if err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	out := make([]callEntry, 0, len(rows))
	for _, e := range rows {
		c := callEntry{
			ID: e.ID, UserID: e.UserID, Server: e.Server, Tool: e.Tool,
			StartedAt:  e.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			DurationMs: e.DurationMs, Outcome: string(e.Outcome),
		}
		if e.TokenID != "" {
			token := e.TokenID
			c.TokenID = &token
		}
		out = append(out, c)
	}
	body, err := marshalJS(out)
	if err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}
