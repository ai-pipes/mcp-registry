package upstream

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

// operation is the state of one upstream operation: its deadline, what the HTTP layer saw and the
// byte budget. Nothing in it outlives the operation.
type operation struct {
	ctx      context.Context // the operation's deadline; cancelled when the byte limit is passed
	cancel   context.CancelFunc
	deadline time.Time
	headers  map[string]string
	limit    int64

	bytes        atomic.Int64
	tooLarge     atomic.Bool
	unauthorized atomic.Bool
	badStatus    atomic.Bool // some response was not 2xx (a redirect included)
	failed       atomic.Bool // the operation failed: no polite goodbye (DELETE) is sent
	protocol     atomic.Pointer[string]
}

var (
	errTooLarge = errors.New("response exceeds the size limit")
	errRedirect = errors.New("redirect refused")
	errFailed   = errors.New("operation already failed")
)

// guard is the RoundTripper of the operation's http.Client. It is the only place where the
// target's headers are added, so every request of the session carries them and nothing else of
// the caller's. It refuses redirects, notes a 401, counts the bytes of every response and stops
// the operation as soon as they pass the limit.
type guard struct {
	op    *operation
	inner http.RoundTripper
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	op := g.op
	r := req.Clone(req.Context())
	// The protocol's own headers (Content-Type, Accept, Mcp-*) win; the target's fill the rest.
	for k, v := range op.headers {
		if r.Header.Get(k) == "" {
			r.Header.Set(k, v)
		}
	}
	if pv := op.protocol.Load(); pv != nil && r.Header.Get("Mcp-Protocol-Version") == "" {
		r.Header.Set("Mcp-Protocol-Version", *pv)
	}
	if r.Method == http.MethodDelete {
		return g.terminate(r)
	}

	resp, err := g.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		op.unauthorized.Store(true)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		op.badStatus.Store(true)
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

// terminate sends the session-ending DELETE of the SDK's Close: only after a successful operation,
// only within what is left of the operation's deadline, and its answer counts for nothing.
func (g *guard) terminate(r *http.Request) (*http.Response, error) {
	op := g.op
	if op.failed.Load() || op.ctx.Err() != nil {
		return nil, errFailed
	}
	ctx, cancel := context.WithDeadline(r.Context(), op.deadline)
	stop := context.AfterFunc(op.ctx, cancel)
	resp, err := g.inner.RoundTrip(r.WithContext(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	resp.Body = &releasingBody{rc: resp.Body, release: func() { stop(); cancel() }}
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

type releasingBody struct {
	rc interface {
		Read([]byte) (int, error)
		Close() error
	}
	release func()
}

func (b *releasingBody) Read(p []byte) (int, error) { return b.rc.Read(p) }
func (b *releasingBody) Close() error {
	defer b.release()
	return b.rc.Close()
}

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
