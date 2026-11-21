package http

import (
	"net/http"
	"time"
)

const (
	// SessionCookieName is the web session cookie. The identity root package repeats the name and
	// the length limit for reading it (this layer may not import the root); a test pins that they agree.
	SessionCookieName = "mcp_session"
	// MaxSessionIDLength is the longest session cookie value looked up; a longer one counts as absent.
	MaxSessionIDLength = 64

	loginCookieName   = "mcp_login"
	loginCookieMaxAge = 600
	loginCookiePath   = "/api/auth"
)

var epoch = time.Unix(0, 0).UTC()

// session cookie: HttpOnly, SameSite=Strict, Path=/, no expiry (a browser-session cookie).
func sessionCookie(value string, secure bool) *http.Cookie {
	return &http.Cookie{Name: SessionCookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure} //nolint:gosec // G124: Secure follows COOKIE_SECURE, which the quick start on plain http sets to false; the other attributes are fixed
}

func setSessionCookie(w http.ResponseWriter, id string, secure bool) {
	http.SetCookie(w, sessionCookie(id, secure))
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	c := sessionCookie("", secure) //nolint:gosec // G124: see sessionCookie
	c.MaxAge, c.Expires = -1, epoch
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
	c.MaxAge, c.Expires = -1, epoch
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

// sessionIDOf is the session cookie's value unless absent, empty or longer than MaxSessionIDLength.
func sessionIDOf(r *http.Request) (string, bool) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" || len(c.Value) > MaxSessionIDLength {
		return "", false
	}
	return c.Value, true
}
