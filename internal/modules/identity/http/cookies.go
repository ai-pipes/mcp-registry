package http

import (
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

const (
	loginCookieName   = "mcp_login"
	loginCookieMaxAge = 600
	loginCookiePath   = "/api/auth"
)

// session cookie: HttpOnly, SameSite=Strict, Path=/, no expiry (a browser-session cookie).
func sessionCookie(value string, secure bool) *http.Cookie {
	return &http.Cookie{Name: identity.SessionCookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure} //nolint:gosec // G124: Secure follows COOKIE_SECURE, which the quick start on plain http sets to false; the other attributes are fixed
}

func setSessionCookie(w http.ResponseWriter, id string, secure bool) {
	http.SetCookie(w, sessionCookie(id, secure))
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	c := sessionCookie("", secure) //nolint:gosec // G124: see sessionCookie
	c.MaxAge = -1
	http.SetCookie(w, c)
}

// login binding cookie: Lax, because the callback is a cross-site top-level redirect from the
// provider and Strict would drop it; scoped to /api/auth; ten minutes.
func loginCookie(value string, secure bool) *http.Cookie {
	return &http.Cookie{Name: loginCookieName, Value: value, Path: loginCookiePath, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure} //nolint:gosec // G124: Secure follows COOKIE_SECURE, which the quick start on plain http sets to false; the other attributes are fixed
}

func setLoginBindingCookie(w http.ResponseWriter, state string, secure bool) {
	c := loginCookie(state, secure) //nolint:gosec // G124: see loginCookie
	c.MaxAge = loginCookieMaxAge
	http.SetCookie(w, c)
}

func clearLoginBindingCookie(w http.ResponseWriter, secure bool) {
	c := loginCookie("", secure) //nolint:gosec // G124: see loginCookie
	c.MaxAge = -1
	http.SetCookie(w, c)
}

// loginBindingOf is the binding cookie's value; ok is false when it is absent.
func loginBindingOf(r *http.Request) (string, bool) {
	c, err := r.Cookie(loginCookieName)
	if err != nil {
		return "", false
	}
	return c.Value, true
}
