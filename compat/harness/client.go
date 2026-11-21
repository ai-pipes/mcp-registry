//go:build compat

package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

// Client is an HTTP client with its own cookie jar; every request carries Origin = BaseURL.
// Redirects are not followed, so a test sees the 302 and its Location.
type Client struct {
	base string
	http *http.Client
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Sent holds the headers of the request as it went out; only DoExpectingEarlyRefusal fills it
	// (it sends the request itself, so the cookies of the client's jar are visible there).
	Sent http.Header
}

// validateSchema is wired to ValidateSchema in schema.go.
var validateSchema func(schema string, body []byte) error

const fakesHost = "host.docker.internal"

// loopbackHost reports whether the test process resolves host to the loopback address itself:
// the name the container uses for the test process, and any *.compat.test name (Options.PublicHost,
// a fake's advertised host).
func loopbackHost(host string) bool {
	return host == fakesHost || strings.HasSuffix(host, ".compat.test")
}

// idleConnTimeout is how long the harness keeps an idle connection for reuse. A server closes an
// idle keep-alive connection after its own timeout (Node's default is 5 s); a request that goes out
// on a connection the server is closing at that very moment dies with EOF, and Go never retries a
// non-idempotent request (a POST) on such a connection. One second is far below any server's
// keep-alive, so a connection reused after a pause is always a fresh one. See
// TestIdleConnectionsOutliveNoServerKeepAlive.
const idleConnTimeout = time.Second

// newTransport builds the transport of the harness's clients; idle is the idle-connection timeout.
func newTransport(idle time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		IdleConnTimeout: idle,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && loopbackHost(host) {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
}

// NewHTTPClient is newHTTPClient for the control API of compat/e2e, which also talks to the app
// and must not hold an idle connection past idleConnTimeout either.
func NewHTTPClient(jar http.CookieJar) *http.Client { return newHTTPClient(jar) }

// newHTTPClient builds a client that maps host.docker.internal to the loopback interface, so the
// URLs the container uses to reach test fakes also work from the test process. Idle connections
// are dropped after idleConnTimeout (the MCP sessions use this client too).
func newHTTPClient(jar http.CookieJar) *http.Client {
	return &http.Client{
		Jar:           jar,
		Timeout:       60 * time.Second,
		Transport:     newTransport(idleConnTimeout),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Anonymous returns a client without a session.
func (s *Stack) Anonymous() *Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err) // cookiejar.New with nil options never fails
	}
	return &Client{base: s.BaseURL, http: newHTTPClient(jar)}
}

// Admin creates an admin (create-user --admin), asks for a sign-in link and uses it.
func (s *Stack) Admin(t *testing.T) *Client {
	t.Helper()
	return s.signIn(t, Unique("admin"), "Admin", "--admin")
}

// Member creates a member with the given sub and signs in with a link.
func (s *Stack) Member(t *testing.T, sub string) *Client {
	t.Helper()
	return s.signIn(t, sub, "Member")
}

func (s *Stack) signIn(t *testing.T, sub, name string, extra ...string) *Client {
	t.Helper()
	create := append([]string{"create-user", "--sub", sub, "--name", name}, extra...)
	if r := s.CLI(t, "", create...); r.Code != 0 {
		t.Fatalf("create-user %s: code %d stderr %q", sub, r.Code, r.Stderr)
	}
	r := s.CLI(t, "", "login-link", sub)
	if r.Code != 0 {
		t.Fatalf("login-link %s: code %d stderr %q", sub, r.Code, r.Stderr)
	}
	_, code, ok := strings.Cut(strings.TrimSpace(r.Stdout), "#link=")
	if !ok || code == "" {
		t.Fatalf("login-link %s: no #link= in output", sub)
	}
	c := s.Anonymous()
	resp := c.Do(t, "POST", "/api/auth/link", map[string]string{"code": code})
	if resp.Status != http.StatusNoContent {
		t.Fatalf("POST /api/auth/link: status %d body %q", resp.Status, resp.Body)
	}
	return c
}

// newRequest builds the request of Do and DoExpectingEarlyRefusal (see Do for the arguments).
func (c *Client) newRequest(t *testing.T, method, path string, body any, hdr []string) *http.Request {
	t.Helper()
	var rd io.Reader
	asJSON := false
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		rd, asJSON = bytes.NewReader(enc), true
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", c.base)
	if asJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range hdr {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			t.Fatalf("header %q is not \"Name: Value\"", h)
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch {
		case strings.EqualFold(name, "Host"):
			req.Host = value
		case value == "":
			req.Header.Del(name)
		default:
			req.Header.Set(name, value)
		}
	}
	return req
}

// Do sends one request. body is nil, a string or []byte (sent as is), or any value (sent as JSON).
// hdr entries are "Name: Value", applied after the default Origin (= BaseURL). "Host: x" sets the
// request's Host (net/http ignores a Host header). An entry with an empty value, such as
// "Origin:", removes that header, so a request without Origin can be sent.
// There is no retry: a transport error fails the test.
func (c *Client) Do(t *testing.T, method, path string, body any, hdr ...string) Response {
	t.Helper()
	req := c.newRequest(t, method, path, body, hdr)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data, Sent: req.Header.Clone()}
}

