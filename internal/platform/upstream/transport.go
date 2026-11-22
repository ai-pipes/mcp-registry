package upstream

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync/atomic"
)

// operation is the state of one upstream operation: its deadline, what the HTTP layer saw and the
// byte budget. Nothing in it outlives the operation.
type operation struct {
	ctx     context.Context // the operation's deadline; cancelled when the byte limit is passed
	cancel  context.CancelFunc
	headers map[string]string
	limit   int64

	bytes        atomic.Int64
	tooLarge     atomic.Bool
	unauthorized atomic.Bool
	protocol     atomic.Pointer[string]
}

var (
	errTooLarge = errors.New("response exceeds the size limit")
	errRedirect = errors.New("redirect refused")
	errNoDelete = errors.New("sessions are not ended")
)

// replyKey marks the context of a request that answers a request of the server.
type replyKey struct{}

// guard is the RoundTripper of the operation's http.Client. It is the only place where the
// target's headers are added, so every request of the session carries them and nothing else of
// the caller's. It refuses redirects, notes a 401, counts the bytes of every response and stops
// the operation as soon as they pass the limit.
type guard struct {
	op    *operation
	inner http.RoundTripper
}

// protocolHeaders are the headers only the protocol sets: a target's value for them is ignored (the
// TypeScript SDK overwrites Accept, Content-Type, Mcp-Session-Id and Mcp-Protocol-Version the same
// way; Content-Length and Host belong to the HTTP layer, which fetch refuses to let a caller set).
var protocolHeaders = map[string]bool{
	"Accept":               true,
	"Content-Type":         true,
	"Content-Length":       true,
	"Host":                 true,
	"Transfer-Encoding":    true,
	"Mcp-Protocol-Version": true,
	"Mcp-Session-Id":       true,
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	op := g.op
	if req.Method == http.MethodDelete {
		// The TypeScript client never ends a session, and an upstream that does not answer would
		// hold every operation: nothing is sent.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errNoDelete
	}
	r := req.Clone(req.Context())
	keys := make([]string, 0, len(op.headers))
	for k := range op.headers {
		keys = append(keys, k)
	}
	sort.Strings(keys) // names that differ only in case: the same winner every time
	for _, k := range keys {
		if !protocolHeaders[http.CanonicalHeaderKey(k)] {
			r.Header.Set(k, op.headers[k])
		}
	}
	if pv := op.protocol.Load(); pv != nil {
		r.Header.Set("Mcp-Protocol-Version", *pv)
	}

	resp, err := g.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		op.unauthorized.Store(true)
	}
	if r.Context().Value(replyKey{}) != nil {
		// An answer to a request of the server is best effort: whatever comes back is read by
		// nobody and cannot fail the operation (the SDK would break the connection on a refusal).
		_ = resp.Body.Close()
		return &http.Response{
			Status: "202 Accepted", StatusCode: http.StatusAccepted, Proto: resp.Proto, ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor,
			Header: http.Header{}, Body: http.NoBody, Request: req,
		}, nil
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, errRedirect
	}
	if resp.ContentLength >= 0 && op.bytes.Load()+resp.ContentLength > op.limit {
		op.tooLarge.Store(true)
		op.cancel()
		_ = resp.Body.Close()
		return nil, errTooLarge
	}
	resp.Body = &countingBody{op: op, rc: resp.Body}
	return resp, nil
}

type countingBody struct {
	op *operation
	rc interface {
		Read([]byte) (int, error)
		Close() error
	}
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && b.op.bytes.Add(int64(n)) > b.op.limit {
		b.op.tooLarge.Store(true)
		b.op.cancel() // aborts every request of the operation, this body's read included
		return 0, errTooLarge
	}
	return n, err
}

func (b *countingBody) Close() error { return b.rc.Close() }

// newHTTPClient builds the operation's own client: a transport that is not shared with any other
// operation (so no connection, and no header, can be reused for another target), no proxy from
// the environment (credentials go to the target and nowhere else), the default TLS verification,
// and no redirects.
func newHTTPClient(op *operation) (*http.Client, func()) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	tr.Proxy = nil
	tr.MaxResponseHeaderBytes = 64 << 10
	tr.MaxIdleConnsPerHost = 2
	return &http.Client{
		Transport: &guard{op: op, inner: tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, tr.CloseIdleConnections
}
