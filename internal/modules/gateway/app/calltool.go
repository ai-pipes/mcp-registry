package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

// logWriteBound is how long the call-log write may take after the call itself.
const logWriteBound = 10 * time.Second

// CallToolCommand is one call_tool: who, which tool, and the arguments exactly as the agent sent
// them (raw JSON object; nil or empty means none).
type CallToolCommand struct {
	Principal Principal
	Ref       string
	Arguments json.RawMessage
}

// ended is how a call ended: the outcome the log gets and the answer the agent gets.
type ended struct {
	outcome domain.CallOutcome
	result  domain.ToolResult
}

func failed(outcome domain.CallOutcome, text string) ended {
	return ended{outcome: outcome, result: domain.FailureResult(text)}
}

// CallTool runs one tool of an upstream for a person. Order: the tool is usable by the person now
// -> the arguments against the input schema -> credentials -> the daily limit -> the upstream call.
// Nothing before the limit spends quota; everything after has spent one unit, never refunded; the
// retry after a 401 does not spend a second one.
//
// The answer is the upstream's result (content, an object structuredContent, isError) or an
// isError result with the short text of the failure. The error is for unexpected failures only (a
// store, a key, a bug); the call is logged transport_error then and the caller answers "Internal
// error.".
//
// The call and its log row do not depend on the client leaving: both run on a context detached
// from ctx (ended only by Deps.BaseContext). The log write never changes the answer.
func CallTool(ctx context.Context, d Deps, c CallToolCommand) (domain.ToolResult, error) {
	startedAt := d.Clock.Now().Truncate(time.Millisecond)
	call, cancel := detach(ctx, d.BaseContext)
	defer cancel()

	server, tool := "", c.Ref
	if s, t, err := catalog.ParseToolRef(c.Ref); err == nil {
		server, tool = s, t
	}
	outcome := domain.OutcomeTransportError
	res, err := guarded(call, d, c)
	if err == nil {
		outcome = res.outcome
	}
	entry := CallLogEntry{
		UserID:     c.Principal.UserID,
		TokenID:    c.Principal.TokenID,
		Server:     domain.BoundName(server),
		Tool:       domain.BoundName(tool),
		StartedAt:  startedAt,
		DurationMs: max(0, d.Clock.Now().Truncate(time.Millisecond).Sub(startedAt).Milliseconds()),
		Outcome:    outcome,
	}
	record(call, d, entry)
	if err != nil {
		return domain.ToolResult{}, err
	}
	return res.result, nil
}

// detach is a context that outlives the request's but ends with base.
func detach(req, base context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(req))
	if base == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(base, cancel)
	if base.Err() != nil {
		// AfterFunc would cancel from its own goroutine, a moment after the call has started.
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

// record writes the log row. The write gets a fresh bounded context even when the process is
// shutting down; a failure is logged (no message text) and swallowed.
func record(call context.Context, d Deps, e CallLogEntry) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(call), logWriteBound)
	defer cancel()
	if err := d.CallLog.Record(wctx, e); err != nil {
		d.Logger.Error("gateway: call log record failed", "err", kernel.ErrorLabel(err), "server", e.Server, "tool", e.Tool, "outcome", string(e.Outcome))
	}
}

// errCallPanicked is the error of a call that panicked: the panic value is never kept (it could
// quote the arguments or a result), only this fixed text and its type's name reach the log.
type errCallPanicked struct{}

func (errCallPanicked) Error() string { return "gateway: call_tool panicked" }

// guarded is run with a panic turned into errCallPanicked, so that the row is written as the
// reference's finally writes it (transport_error) and the caller answers Internal error.
func guarded(ctx context.Context, d Deps, c CallToolCommand) (res ended, err error) {
	defer func() {
		if recover() != nil {
			res, err = ended{}, errCallPanicked{}
		}
	}()
	return run(ctx, d, c)
}

