package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Route is one handler of the REST API. Path is exact ("/api/tokens") or has one parameter
// segment ("/api/tokens/{id}", "/api/users/{id}/disabled"); a parameter is the whole segment.
type Route struct {
	Method, Path string
	Handler      http.Handler
	// Unguarded opts this route out of EVERY enclosing prefix's middleware, also that of an outer
	// prefix registered by another module. Only a request that matches the route (method and
	// path, HEAD by a GET route) skips the guards; every other path under the prefixes, and
	// another method on the same path, is still guarded. For a route that sits under a guarded
	// prefix but handles a missing session itself (the connections OAuth callback).
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
	segments []string // literal (escaped) text, or "" at a parameter position
	names    []string // the parameter's name at a parameter position, "" at a literal one
	handlers map[string]entry
}

// moreSpecific reports whether a ranks before b (same length): at the first position where one
// has a literal and the other a parameter, the literal wins.
func moreSpecific(a, b *pattern) bool {
	for i := range a.segments {
		if (a.names[i] == "") != (b.names[i] == "") {
			return a.names[i] == ""
		}
	}
	return false
}

type router struct {
	exact    map[string]map[string]entry // path -> method -> handler
	params   []*pattern                  // most specific first
	prefixes []Prefix
	notFound http.Handler
}

// NewRouter dispatches by method and path with the behaviour of the TypeScript router rather
// than net/http's: another method on a known path is the ordinary not-found (no 405, no Allow),
// a path that is not clean ("//x", "/x/", "/a/../b") is not found and never redirected, HEAD is
// served by a GET route. Matching is on the escaped path, so "%2F" never separates segments and
// an encoded dot or slash cannot dodge a prefix.
//
// A route path has any number of {name} parameters, each a whole segment
// ("/api/servers/{slug}/tools/{name}/hidden"); a value is a non-empty segment of the escaped
// path, percent-decoded once, and a value that decodes to "." or ".." or is not valid UTF-8
// makes the request not found.
//
// Which route answers: an exact route wins over every parameter route. Among parameter routes
// with the same number of segments the MOST SPECIFIC wins, whatever the registration order
// (modules register independently): at the first segment where two patterns differ, the one
// with a literal beats the one with a parameter ("/api/x/new" over "/api/x/{id}",
// "/api/servers/{slug}/tools" over "/api/servers/{slug}/{other}"). A pattern with no handler
// for the request's method does not hide a less specific one that has. Two patterns that are
// the same up to parameter names are one shape and registering both is an error.
//
// Prefix middleware wraps everything under its path, outermost first in the order given.
//
// An invalid registration (a path that is not absolute and clean, a parameter that is not a
// whole segment or has no name, the same name twice in one path, a missing method or handler,
// the same method and shape twice, one shape under two sets of names) is a programming error
// and panics.
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
	names := make([]string, len(segs))
	hasParam := false
	for i, s := range segs {
		if !strings.ContainsAny(s, "{}") {
			continue
		}
		if len(s) < 3 || s[0] != '{' || s[len(s)-1] != '}' || strings.ContainsAny(s[1:len(s)-1], "{}") {
			panic(fmt.Sprintf("httpx: invalid route %s %q: a parameter is a whole segment {name}", r.Method, r.Path))
		}
		name := s[1 : len(s)-1]
		for _, other := range names[:i] {
			if other == name {
				panic(fmt.Sprintf("httpx: invalid route %s %q: the parameter {%s} appears twice", r.Method, r.Path, name))
			}
		}
		hasParam = true
		names[i], segs[i] = name, ""
	}
	if !hasParam {
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
		if !slices.Equal(p.segments, segs) {
			continue
		}
		if !slices.Equal(p.names, names) {
			panic(fmt.Sprintf("httpx: route %s %q: the same path shape is registered with other parameter names", r.Method, r.Path))
		}
		pat = p
	}
	if pat == nil {
		pat = &pattern{segments: segs, names: names, handlers: map[string]entry{}}
		rt.params = append(rt.params, pat)
		// Shorter patterns first, then the more specific; lookup only meets equal lengths.
		sort.SliceStable(rt.params, func(i, j int) bool {
			a, b := rt.params[i], rt.params[j]
			if len(a.segments) != len(b.segments) {
				return len(a.segments) < len(b.segments)
			}
			return moreSpecific(a, b)
		})
	}
	if _, dup := pat.handlers[r.Method]; dup {
		panic(fmt.Sprintf("httpx: route %s %q registered twice", r.Method, r.Path))
	}
	pat.handlers[r.Method] = entry{r.Handler, r.Unguarded}
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
		if !ok {
			continue
		}
		values, ok := p.match(segs)
		if !ok {
			continue
		}
		ctx := context.WithValue(r.Context(), paramKey{}, values)
		return e.h, r.WithContext(ctx), e.unguarded
	}
	return nil, r, false
}

// match returns the parameter values of segs (escaped, same length as the pattern) or false: a
// literal differs, a parameter segment is empty, or its value is unusable. An encoded dot
// segment ("%2e%2e") is clean as escaped but not as a value, and a value that is not UTF-8 is
// never a name of ours.
func (p *pattern) match(segs []string) (map[string]string, bool) {
	values := map[string]string{}
	for i, s := range p.segments {
		if p.names[i] == "" {
			if s != segs[i] {
				return nil, false
			}
			continue
		}
		if segs[i] == "" {
			return nil, false
		}
		value, err := url.PathUnescape(segs[i])
		if err != nil || !utf8.ValidString(value) || value == "." || value == ".." {
			return nil, false
		}
		values[p.names[i]] = value
	}
	return values, true
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
