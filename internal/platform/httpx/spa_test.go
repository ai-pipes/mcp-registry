package httpx_test

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

const shell = `<!doctype html><div id="root"></div><script src="/assets/app-abc.js"></script>`

// site lays out a web root next to a file that must never be served.
func site(t *testing.T) (webRoot string) {
	t.Helper()
	dir := t.TempDir()
	webRoot = filepath.Join(dir, "web")
	files := map[string]string{
		"web/index.html":        shell,
		"web/assets/app-abc.js": "console.log(1)",
		"web/assets/app.css":    "a{}",
		"web/assets/icon.svg":   "<svg/>",
		"web/assets/font.woff2": "wOF2",
		"web/assets/app.js.map": "{}",
		"web/data.json":         "{}",
		"web/favicon.svg":       "<svg/>",
		"secret.txt":            "TOP-SECRET",
		"web/assets/inner/x.js": "x",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return webRoot
}

// handler is Static then SPAFallback then NotFound: the order of the composition root.
func handler(webRoot string) http.Handler {
	return httpx.Static(webRoot, httpx.SPAFallback(webRoot, httpx.NotFound()))
}

func get(h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, kv := range hdr {
		k, v, _ := strings.Cut(kv, ": ")
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const html = "Accept: text/html"

// compat: TestPlatformStaticFilesAndSPA, "hashed assets are cached forever".
func TestAssetsAreCachedForever(t *testing.T) {
	h := handler(site(t))
	for path, ctype := range map[string]string{
		"/assets/app-abc.js": "text/javascript",
		"/assets/app.css":    "text/css",
		"/assets/icon.svg":   "image/svg+xml",
		"/assets/font.woff2": "font/woff2",
		"/assets/app.js.map": "application/json",
	} {
		w := get(h, "GET", path)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
		if got, _, _ := mime.ParseMediaType(w.Header().Get("Content-Type")); got != ctype {
			t.Errorf("%s: Content-Type %q, want %s", path, w.Header().Get("Content-Type"), ctype)
		}
	}
	if ct := get(h, "GET", "/assets/app-abc.js").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("js %q", ct)
	}
	if ct := get(h, "GET", "/data.json").Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("json %q", ct)
	}
}

// compat: "GET /" and index.html: no-cache, text/html utf-8.
func TestShellIsRevalidated(t *testing.T) {
	h := handler(site(t))
	for _, p := range []string{"/", "/index.html"} {
		w := get(h, "GET", p, html)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(w.Body.String(), `<div id="root">`) {
			t.Errorf("%s: %d %q %.60s", p, w.Code, w.Header().Get("Cache-Control"), w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", p, ct)
		}
	}
	// "/" is the shell with any Accept, like the reference's static middleware.
	if w := get(h, "GET", "/"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("/ without Accept: %d", w.Code)
	}
}

// compat: "a missing asset is a 404 that is not cached".
func TestMissIsAUncached404(t *testing.T) {
	h := handler(site(t))
	for _, p := range []string{"/assets/no-such-file-abc123.js", "/missing.js"} {
		w := get(h, "GET", p, html)
		cc := w.Header().Get("Cache-Control")
		if w.Code != 404 || cc != "" || w.Body.String() != "Not found" {
			t.Errorf("%s: %d Cache-Control %q body %q", p, w.Code, cc, w.Body)
		}
	}
}

// compat: "a client-side route gets index.html, never cached" (+ HEAD).
func TestClientRoutesGetTheShell(t *testing.T) {
	h := handler(site(t))
	for _, p := range []string{"/catalog", "/servers/gitea", "/admin/servers/new", "/connections", "/catalog/"} {
		w := get(h, "GET", p, html)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(w.Body.String(), `<div id="root">`) {
			t.Errorf("%s: %d %q %.60s", p, w.Code, w.Header().Get("Cache-Control"), w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", p, ct)
		}
	}
	w := get(h, "HEAD", "/catalog", html)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("HEAD /catalog: %d, %d body bytes", w.Code, w.Body.Len())
	}
	if w.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD has no Content-Length")
	}
	w = get(h, "HEAD", "/", html)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD /: %d, %d body bytes", w.Code, w.Body.Len())
	}
}