func run(ctx context.Context, d Deps, c CallToolCommand) (ended, error) {
	tool, ok, err := d.Catalog.ToolFor(ctx, c.Principal, c.Ref)
	if err != nil {
		return ended{}, err
	}
	if !ok {
		return failed(domain.OutcomeDeniedNotFound, domain.TextNotFound(c.Ref)), nil
	}
	// The server may have been disabled since the tool was read.
	server, err := d.Catalog.ServerFor(ctx, tool.Server)
	if err != nil {
		return ended{}, err
	}
	if server == nil {
		return failed(domain.OutcomeDeniedNotFound, domain.TextNotFound(c.Ref)), nil
	}
	args := c.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if check := d.Validator.Validate(c.Ref, tool.InputSchema, args); !check.OK {
		return failed(domain.OutcomeDeniedInvalidArgs, domain.TextInvalidArguments(c.Ref, check.Path, check.Message)), nil
	}
	secret, denied, err := secretFor(ctx, d, server, c.Principal.UserID, false)
	if err != nil || denied != nil {
		return deniedOr(denied), err
	}
	if server.DailyLimit != nil {
		day, resetAt := domain.QuotaDayOf(d.Clock.Now())
		fits, err := d.Quota.TryConsume(ctx, QuotaRequest{UserID: c.Principal.UserID, Server: server.Slug, Day: day, Limit: *server.DailyLimit})
		if err != nil {
			return ended{}, err
		}
		if !fits {
			return failed(domain.OutcomeDeniedQuota, domain.TextQuota(server.Name, resetAt)), nil
		}
	}
	return send(ctx, d, server, c.Principal.UserID, tool.Name, args, secret)
}

func deniedOr(e *ended) ended {
	if e == nil {
		return ended{}
	}
	return *e
}

// secretFor is the secret to send to the server: none for a server without authentication, the
// server's own for shared_secret, the person's for oauth_user and pat_user. A refusal is an
// answer (denied); the error is for unexpected failures.
func secretFor(ctx context.Context, d Deps, server *catalog.ServerCallTarget, userID string, forceRefresh bool) (string, *ended, error) {
	switch server.Auth.CredentialSource() {
	case catalog.SourceNone:
		return "", nil, nil
	case catalog.SourceServerSecret:
		secret, ok, err := d.Catalog.ServerSecret(ctx, server.Slug)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			e := failed(domain.OutcomeTransportError, domain.TextRejected(server.Name))
			return "", &e, nil
		}
		return secret, nil, nil
	}
	r, err := d.Credentials.ResolveForSending(ctx, userID, *server, forceRefresh)
	if err != nil {
		return "", nil, err
	}
	if r.Ok() {
		return r.Secret, nil, nil
	}
	e, err := credentialFailure(d, server, domain.CredentialFailure(r.Failure))
	return "", &e, err
}

// credentialFailure is the answer for a credential that could not be had. An unknown failure is
// an error: never guess what a new one means to the agent.
func credentialFailure(d Deps, server *catalog.ServerCallTarget, f domain.CredentialFailure) (ended, error) {
	var text string
	switch f {
	case domain.CredentialNeedsConnection:
		text = domain.TextNeedsConnection(server.Name, publicURL(d))
	case domain.CredentialExpired:
		text = domain.TextExpired(server.Name, publicURL(d))
	case domain.CredentialUnavailable:
		text = domain.TextTransport(server.Name)
	default:
		return ended{}, fmt.Errorf("gateway: unknown credential failure %q", string(f))
	}
	return failed(domain.OutcomeOfCredentialFailure(f), text), nil
}

func publicURL(d Deps) string { return strings.TrimRight(d.PublicURL, "/") }

// attempt is one upstream call. unauthorized is true when upstream answered 401; any other
// upstream failure is an answer, and so is a call cut by the shutdown; any other error that is not
// an upstream failure is unexpected.
func attempt(ctx context.Context, d Deps, server *catalog.ServerCallTarget, tool string, args json.RawMessage, secret string) (res ended, unauthorized bool, err error) {
	target := kernel.UpstreamTarget{
		URL:              server.URL,
		Headers:          catalog.HeaderFor(server.Auth, secret),
		Timeout:          time.Duration(server.TimeoutMs) * time.Millisecond,
		MaxResponseBytes: d.MaxResponseBytes,
	}
	raw, err := d.Upstream.CallTool(ctx, target, tool, args)
	if err != nil {
		ue, isUpstream := kernel.AsUpstreamError(err)
		if !isUpstream {
			if ctx.Err() != nil {
				// The call's context is detached from the client's: only the shutdown ends it (its
				// grace is over). The server did not answer in time: its transport text.
				return failed(domain.OutcomeTransportError, domain.TextTransport(server.Name)), false, nil
			}
			return ended{}, false, err
		}
		if ue.Kind == kernel.UpstreamUnauthorized {
			return ended{}, true, nil
		}
		return upstreamFailure(d, server, ue), false, nil
	}
	projected, perr := domain.ProjectUpstreamResult(raw)
	if perr != nil {
		// Not a CallToolResult: as a client that cannot parse it, a transport failure.
		return failed(domain.OutcomeTransportError, domain.TextTransport(server.Name)), false, nil
	}
	return ended{outcome: domain.OutcomeOfResult(projected.IsError), result: projected}, false, nil
}