// Answers fails the test unless the app answers a request that touches neither the database nor
// the embedding model: GET /, status 200 (the body is not judged). The UI shell is served by the
// platform layer of any server from its first milestone, whereas /api/auth/methods belongs to a
// module (identity) that a server may not have yet; the probe only has to show that the
// application answers. It is the liveness probe for right after a start or a restart. /healthz is not: it runs a database query against a 1.5 s timer, and the
// reference image blocks its event loop while it loads its embedding model right after it first
// answers healthy, so under load /healthz can answer 503 while the database is fine (decision
// ready-before-serving, see deviations.go). Start, SwapImage and the boot helpers have already
// proven health once.
func (s *Stack) Answers(t *testing.T) {
	t.Helper()
	// Accept: text/html, as a browser's first request: a server serves the shell to it, and the
	// probe then does not depend on what a server answers a request that wants anything else.
	if r := s.Anonymous().Do(t, "GET", "/", nil, "Accept: text/html"); r.Status != http.StatusOK {
		t.Fatalf("GET /: %d %.200q", r.Status, r.Body)
	}
}

// Token creates an API token for the signed-in person and returns the sk_ value.
func (c *Client) Token(t *testing.T, name string) string {
	t.Helper()
	r := c.Do(t, "POST", "/api/tokens", map[string]string{"name": name})
	if r.Status != http.StatusCreated && r.Status != http.StatusOK {
		t.Fatalf("POST /api/tokens: status %d body %q", r.Status, r.Body)
	}
	var out struct {
		Token string `json:"token"`
	}
	r.Decode(t, &out)
	if !strings.HasPrefix(out.Token, "sk_") {
		t.Fatalf("POST /api/tokens: unexpected token %q", Redact(out.Token))
	}
	return out.Token
}

// JSON validates the body against compat/schemas/<schema>.json and decodes it into into. The
// schema name must not be empty; use Decode for the rare unvalidated body.
func (r Response) JSON(t *testing.T, schema string, into any) {
	t.Helper()
	if schema == "" {
		t.Fatal("Response.JSON needs a schema name; use Decode for an unvalidated body")
	}
	if err := validateSchema(schema, r.Body); err != nil {
		t.Fatalf("response does not match schema %s: %v", schema, err)
	}
	r.Decode(t, into)
}

// Decode unmarshals the body into into without schema validation.
func (r Response) Decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, into); err != nil {
		t.Fatalf("decode response (status %d): %v", r.Status, err)
	}
}

// DoExpectingEarlyRefusal sends a request whose body the server is expected to refuse without
// reading it (a body over a limit: 413) and returns that answer. A server that refuses early
// answers and closes while the client is still writing, so the kernel may reset the connection
// under the writer, and a regular client can lose the answer to a "broken pipe" or "connection
// reset". This helper writes the request from a goroutine on a raw connection, reads the response
// at the same time and returns it whatever happened to the write; if no response arrives at all it
// fails. It is for the oversize-refusal scenarios only: a request that is expected to succeed goes
// through Do, which has no retry and no tolerance for transport errors.
func (c *Client) DoExpectingEarlyRefusal(t *testing.T, method, path string, body []byte, hdr ...string) Response {
	t.Helper()
	return c.doExpectingEarlyRefusal(t, method, path, body, false, hdr)
}

// DoChunkedExpectingEarlyRefusal is DoExpectingEarlyRefusal for a body sent without a declared
// length (Transfer-Encoding: chunked, no Content-Length): the server cannot refuse it by length
// before reading, only after it has read up to its limit.
func (c *Client) DoChunkedExpectingEarlyRefusal(t *testing.T, method, path string, body []byte, hdr ...string) Response {
	t.Helper()
	return c.doExpectingEarlyRefusal(t, method, path, body, true, hdr)
}

func (c *Client) doExpectingEarlyRefusal(t *testing.T, method, path string, body []byte, chunked bool, hdr []string) Response {
	t.Helper()
	req := c.newRequest(t, method, path, body, hdr)
	if chunked {
		// A body of unknown length: net/http writes it as chunks and sends no Content-Length.
		req.Body, req.GetBody, req.ContentLength = io.NopCloser(bytes.NewReader(body)), nil, -1
	}
	req.Header.Set("Connection", "close")
	// Only http.Client.Do adds the cookies of its jar; this request is sent raw, so add them here.
	if jar := c.http.Jar; jar != nil {
		for _, ck := range jar.Cookies(req.URL) {
			req.AddCookie(ck)
		}
	}
	addr := req.URL.Host
	if !strings.Contains(addr, ":") {
		addr += ":80"
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("%s %s: dial: %v", method, path, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	go func() { _ = req.Write(conn) }() // a write error is expected when the server refuses early
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("%s %s: no response to a request that was expected to be refused early: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil && len(data) == 0 {
		t.Fatalf("%s %s: read the refusal: %v", method, path, err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data, Sent: req.Header.Clone()}
}