// compat: "without Accept text/html a route is a 404", "a path with a file extension is a 404",
// "only GET and HEAD get the shell".
func TestWhenTheShellIsNotServed(t *testing.T) {
	h := handler(site(t))
	for _, accept := range [][]string{nil, {"Accept: application/json"}, {"Accept: */*"}} {
		if w := get(h, "GET", "/catalog", accept...); w.Code != 404 || w.Body.String() != "Not found" {
			t.Errorf("%v: %d %.60s", accept, w.Code, w.Body)
		}
	}
	if w := get(h, "GET", "/missing.js", html); w.Code != 404 || w.Body.String() != "Not found" {
		t.Errorf("/missing.js: %d %.60s", w.Code, w.Body)
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if w := get(h, m, "/catalog", html); w.Code != 404 {
			t.Errorf("%s /catalog: %d", m, w.Code)
		}
	}
}

// compat: "/api, /mcp and /healthz are never the shell".
func TestReservedPathsAreNeverTheShell(t *testing.T) {
	h := handler(site(t))
	api := get(h, "GET", "/api/unknown", html)
	if api.Code != 404 || errorOf(t, api) != "not_found|not found" || !strings.HasPrefix(api.Header().Get("Content-Type"), "application/json") {
		t.Errorf("/api/unknown: %d %s", api.Code, api.Body)
	}
	for _, p := range []string{"/api", "/mcp", "/mcp/sub", "/healthz", "/healthz/x", "/api/x"} {
		if w := get(h, "GET", p, html); strings.Contains(w.Body.String(), `<div id="root">`) {
			t.Errorf("%s served the shell", p)
		}
	}
	if w := get(h, "GET", "/mcp/sub", html); w.Code != 404 || w.Body.String() != "Not found" {
		t.Errorf("/mcp/sub: %d %.60s", w.Code, w.Body)
	}
	// Reserved paths are also not read from disk, even when a file of that name exists.
	root := site(t)
	if err := os.MkdirAll(filepath.Join(root, "api"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "api", "x.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := get(handler(root), "GET", "/api/x.js"); w.Code != 404 {
		t.Errorf("/api/x.js: %d", w.Code)
	}
}

func TestNothingOutsideTheWebRoot(t *testing.T) {
	root := site(t)
	h := handler(root)
	// A symlink inside the root that points out of it.
	if err := os.Symlink(filepath.Join(filepath.Dir(root), "secret.txt"), filepath.Join(root, "assets", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(root), filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/..%2f..%2fetc/passwd",
		"/..%2fsecret.txt",
		"/%2e%2e/secret.txt",
		"/%2e%2e%2fsecret.txt",
		"/../secret.txt",
		"/assets/../index.html",
		"/assets/%2e%2e/secret.txt",
		"/assets/..%2f..%2fsecret.txt",
		"/assets/%2e%2e%2f%2e%2e%2fsecret.txt",
		"/assets/../../secret.txt",
		"/..%5csecret.txt",
		"/%252e%252e/secret.txt",
		"//secret.txt",
		"/assets/link.txt",
		"/up/secret.txt",
		"/assets/%00",
	} {
		for _, accept := range []string{"", html} {
			var hdr []string
			if accept != "" {
				hdr = []string{accept}
			}
			w := get(h, "GET", target, hdr...)
			if strings.Contains(w.Body.String(), "TOP-SECRET") || strings.Contains(w.Body.String(), "root:") {
				t.Errorf("%s served a file outside the web root", target)
			}
			if target == "/assets/../index.html" && w.Code == 200 && w.Header().Get("Cache-Control") == "public, max-age=31536000, immutable" {
				t.Errorf("%s got the asset cache", target)
			}
		}
	}
	// Static alone: these are misses that reach next.
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	for _, target := range []string{"/..%2fsecret.txt", "/assets/../index.html", "/assets/%2e%2e/secret.txt", "/assets/link.txt"} {
		reached = false
		get(httpx.Static(root, next), "GET", target)
		if !reached {
			t.Errorf("%s was not passed on", target)
		}
	}
}

func TestNoDirectoryListing(t *testing.T) {
	root := site(t)
	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(404) })
	h := httpx.Static(root, next)
	for _, p := range []string{"/assets", "/assets/", "/assets/inner", "/assets/inner/"} {
		w := get(h, "GET", p)
		if w.Code != 404 || strings.Contains(w.Body.String(), "app-abc.js") {
			t.Errorf("%s: %d %.80s", p, w.Code, w.Body)
		}
	}
	if reached != 4 {
		t.Errorf("%d of 4 directory requests passed on", reached)
	}
	// A root without index.html: "/" is a miss, not a listing.
	empty := t.TempDir()
	reached = 0
	if w := get(httpx.Static(empty, next), "GET", "/"); reached != 1 || strings.Contains(w.Body.String(), "<pre>") {
		t.Errorf("/ of an empty root: %d %.80s", reached, w.Body)
	}
}