func upstreamFailure(d Deps, server *catalog.ServerCallTarget, ue *kernel.UpstreamError) ended {
	outcome := domain.OutcomeOfUpstreamFailure(ue.Kind)
	switch ue.Kind {
	case kernel.UpstreamRejected:
		rej := kernel.UpstreamRejection{}
		if ue.Rejection != nil {
			rej = *ue.Rejection
		}
		return failed(outcome, domain.TextUpstreamRejected(server.Name, rej))
	case kernel.UpstreamTimeout:
		return failed(outcome, domain.TextTimeout(server.Name, server.TimeoutMs))
	case kernel.UpstreamTooLarge:
		return failed(outcome, domain.TextTooLarge(server.Name, d.MaxResponseBytes))
	default:
		// Includes the client's own limits (a server that keeps asking requests of us, a malformed
		// answer): nothing an agent can act on, no internal detail.
		return failed(outcome, domain.TextTransport(server.Name))
	}
}

// send is the upstream call and what follows a 401. The server is the target read before the
// call: the retry uses it too, never a fresh read.
func send(ctx context.Context, d Deps, server *catalog.ServerCallTarget, userID, tool string, args json.RawMessage, secret string) (ended, error) {
	first, unauthorized, err := attempt(ctx, d, server, tool, args, secret)
	if err != nil || !unauthorized {
		return first, err
	}
	switch domain.OnUpstreamUnauthorized(domain.AuthKind(server.Auth.Kind)) {
	case domain.Reject:
		return failed(domain.OutcomeTransportError, domain.TextRejected(server.Name)), nil
	case domain.RefreshAndRetry:
		return refreshAndRetry(ctx, d, server, userID, tool, args, secret)
	default:
		return expire(ctx, d, server, userID)
	}
}

func expire(ctx context.Context, d Deps, server *catalog.ServerCallTarget, userID string) (ended, error) {
	if err := d.Credentials.MarkExpired(ctx, connections.MarkExpiredCommand{UserID: userID, Server: server.Slug}); err != nil {
		return ended{}, err
	}
	return failed(domain.OutcomeDeniedExpired, domain.TextExpired(server.Name, publicURL(d))), nil
}

// refreshAndRetry forces one refresh. The forced resolve returning the secret upstream just
// rejected (see domain.RefreshedSecretSame) is answered as unavailable: nothing is expired and
// upstream is not asked again. A different secret is tried once; a second 401 expires the
// connection.
func refreshAndRetry(ctx context.Context, d Deps, server *catalog.ServerCallTarget, userID, tool string, args json.RawMessage, rejected string) (ended, error) {
	fresh, denied, err := secretFor(ctx, d, server, userID, true)
	if err != nil {
		return ended{}, err
	}
	result := domain.RefreshedSecretChanged
	switch {
	case denied != nil:
		result = domain.RefreshFailed
	case d.Secrets.Equal(fresh, rejected):
		result = domain.RefreshedSecretSame
	}
	switch domain.AfterForcedRefresh(result) {
	case domain.ReportRefreshFailure:
		return *denied, nil
	case domain.ReportUnavailable:
		return failed(domain.OutcomeTransportError, domain.TextTransport(server.Name)), nil
	}
	second, unauthorized, err := attempt(ctx, d, server, tool, args, fresh)
	if err != nil || !unauthorized {
		return second, err
	}
	return expire(ctx, d, server, userID)
}
