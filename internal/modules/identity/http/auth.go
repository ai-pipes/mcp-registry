package http

import (
	"net/http"
	"net/url"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

type meResponse struct {
	UserID string   `json:"userId"`
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Groups []string `json:"groups"`
}

type linkLoginRequest struct {
	Code *string `json:"code"`
}

func (h *handlers) methods(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"oidc": h.o.OIDCConfigured})
}

// linkStatus is the status of each way a link sign-in can fail; this route's own table (in the
// generic one disabled would be 400 and invalid_link 422). Any other code is 400.
func linkStatus(code string) int {
	switch code {
	case domain.CodeInvalidLink:
		return http.StatusUnauthorized
	case domain.CodeDisabled:
		return http.StatusForbidden
	case domain.CodeRateLimited:
		return http.StatusTooManyRequests
	}
	return http.StatusBadRequest
}

// link redeems a one-time code. The body is validated first (a refused body spends neither the code
// nor a limiter attempt).
func (h *handlers) link(w http.ResponseWriter, r *http.Request) {
	var body linkLoginRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	var f httpx.Fields
	code := f.RequiredString("code", body.Code, 1, 100)
	if err := f.Err(); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	sessionID, err := app.CompleteLinkLogin(r.Context(), h.d, app.CompleteLinkLoginInput{Code: code, IP: httpx.ClientIPOf(r)})
	if err != nil {
		de, ok := domain.AsError(err)
		if !ok {
			httpx.WriteDomainError(w, r, err)
			return
		}
		if de.Code == domain.CodeRateLimited && de.RetryAfterSec > 0 {
			w.Header().Set("Retry-After", itoa(de.RetryAfterSec))
		}
		// The message is fixed text: it never carries the code.
		writeJSON(w, linkStatus(de.Code), map[string]any{"error": map[string]string{"code": de.Code, "message": de.Message}})
		return
	}
	// The person signed in with a link: the session this browser held is ended, not left alive.
	if replaced, ok := identity.SessionIDOf(r); ok {
		if err := app.Logout(r.Context(), h.d, replaced); err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
	}
	setSessionCookie(w, sessionID, h.o.CookieSecure)
	noContent(w)
}

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	if !h.o.OIDCConfigured {
		redirect(w, "/?loginError=oidc_not_configured")
		return
	}
	var f httpx.Fields
	q := r.URL.Query()
	returnTo := "/"
	if q.Has("returnTo") {
		returnTo = f.String("returnTo", q.Get("returnTo"), 0, 2048)
	}
	if err := f.Err(); err != nil {
		httpx.WriteDomainError(w, r, err)
		return
	}
	authURL, err := app.StartOIDCLogin(r.Context(), h.d, app.StartOIDCLoginInput{ReturnTo: returnTo})
	if err != nil {
		// Only an unreachable provider goes back to the UI with a fixed code; anything else is a 500.
		if de, ok := kernel.AsDomainError(err); ok && de.Code == domain.CodeIDPUnavailable {
			redirect(w, "/?loginError="+domain.CodeIDPUnavailable)
			return
		}
		httpx.WriteDomainError(w, r, err)
		return
	}
	// Binds this attempt to this browser: the callback must come back with the same cookie.
	setLoginBindingCookie(w, authURL.Query().Get("state"), h.o.CookieSecure)
	redirect(w, authURL.String())
}

func (h *handlers) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f httpx.Fields
	state := f.String("state", q.Get("state"), 1, 256)
	code := f.String("code", q.Get("code"), 1, 4096)
	var iss *string
	if q.Has("iss") {
		v := q.Get("iss")
		iss = f.OptionalString("iss", &v, 2048)
	}
	bound, hasBinding := loginBindingOf(r)
	clearLoginBindingCookie(w, h.o.CookieSecure)
	// A cancelled or broken provider answer carries no usable code or state; the attempt is not consumed.
	if f.Err() != nil || !hasBinding || bound != state {
		redirect(w, "/?loginError="+domain.CodeInvalidState)
		return
	}
	res, err := app.CompleteOIDCLogin(r.Context(), h.d, app.CompleteOIDCLoginInput{State: state, Code: code, Iss: iss, IP: httpx.ClientIPOf(r)})
	if err != nil {
		// The browser came back from the provider: a page with a fixed code, not a JSON 500.
		if de, ok := kernel.AsDomainError(err); ok {
			redirect(w, "/?loginError="+url.QueryEscape(de.Code))
			return
		}
		h.o.Logger.Error("auth: login callback failed", "reqId", httpx.RequestIDOf(r), logging.ErrAttr(err))
		redirect(w, "/?loginError=internal")
		return
	}
	setSessionCookie(w, res.SessionID, h.o.CookieSecure)
	redirect(w, res.ReturnTo)
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	if id, ok := identity.SessionIDOf(r); ok {
		if err := app.Logout(r.Context(), h.d, id); err != nil {
			httpx.WriteDomainError(w, r, err)
			return
		}
	}
	clearSessionCookie(w, h.o.CookieSecure)
	noContent(w)
}

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	groups := p.Groups
	if groups == nil {
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, meResponse{UserID: p.UserID, Name: p.Name, Role: string(p.Role), Groups: groups})
}
