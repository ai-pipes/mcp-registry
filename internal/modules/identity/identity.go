package identity

import (
	"context"
	"net/http"
	"strconv"
	"unicode"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// Principal is who is making a request.
type Principal = domain.Principal

const (
	// SessionCookieName is the web session cookie; identity/http sets and clears it under this name.
	SessionCookieName = "mcp_session"
	// MaxSessionIDLength is the longest session cookie value looked up; a longer one counts as absent.
	MaxSessionIDLength = 64
)

type principalKey struct{}

// PrincipalOf is the principal RequireSession or RequireBearerToken stored for the request.
func PrincipalOf(r *http.Request) (Principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(Principal)
	return p, ok
}

func withPrincipal(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

func hasAuthorization(r *http.Request) bool { return len(r.Header.Values("Authorization")) > 0 }

// SessionIDOf is the session cookie's value unless it is absent, empty or longer than
// MaxSessionIDLength (such a value is treated as absent and never looked up).
func SessionIDOf(r *http.Request) (string, bool) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" || len(c.Value) > MaxSessionIDLength {
		return "", false
	}
	return c.Value, true
}

// SessionPrincipal is the person behind the session cookie, for routes that answer a missing
// session with a redirect rather than 401 (provider callbacks). A request carrying an
// Authorization header gets no principal. err is a store failure, which is not "signed out".
func SessionPrincipal(d app.Deps, r *http.Request) (Principal, bool, error) {
	if hasAuthorization(r) {
		return Principal{}, false, nil
	}
	id, ok := SessionIDOf(r)
	if !ok {
		return Principal{}, false, nil
	}
	p, err := app.AuthenticateSession(r.Context(), d, id)
	if err != nil || p == nil {
		return Principal{}, false, err
	}
	return *p, true, nil
}

// RequireSession is the guard of /api/**: the session cookie is the only credential. Any
// Authorization header, an sk_ token in particular, is refused even next to a valid cookie.
func RequireSession(d app.Deps) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hasAuthorization(r) {
				httpx.WriteError(w, "unauthenticated", "tokens do not work on the web API")
				return
			}
			p, ok, err := SessionPrincipal(d, r)
			if err != nil {
				httpx.WriteDomainError(w, r, err)
				return
			}
			if !ok {
				httpx.WriteError(w, "unauthenticated", "sign in required")
				return
			}
			next.ServeHTTP(w, withPrincipal(r, p))
		})
	}
}

// RequireAdmin runs after RequireSession: only admins pass, before any body or id is parsed.
func RequireAdmin() httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalOf(r)
			if !ok {
				httpx.WriteError(w, "unauthenticated", "sign in required")
				return
			}
			if !domain.IsAdmin(p) {
				httpx.WriteError(w, "forbidden", "admin only")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isJSSpace is JavaScript's \s (what the TypeScript regex's \S excludes).
func isJSSpace(r rune) bool {
	return (unicode.IsSpace(r) && r != '\u0085') || r == 0xFEFF
}

// bearerToken is /^Bearer[ \t]+(\S+)$/i: the scheme in any case, blanks, then one token without
// whitespace and nothing after it.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer"
	if len(header) < len(scheme) || !equalFoldASCII(header[:len(scheme)], scheme) {
		return "", false
	}
	rest := header[len(scheme):]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	token := rest[i:]
	if i == 0 || token == "" {
		return "", false
	}
	for _, r := range token {
		if isJSSpace(r) {
			return "", false
		}
	}
	return token, true
}

func equalFoldASCII(a, b string) bool {
	for i := range len(a) {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != b[i] {
			return false
		}
	}
	return true
}

// RequireBearerToken is the guard of /mcp: "Authorization: Bearer sk_..." is the only credential,
// the cookie is ignored. A missing or malformed header is 401 unauthenticated and is not counted by
// the failed-attempts limiter; a bad token is 401 invalid_token (both with WWW-Authenticate:
// Bearer); a refused client address is 429 rate_limited with one Retry-After and no
// WWW-Authenticate (decided deviation token-429-retry-after). The client address is
// httpx.ClientIPOf.
func RequireBearerToken(d app.Deps) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpx.WriteError(w, "unauthenticated", "bearer token required")
				return
			}
			p, err := app.AuthenticateToken(r.Context(), d, token, httpx.ClientIPOf(r))
			if err != nil {
				de, isDomain := domain.AsError(err)
				switch {
				case !isDomain:
					httpx.WriteDomainError(w, r, err)
				case de.Code == domain.CodeRateLimited:
					w.Header().Set("Retry-After", strconv.Itoa(max(de.RetryAfterSec, 1)))
					httpx.WriteError(w, de.Code, de.Message)
				default:
					w.Header().Set("WWW-Authenticate", "Bearer")
					httpx.WriteError(w, de.Code, de.Message)
				}
				return
			}
			next.ServeHTTP(w, withPrincipal(r, p))
		})
	}
}

// IsUserActiveAdmin reports whether the person is still an enabled admin, for work done on an
// admin's behalf without a session, such as scheduled catalog sync.
func IsUserActiveAdmin(ctx context.Context, d app.Deps, userID string) (bool, error) {
	return app.IsUserActiveAdmin(ctx, d, userID)
}
