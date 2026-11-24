package http

import (
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

type createTokenRequest struct {
	Name *string `json:"name"`
}

type tokenSummary struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	RevokedAt  *string `json:"revokedAt"`
}

// createTokenResponse is the wire form of a created token: the one response that carries the
// token's plain value, filled from CreatedToken.Token by an explicit Reveal. It is built only to be
// written and is never kept or printed.
type createTokenResponse struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
	Token  string `json:"token"`
}

func (h *handlers) listTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	rows, err := app.ListTokens(r.Context(), h.d, p)
	if err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	out := make([]tokenSummary, 0, len(rows))
	for _, t := range rows {
		out = append(out, tokenSummary{
			ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: iso(t.CreatedAt),
			LastUsedAt: isoPtr(t.LastUsedAt), RevokedAt: isoPtr(t.RevokedAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) createToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var body createTokenRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	var f httpx.Fields
	name := ""
	if body.Name == nil {
		f.RequiredString("name", nil, 1, 100)
	} else {
		name = f.TrimmedString("name", *body.Name, 1, 100)
	}
	if err := f.Err(); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	t, err := app.CreateToken(r.Context(), h.d, p, name)
	if err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, createTokenResponse{ID: t.ID, Prefix: t.Prefix, Token: t.Token.Reveal()})
}

func (h *handlers) revokeToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := parseID(w, r, "token")
	if !ok {
		return
	}
	if err := app.RevokeToken(r.Context(), h.d, p, id); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	noContent(w)
}
