package http_test

import (
	"context"
	"net/http"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// The real guards live in the identity root package, which this layer (tests included) may not
// import; the root's own tests pin them. These stand-ins have the same observable behaviour for the
// routes under test: Authorization header -> 401, cookie of at most 64 characters, admin only.

type principalKey struct{}

func principalOf(r *http.Request) (domain.Principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(domain.Principal)
	return p, ok
}

func requireSession(d app.Deps) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.Header.Values("Authorization")) > 0 {
				httpx.WriteError(w, "unauthenticated", "tokens do not work on the web API")
				return
			}
			c, err := r.Cookie("mcp_session")
			if err != nil || c.Value == "" || len(c.Value) > 64 {
				httpx.WriteError(w, "unauthenticated", "sign in required")
				return
			}
			p, err := app.AuthenticateSession(r.Context(), d, c.Value)
			if err != nil || p == nil {
				httpx.WriteError(w, "unauthenticated", "sign in required")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, *p)))
		})
	}
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, _ := principalOf(r); p.Role != domain.RoleAdmin {
			httpx.WriteError(w, "forbidden", "admin only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// reqCookie is a cookie as a browser sends it back: a name and a value only.
func reqCookie(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value} //nolint:gosec // G124: a request cookie carries no attributes
}
