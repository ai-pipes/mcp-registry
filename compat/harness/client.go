//go:build compat

package harness

import (
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
}

// validateSchema is replaced by the real JSON Schema validator in a later task.
var validateSchema = func(_ string, _ []byte) error { return nil }

const fakesHost = "host.docker.internal"

// newHTTPClient builds a client that maps host.docker.internal to the loopback interface, so the
// URLs the container uses to reach test fakes also work from the test process.
func newHTTPClient(jar http.CookieJar) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Jar:     jar,
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if host, port, err := net.SplitHostPort(addr); err == nil && host == fakesHost {
					addr = net.JoinHostPort("127.0.0.1", port)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
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

// Do sends one request. body is nil, a string or []byte (sent as is), or any value (sent as JSON).
// hdr entries are "Name: Value", applied after the default Origin (= BaseURL). "Host: x" sets the
// request's Host (net/http ignores a Host header). An entry with an empty value, such as
// "Origin:", removes that header, so a request without Origin can be sent.
func (c *Client) Do(t *testing.T, method, path string, body any, hdr ...string) Response {
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
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data}
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
