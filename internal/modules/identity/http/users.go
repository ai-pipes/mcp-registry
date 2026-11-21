package http

import (
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

type userSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Email       *string  `json:"email"`
	Role        string   `json:"role"`
	Groups      []string `json:"groups"`
	Disabled    bool     `json:"disabled"`
	CreatedAt   string   `json:"createdAt"`
	LastLoginAt *string  `json:"lastLoginAt"`
}

type setDisabledRequest struct {
	Disabled *bool `json:"disabled"`
}

func (h *handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	rows, err := app.ListUsers(r.Context(), h.d, p)
	if err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	out := make([]userSummary, 0, len(rows))
	for _, u := range rows {
		groups := u.Groups
		if groups == nil {
			groups = []string{}
		}
		out = append(out, userSummary{
			ID: u.ID, Name: u.Name, Email: u.Email, Role: string(u.Role), Groups: groups,
			Disabled: u.Disabled, CreatedAt: iso(u.CreatedAt), LastLoginAt: isoPtr(u.LastLoginAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// setDisabled checks in this order: the id (404), the body (422), then the use case.
func (h *handlers) setDisabled(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := parseID(w, r, "user")
	if !ok {
		return
	}
	var body setDisabledRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	var f httpx.Fields
	disabled := f.Bool("disabled", body.Disabled)
	if err := f.Err(); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	if err := app.SetDisabled(r.Context(), h.d, app.SetDisabledInput{Principal: p, UserID: id, Disabled: disabled}); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	noContent(w)
}