func TestMissingWebRootPassesEverythingOn(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "no-such-dir")
	h := handler(gone)
	for _, p := range []string{"/", "/index.html", "/catalog", "/assets/x.js"} {
		if w := get(h, "GET", p, html); w.Code != 404 || w.Body.String() != "Not found" {
			t.Errorf("%s: %d %.60s", p, w.Code, w.Body)
		}
	}
	reached := 0
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ })
	get(httpx.Static(gone, next), "GET", "/")
	get(httpx.SPAFallback(gone, next), "GET", "/catalog", html)
	if reached != 2 {
		t.Errorf("passed on %d of 2", reached)
	}
}

func TestRangeAndConditionalRequests(t *testing.T) {
	h := handler(site(t))
	w := get(h, "GET", "/assets/app-abc.js", "Range: bytes=0-6")
	if w.Code != 206 || w.Body.String() != "console" {
		t.Errorf("range: %d %q", w.Code, w.Body)
	}
	first := get(h, "GET", "/assets/app-abc.js")
	lm := first.Header().Get("Last-Modified")
	if lm == "" {
		t.Fatal("no Last-Modified")
	}
	if w := get(h, "GET", "/assets/app-abc.js", "If-Modified-Since: "+lm); w.Code != 304 {
		t.Errorf("conditional: %d", w.Code)
	}
	if w := get(h, "HEAD", "/assets/app-abc.js"); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Cache-Control") == "" {
		t.Errorf("HEAD asset: %d %d", w.Code, w.Body.Len())
	}
}

func TestShellMissingFromRootPassesOn(t *testing.T) {
	root := t.TempDir() // exists, has no index.html
	if w := get(handler(root), "GET", "/catalog", html); w.Code != 404 {
		t.Errorf("%d", w.Code)
	}
}

// Item 7: dotfiles, the cache decision on the cleaned path, no content sniffing.
func TestDotSegmentsAreMisses(t *testing.T) {
	root := site(t)
	for _, f := range []string{".env", ".git/config", "assets/.hidden", ".well-known/x"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("DOTFILE"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := handler(root)
	for _, target := range []string{"/.env", "/.git/config", "/assets/.hidden", "/.well-known/x", "/.git/"} {
		for _, accept := range [][]string{nil, {html}} {
			w := get(h, "GET", target, accept...)
			if w.Code == 200 || strings.Contains(w.Body.String(), "DOTFILE") || strings.Contains(w.Body.String(), `<div id="root">`) {
				t.Errorf("%s %v: %d %.40s", target, accept, w.Code, w.Body)
			}
		}
	}
}

func TestCacheDecisionUsesTheCleanedPath(t *testing.T) {
	h := handler(site(t))
	for target, want := range map[string]string{
		"//index.html":         "no-cache",
		"/./index.html":        "no-cache",
		"/.":                   "no-cache",
		"//":                   "no-cache",
		"/assets/./app-abc.js": "public, max-age=31536000, immutable",
		"//assets/app-abc.js":  "public, max-age=31536000, immutable",
	} {
		w := get(h, "GET", target)
		if w.Code != 200 || w.Header().Get("Cache-Control") != want {
			t.Errorf("%s: %d Cache-Control %q, want %q", target, w.Code, w.Header().Get("Cache-Control"), want)
		}
	}
	if w := get(h, "GET", "//api/x.js"); w.Code != 404 {
		t.Errorf("//api/x.js: %d", w.Code)
	}
}

func TestUnknownExtensionsAreOctetStreamNeverSniffed(t *testing.T) {
	root := site(t)
	for name, body := range map[string]string{"assets/blob.zzqq": "<html><script>alert(1)</script>", "assets/NOEXT": "<html>"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := handler(root)
	for _, p := range []string{"/assets/blob.zzqq", "/assets/NOEXT"} {
		w := get(h, "GET", p)
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/octet-stream" {
			t.Errorf("%s: %d %q", p, w.Code, w.Header().Get("Content-Type"))
		}
	}
}
