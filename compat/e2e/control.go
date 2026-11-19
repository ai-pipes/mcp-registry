//go:build compat && e2e

// Package e2e runs the browser flows (e2e/flows, Playwright) against the application image under
// test. The Go side boots the stacks and the fakes, and answers a small control API the specs call
// for the few things a browser cannot do: run a fixed operator command, queue the next IdP login,
// register a server on a fake upstream, move a session's expiry.
//
// The control API is a test tool, not a product surface, and is kept narrow on purpose: it listens
// on 127.0.0.1 only, every request must carry the random bearer token of this run, and it knows
// FIXED operations with typed parameters. There is no way to send SQL or an arbitrary CLI
// argument list through it.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// controlAdminSub is the person the control API registers servers as. It is a normal admin of the
// stack; the specs' own people are created next to it.
const controlAdminSub = "e2e-control"

// StackInfo is what the specs learn about a stack.
type StackInfo struct {
	BaseURL string `json:"baseURL"`
	OIDC    bool   `json:"oidc"`
	// ProviderURL is the authorize URL of the stack's OAuth provider ("" when it has none); a
	// server of kind oauth_user registered through POST /servers uses it.
	ProviderURL string `json:"providerURL"`
}

// Stack is a booted stack and what belongs to it.
type Stack struct {
	Name     string
	Harness  *harness.Stack
	OIDC     bool
	Provider *fakes.OAuthProvider // nil = oauth_user servers cannot be registered here

	adminOnce sync.Once
	admin     *http.Client
	adminErr  error
}

// Control is the control API. Build it with NewControl, Listen, and keep Serve going: a handler
// that must create a fake (they need *testing.T) hands the work to Serve instead of using t from
// its own request goroutine.
type Control struct {
	token  string
	stacks map[string]*Stack
	idp    *fakes.FakeIdP
	jobs   chan job

	mu        sync.Mutex
	upstreams map[string]*fakes.Upstream // by slug
}

type job struct {
	run  func(t *testing.T)
	done chan struct{}
}

// NewControl returns a control API over the stacks (by name) and the IdP the OIDC stack uses (nil
// when none).
func NewControl(stacks []*Stack, idp *fakes.FakeIdP) *Control {
	c := &Control{
		token:     randomToken(),
		stacks:    map[string]*Stack{},
		idp:       idp,
		jobs:      make(chan job),
		upstreams: map[string]*fakes.Upstream{},
	}
	for _, s := range stacks {
		c.stacks[s.Name] = s
	}
	return c
}

// Token is the bearer token every request must carry.
func (c *Control) Token() string { return c.token }

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system random source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

// Serve answers the jobs of handlers that create fakes until ctx ends; t is the test the fakes
// belong to. In TestBrowserFlows the test goroutine itself is blocked on the browser run, so it
// serves; a test that makes the calls itself runs Serve on a goroutine of its own.
func (c *Control) Serve(ctx context.Context, t *testing.T) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-c.jobs:
			func() {
				defer close(j.done) // also when a fake's t.Fatalf ends this goroutine: the caller must not hang
				j.run(t)
			}()
		}
	}
}

// onServe runs f on the goroutine that runs Serve and waits for it.
func (c *Control) onServe(ctx context.Context, f func(t *testing.T)) error {
	j := job{run: f, done: make(chan struct{})}
	select {
	case c.jobs <- j:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handler is the control API.
func (c *Control) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stacks", c.getStacks)
	mux.HandleFunc("POST /cli/create-user", c.createUser)
	mux.HandleFunc("POST /cli/login-link", c.loginLink)
	mux.HandleFunc("POST /stack/first-start-link", c.firstStartLink)
	mux.HandleFunc("POST /idp/next-login", c.nextLogin)
	mux.HandleFunc("POST /servers", c.registerServer)
	mux.HandleFunc("GET /upstream/calls", c.upstreamCalls)
	mux.HandleFunc("POST /sessions/expire", c.expireSession)
	return c.authenticated(mux)
}

// authenticated refuses every request that lacks the run's token, before any routing detail is
// revealed (an unknown path without the token is a 401 too).
func (c *Control) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(c.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or wrong control token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Listen binds the control API to 127.0.0.1 on a free port and serves it until ctx ends. It
// returns the base URL.
func (c *Control) Listen(ctx context.Context) (string, error) {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = srv.Serve(l) }() // ErrServerClosed at the end
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return "http://" + l.Addr().String(), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decode reads a JSON body into v and refuses unknown fields and trailing data.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "bad body: trailing data")
		return false
	}
	return true
}

