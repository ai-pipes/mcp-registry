package httpx

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

const (
	immutableCache = "public, max-age=31536000, immutable"
	shellCache     = "no-cache"
)

// reserved are the prefixes that are never the web UI: the API, the MCP endpoint, the probe.
var reserved = []string{"/api", "/mcp", "/healthz"}

func isReserved(p string) bool {
	for _, r := range reserved {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// contentTypes covers what the built UI contains. The standard table reads /etc/mime.types,
// which a minimal container does not have, so these are not left to it.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".svg":         "image/svg+xml",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".ttf":         "font/ttf",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

func contentTypeOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	return mime.TypeByExtension(ext) // "" lets ServeContent sniff
}

// readable opens name (slash-separated, relative to webRoot) for reading. The open goes through
// os.Root: a path that leaves webRoot, by ".." or by a symlink, fails like a missing file, whatever
// the encoding of the request. ok is false for a missing webRoot, a missing file and a directory.
func readable(webRoot, name string) (f *os.File, info os.FileInfo, ok bool) {
	root, err := os.OpenRoot(webRoot)
	if err != nil {
		return nil, nil, false
	}
	defer func() { _ = root.Close() }()
	f, err = root.Open(name)
	if err != nil {
		return nil, nil, false
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, false
	}
	return f, info, true
}

func hasDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func isRead(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// Static serves files of webRoot (the built UI) for GET and HEAD: hashed /assets/* are cached for
// a year, the shell (/ and /index.html) is no-cache, a miss carries no cache header and goes to
// next. Directories are never listed (a directory other than / is a miss). Without webRoot every
// request goes to next. Content types come from the extension; ranges and conditional requests
// are honoured.
func Static(webRoot string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !isRead(r) || isReserved(p) || !strings.HasPrefix(p, "/") || hasDotDot(p) || strings.ContainsRune(p, 0) {
			next.ServeHTTP(w, r)
			return
		}
		name := path.Clean(p)[1:]
		if name == "" {
			name = "index.html"
		}
		f, info, ok := readable(webRoot, name)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		h := w.Header()
		switch {
		case strings.HasPrefix(p, "/assets/"):
			h.Set("Cache-Control", immutableCache)
		case p == "/" || p == "/index.html":
			h.Set("Cache-Control", shellCache)
		}
		if ct := contentTypeOf(name); ct != "" {
			h.Set("Content-Type", ct)
		}
		http.ServeContent(w, r, name, info.ModTime(), f)
	})
}

// SPAFallback answers browser navigations to client-side routes with index.html: GET or HEAD with
// an Accept containing text/html, a path whose last segment has no dot, outside /api, /mcp and
// /healthz. Everything else, and a webRoot without index.html, goes to next. The shell is never
// cached, so an upgrade shows at once.
func SPAFallback(webRoot string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		last := p[strings.LastIndex(p, "/")+1:]
		if !isRead(r) || isReserved(p) || strings.Contains(last, ".") ||
			!strings.Contains(r.Header.Get("Accept"), "text/html") {
			next.ServeHTTP(w, r)
			return
		}
		f, info, ok := readable(webRoot, "index.html")
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", shellCache)
		h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, f)
		}
	})
}
