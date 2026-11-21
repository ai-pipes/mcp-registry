package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// Route is one handler of the REST API. Path is exact ("/api/tokens") or has one parameter
// segment ("/api/tokens/{id}", "/api/users/{id}/disabled"); a parameter is the whole segment.
type Route struct {
	Method, Path string
	Handler      http.Handler
}

// Prefix puts middleware in front of every path at or under Path ("/api/tokens" covers
// "/api/tokens", "/api/tokens/x/y", not "/api/tokensx"). It runs before the route is looked up,
// so an unknown path under a guarded prefix gets the guard's answer, not a 404.
type Prefix struct {
	Path       string
	Middleware []Middleware
}

type paramKey struct{}

// PathParam is the value of the {name} segment of the route that matched r, percent-decoded
// once; "" outside a route or for an unknown name.
func PathParam(r *http.Request, name string) string {
	p, _ := r.Context().Value(paramKey{}).(map[string]string)
	return p[name]
}

type pattern struct {
	segments []string // literal text, or "" at the parameter position
	param    string   // the parameter's name, "" for an exact path
	at       int      // the parameter's segment index
	handlers map[string]http.Handler
}

type router struct {
	exact    map[string]map[string]http.Handler // path -> method -> handler
	params   []*pattern                         // in registration order
	prefixes []Prefix
	notFound http.Handler
}

// NewRouter dispatches by method and path with the behaviour of the TypeScript router rather
// than net/http's: another method on a known path is the ordinary not-found (no 405, no Allow),
// a path that is not clean ("//x", "/x/", "/a/../b") is not found and never redirected, HEAD is
// served by a GET route. Matching is on the escaped path, so "%2F" never separates segments and
// an encoded dot or slash cannot dodge a prefix. An exact route wins over a parameter route.
// Prefix middleware wraps everything under its path, outermost first in the order given.
//
// An invalid registration (a path that is not absolute and clean, a second parameter, a
// parameter that is not a whole segment, a missing method or handler, the same method and shape
// twice) is a programming error and panics.
func NewRouter(routes []Route, prefixes []Prefix, notFound http.Handler) http.Handler {
	rt := &router{exact: map[string]map[string]http.Handler{}, prefixes: prefixes, notFound: notFound}
	for _, p := range prefixes {
		if !isClean(p.Path) || strings.ContainsAny(p.Path, "{}") {
			panic(fmt.Sprintf("httpx: invalid prefix %q", p.Path))
		}
	}
	for _, r := range routes {
		rt.add(r)
	}
	return rt
}

func isClean(p string) bool { return strings.HasPrefix(p, "/") && path.Clean(p) == p }

func (rt *router) add(r Route) {
	if r.Method == "" || r.Handler == nil || !isClean(r.Path) {
		panic(fmt.Sprintf("httpx: invalid route %s %q", r.Method, r.Path))
	}
	segs := strings.Split(r.Path[1:], "/")
	if r.Path == "/" {
		segs = []string{""}
	}
	at, name := -1, ""
	for i, s := range segs {
		if !strings.ContainsAny(s, "{}") {
			continue
		}
		if at >= 0 || len(s) < 3 || s[0] != '{' || s[len(s)-1] != '}' || strings.ContainsAny(s[1:len(s)-1], "{}") {
			panic(fmt.Sprintf("httpx: invalid route %s %q: one whole-segment parameter at most", r.Method, r.Path))
		}
		at, name, segs[i] = i, s[1:len(s)-1], ""
	}
	if at < 0 {
		byMethod := rt.exact[r.Path]
		if byMethod == nil {
			byMethod = map[string]http.Handler{}
			rt.exact[r.Path] = byMethod
		}
		if byMethod[r.Method] != nil {
			panic(fmt.Sprintf("httpx: route %s %q registered twice", r.Method, r.Path))
		}
		byMethod[r.Method] = r.Handler
		return
	}
	var pat *pattern
	for _, p := range rt.params {
		if p.at == at && sameSegments(p.segments, segs) {
			pat = p
			if p.param != name {
				panic(fmt.Sprintf("httpx: route %s %q: the parameter is named {%s} elsewhere", r.Method, r.Path, p.param))
			}
		}
	}
	if pat == nil {
		pat = &pattern{segments: segs, param: name, at: at, handlers: map[string]http.Handler{}}
		rt.params = append(rt.params, pat)
	}
	if pat.handlers[r.Method] != nil {
		panic(fmt.Sprintf("httpx: route %s %q registered twice", r.Method, r.Path))
	}
	pat.handlers[r.Method] = r.Handler
}

func sameSegments(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func underPrefix(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.EscapedPath()
	var guards []Middleware
	for _, p := range rt.prefixes {
		if underPrefix(raw, p.Path) {
			guards = append(guards, p.Middleware...)
		}
	}
	Chain(http.HandlerFunc(rt.route), guards...).ServeHTTP(w, r)
}

// route looks the request up and serves it, or answers not found.
func (rt *router) route(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.EscapedPath()
	if !isClean(raw) {
		rt.notFound.ServeHTTP(w, r)
		return
	}
	if h := handlerFor(rt.exact[raw], r.Method); h != nil {
		h.ServeHTTP(w, r)
		return
	}
	segs := strings.Split(raw[1:], "/")
	for _, p := range rt.params {
		if len(segs) != len(p.segments) {
			continue
		}
		h := handlerFor(p.handlers, r.Method)
		if h == nil || !p.matches(segs) {
			continue
		}
		value, err := url.PathUnescape(segs[p.at])
		if err != nil {
			continue
		}
		ctx := context.WithValue(r.Context(), paramKey{}, map[string]string{p.param: value})
		h.ServeHTTP(w, r.WithContext(ctx))
		return
	}
	rt.notFound.ServeHTTP(w, r)
}

func (p *pattern) matches(segs []string) bool {
	for i, s := range p.segments {
		if i == p.at {
			if segs[i] == "" {
				return false
			}
			continue
		}
		if s != segs[i] {
			return false
		}
	}
	return true
}

// handlerFor is the handler of method, a GET handler serving HEAD.
func handlerFor(byMethod map[string]http.Handler, method string) http.Handler {
	if h := byMethod[method]; h != nil {
		return h
	}
	if method == http.MethodHead {
		return byMethod[http.MethodGet]
	}
	return nil
}