func (c *Control) stack(w http.ResponseWriter, name string) *Stack {
	s, ok := c.stacks[name]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown stack %q", name))
		return nil
	}
	return s
}

// subjectPattern limits what a person's sub and name may be on the command line: it is passed to
// the CLI as one argument, and must never be taken for an option or hold a shell-looking payload.
var subjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@:-]{0,99}$`)

func validPerson(w http.ResponseWriter, what, v string) bool {
	if !subjectPattern.MatchString(v) {
		writeError(w, http.StatusBadRequest, what+" must match "+subjectPattern.String())
		return false
	}
	return true
}

// --- GET /stacks -----------------------------------------------------------------------------

func (c *Control) getStacks(w http.ResponseWriter, _ *http.Request) {
	out := map[string]StackInfo{}
	for name, s := range c.stacks {
		info := StackInfo{BaseURL: s.Harness.BaseURL, OIDC: s.OIDC}
		if s.Provider != nil {
			info.ProviderURL = s.Provider.AuthorizeURL
		}
		out[name] = info
	}
	writeJSON(w, http.StatusOK, map[string]any{"stacks": out})
}

// --- operator CLI: the two fixed commands ----------------------------------------------------

type createUserBody struct {
	Stack string `json:"stack"`
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
}

func (c *Control) createUser(w http.ResponseWriter, r *http.Request) {
	var b createUserBody
	if !decode(w, r, &b) || !validPerson(w, "sub", b.Sub) || !validPerson(w, "name", b.Name) {
		return
	}
	s := c.stack(w, b.Stack)
	if s == nil {
		return
	}
	if err := createUser(s, b.Sub, b.Name, b.Admin); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func createUser(s *Stack, sub, name string, admin bool) error {
	args := []string{"create-user", "--sub", sub, "--name", name}
	if admin {
		args = append(args, "--admin")
	}
	res, err := s.Harness.TryCLI("", args...)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("create-user exited %d: %s", res.Code, strings.TrimSpace(res.Stderr))
	}
	return nil
}

type loginLinkBody struct {
	Stack string `json:"stack"`
	Sub   string `json:"sub"`
}

func (c *Control) loginLink(w http.ResponseWriter, r *http.Request) {
	var b loginLinkBody
	if !decode(w, r, &b) || !validPerson(w, "sub", b.Sub) {
		return
	}
	s := c.stack(w, b.Stack)
	if s == nil {
		return
	}
	link, err := loginLink(s, b.Sub)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"link": link})
}

func loginLink(s *Stack, sub string) (string, error) {
	res, err := s.Harness.TryCLI("", "login-link", sub)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("login-link exited %d: %s", res.Code, strings.TrimSpace(res.Stderr))
	}
	link := strings.TrimSpace(res.Stdout)
	if !strings.Contains(link, "#link=") {
		return "", errors.New("login-link printed no #link= URL")
	}
	return link, nil
}

// --- first start link ------------------------------------------------------------------------

type stackBody struct {
	Stack string `json:"stack"`
}

func (c *Control) firstStartLink(w http.ResponseWriter, r *http.Request) {
	var b stackBody
	if !decode(w, r, &b) {
		return
	}
	s := c.stack(w, b.Stack)
	if s == nil {
		return
	}
	logs, err := s.Harness.TryLogs()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	link, ok := linkInLogs(logs)
	if !ok {
		writeError(w, http.StatusNotFound, "no record with a one-time link in the app log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"link": link})
}

// linkInLogs finds the last JSON log record whose url field holds a sign-in link. The message
// text of the record is not read: only the field the product documents.
func linkInLogs(logs string) (string, bool) {
	link := ""
	for _, line := range strings.Split(logs, "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if strings.Contains(rec.URL, "#link=") {
			link = rec.URL
		}
	}
	return link, link != ""
}

// --- IdP -------------------------------------------------------------------------------------

type nextLoginBody struct {
	Sub    string   `json:"sub"`
	Name   string   `json:"name"`
	Email  string   `json:"email"`
	Groups []string `json:"groups"`
}

func (c *Control) nextLogin(w http.ResponseWriter, r *http.Request) {
	var b nextLoginBody
	if !decode(w, r, &b) {
		return
	}
	if c.idp == nil {
		writeError(w, http.StatusNotFound, "this run has no identity provider")
		return
	}
	if b.Sub == "" || b.Name == "" {
		writeError(w, http.StatusBadRequest, "sub and name are required")
		return
	}
	c.idp.NextLogin(fakes.Claims{Sub: b.Sub, Name: b.Name, Email: b.Email, Groups: b.Groups})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- servers on fake upstreams ---------------------------------------------------------------

type registerBody struct {
	Stack string   `json:"stack"`
	Kind  string   `json:"kind"`
	Slug  string   `json:"slug"`
	Tools []string `json:"tools"`
}

// RegisteredServer is the answer of POST /servers.
type RegisteredServer struct {
	Slug  string   `json:"slug"`
	Name  string   `json:"name"`
	URL   string   `json:"url"`
	Tools []string `json:"tools"`
}

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// sharedSecret is what a shared_secret server registered here is given; the fake upstream demands
// it, so the registration's own sync proves the gateway sends it.
const sharedSecret = "e2e-shared-secret" //nolint:gosec // G101: a made-up secret of a test double

func (c *Control) registerServer(w http.ResponseWriter, r *http.Request) {
	var b registerBody
	if !decode(w, r, &b) {
		return
	}
	s := c.stack(w, b.Stack)
	if s == nil {
		return
	}
	switch b.Kind {
	case "none", "shared_secret", "pat_user":
	case "oauth_user":
		if s.Provider == nil {
			writeError(w, http.StatusBadRequest, "this stack has no OAuth provider")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown kind %q", b.Kind))
		return
	}
	if b.Slug == "" {
		b.Slug = "e2e-" + randomToken()[:10]
	}
	if !slugPattern.MatchString(b.Slug) {
		writeError(w, http.StatusBadRequest, "slug must match "+slugPattern.String())
		return
	}
	if len(b.Tools) == 0 {
		b.Tools = []string{"echo", "ping"}
	}
	if len(b.Tools) > 20 {
		writeError(w, http.StatusBadRequest, "at most 20 tools")
		return
	}
	for _, name := range b.Tools {
		if !toolNamePattern.MatchString(name) {
			writeError(w, http.StatusBadRequest, "tool names must match "+toolNamePattern.String())
			return
		}
	}

	var up *fakes.Upstream
	opts := fakes.UpstreamOptions{Tools: fakes.Tools(b.Tools...)}
	if b.Kind == "shared_secret" {
		opts.RequireHeader = [2]string{"Authorization", "Bearer " + sharedSecret}
	}
	if err := c.onServe(r.Context(), func(t *testing.T) { up = fakes.StartUpstream(t, opts) }); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if up == nil {
		writeError(w, http.StatusInternalServerError, "the fake upstream could not be started")
		return
	}
	var sopts []fakes.ServerOpt
	switch b.Kind {
	case "shared_secret":
		sopts = append(sopts, fakes.WithSecret(sharedSecret))
	case "oauth_user":
		sopts = append(sopts, fakes.WithOAuth(s.Provider, "read"))
	}
	body := fakes.ServerInput(up, b.Slug, b.Kind, sopts...)
	status, resp, err := s.adminDo(http.MethodPost, "/api/servers", body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if status != http.StatusCreated {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("POST /api/servers: %d %s", status, resp))
		return
	}
	c.mu.Lock()
	c.upstreams[b.Slug] = up
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, RegisteredServer{Slug: b.Slug, Name: fakes.DisplayName(b.Slug), URL: up.URL, Tools: b.Tools})
}

// adminDo sends a request to the stack as the control admin (a session made on first use).
func (s *Stack) adminDo(method, path string, body any) (int, []byte, error) {
	s.adminOnce.Do(func() { s.admin, s.adminErr = s.signInControlAdmin() })
	if s.adminErr != nil {
		return 0, nil, s.adminErr
	}
	return s.doJSON(s.admin, method, path, body)
}

func (s *Stack) doJSON(client *http.Client, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		enc, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(enc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.Harness.LoopbackURL()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	// The app compares Origin with its PUBLIC_URL, which is the stack's BaseURL.
	req.Header.Set("Origin", s.Harness.BaseURL)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

func (s *Stack) signInControlAdmin() (*http.Client, error) {
	if err := createUser(s, controlAdminSub, "E2E Control", true); err != nil {
		return nil, err
	}
	link, err := loginLink(s, controlAdminSub)
	if err != nil {
		return nil, err
	}
	_, code, _ := strings.Cut(link, "#link=")
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Jar: jar, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	status, resp, err := s.doJSON(client, http.MethodPost, "/api/auth/link", map[string]string{"code": code})
	if err != nil {
		return nil, err
	}
	if status != http.StatusNoContent {
		return nil, fmt.Errorf("control admin sign-in: %d %s", status, resp)
	}
	return client, nil
}

// --- upstream calls --------------------------------------------------------------------------

// UpstreamCall is a tools/call a fake upstream received. Header values are not returned (they
// hold credentials): only the names.
type UpstreamCall struct {
	Tool        string          `json:"tool"`
	Arguments   json.RawMessage `json:"arguments"`
	HeaderNames []string        `json:"headerNames"`
}

func (c *Control) upstreamCalls(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("slug")
	c.mu.Lock()
	up, ok := c.upstreams[slug]
	c.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no upstream registered for %q", slug))
		return
	}
	calls := []UpstreamCall{}
	for _, call := range up.Calls() {
		names := make([]string, 0, len(call.Header))
		for n := range call.Header {
			names = append(names, strings.ToLower(n))
		}
		sort.Strings(names)
		args := call.Arguments
		if len(args) == 0 {
			args = json.RawMessage("null")
		}
		calls = append(calls, UpstreamCall{Tool: call.Tool, Arguments: args, HeaderNames: names})
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls, "listRequests": up.ListRequests()})
}

// --- sessions --------------------------------------------------------------------------------

type expireBody struct {
	Stack string `json:"stack"`
	Sub   string `json:"sub"`
}

func (c *Control) expireSession(w http.ResponseWriter, r *http.Request) {
	var b expireBody
	if !decode(w, r, &b) || !validPerson(w, "sub", b.Sub) {
		return
	}
	s := c.stack(w, b.Stack)
	if s == nil {
		return
	}
	// The one fixed SQL statement of the control API: the person's sessions look unused for
	// longer than the idle limit, which is how a session expires without waiting.
	tag, err := s.Harness.DB.Exec(r.Context(),
		`update identity.sessions set last_seen_at = now() - interval '13 hours' -- past the 12 h idle limit
		  where user_id = (select id from identity.users where oidc_sub = $1)`, b.Sub)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "that person has no session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": tag.RowsAffected()})
}
