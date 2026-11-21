package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

// Route is one handler of the REST API. Path is exact ("/api/tokens") or has one parameter
// segment ("/api/tokens/{id}", "/api/users/{id}/disabled"); a parameter is the whole segment.
type Route struct {
	Method, Path string
	Handler      http.Handler
	// Unguarded opts this route out of the middleware of the prefixes above it, like a hono route
	// registered before .use(): the connections OAuth callback sits under a guarded prefix but
	// handles a missing session itself. Only a request that matches the route (method and path,
	// HEAD by a GET route) skips the guards; every other path under the prefix is still guarded.
	Unguarded bool
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

// entry is the handler of a route and whether it skips the prefix middleware.
type entry struct {
	h         http.Handler
	unguarded bool
}

type pattern struct {
	segments []string // literal text, or "" at the parameter position
	param    string   // the parameter's name, "" for an exact path
	at       int      // the parameter's segment index
	handlers map[string]entry
}

type router struct {
	exact    map[string]map[string]entry // path -> method -> handler
	params   []*pattern                  // in registration order
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
	rt := &router{exact: map[string]map[string]entry{}, prefixes: prefixes, notFound: notFound}
	for _, p := range prefixes {
		if !isClean(p.Path) || p.Path == "/" || strings.ContainsAny(p.Path, "{}") {
			panic(fmt.Sprintf("httpx: invalid prefix %q: a prefix is an absolute, clean path below the root without parameters", p.Path))
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
			byMethod = map[string]entry{}
			rt.exact[r.Path] = byMethod
		}
		if _, dup := byMethod[r.Method]; dup {
			panic(fmt.Sprintf("httpx: route %s %q registered twice", r.Method, r.Path))
		}
		byMethod[r.Method] = entry{r.Handler, r.Unguarded}
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
		pat = &pattern{segments: segs, param: name, at: at, handlers: map[string]entry{}}
		rt.params = append(rt.params, pat)
	}
	if _, dup := pat.handlers[r.Method]; dup {
		panic(fmt.Sprintf("httpx: route %s %q registered twice", r.Method, r.Path))
	}
	pat.handlers[r.Method] = entry{r.Handler, r.Unguarded}
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
	h, req, unguarded := rt.lookup(r)
	if h == nil {
		h = rt.notFound
	}
	if unguarded {
		h.ServeHTTP(w, req)
		return
	}
	raw := r.URL.EscapedPath()
	var guards []Middleware
	for _, p := range rt.prefixes {
		if underPrefix(raw, p.Path) {
			guards = append(guards, p.Middleware...)
		}
	}
	// The guards run before the route (or the not-found answer) whatever the path is.
	Chain(h, guards...).ServeHTTP(w, req)
}

// lookup finds the handler for r and the request to hand it (carrying the path parameter); nil
// when no route matches.
func (rt *router) lookup(r *http.Request) (h http.Handler, req *http.Request, unguarded bool) {
	raw := r.URL.EscapedPath()
	if !isClean(raw) {
		return nil, r, false
	}
	if e, ok := entryFor(rt.exact[raw], r.Method); ok {
		return e.h, r, e.unguarded
	}
	segs := strings.Split(raw[1:], "/")
	for _, p := range rt.params {
		if len(segs) != len(p.segments) {
			continue
		}
		e, ok := entryFor(p.handlers, r.Method)
		if !ok || !p.matches(segs) {
			continue
		}
		value, err := url.PathUnescape(segs[p.at])
		// An encoded dot segment ("%2e%2e") is clean as escaped but not as a value; a value that is
		// not UTF-8 is never a name of ours.
		if err != nil || !utf8.ValidString(value) || value == "." || value == ".." {
			continue
		}
		ctx := context.WithValue(r.Context(), paramKey{}, map[string]string{p.param: value})
		return e.h, r.WithContext(ctx), e.unguarded
	}
	return nil, r, false
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

// entryFor is the entry of method, a GET entry serving HEAD.
func entryFor(byMethod map[string]entry, method string) (entry, bool) {
	if e, ok := byMethod[method]; ok {
		return e, true
	}
	if method == http.MethodHead {
		e, ok := byMethod[http.MethodGet]
		return e, ok
	}
	return entry{}, false
}
