package app

import (
	"context"
	"errors"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// SyncCounts is what a successful synchronisation changed: the tools that were new (or came back),
// the ones whose content changed, and the ones upstream no longer offers.
type SyncCounts struct {
	Added   int
	Changed int
	Removed int
}

// SyncCommand synchronises one server. Principal is the admin who pressed the button; nil is the
// scheduler (or a CLI run without --as): no admin check and no audit record.
type SyncCommand struct {
	Principal *Principal
	Slug      string
}

// CodeInternal is the code of a synchronisation that failed unexpectedly (see
// SyncServerRecordingFailure); its message is the stored internal failure text.
const CodeInternal = "internal"

// SyncOutcome is the result of one server in SyncAllServers: OK, or the code of the error
// (needs_connection, sync_failed, internal...).
type SyncOutcome struct {
	Slug string
	OK   bool
	Code string
}

// SyncServer fetches the upstream tools/list and stores it as the server's tool snapshot.
//
// Order of steps: the attempt's start time is read first (before the admin check; a slower, older
// attempt never overwrites a newer one); admin check; the server must exist and be active
// (not_found `active server "<slug>" not found`, nothing recorded); credentials; tools/list;
// on failure: the sync state with the failure text, then the audit record, then the error; on
// success: a second clock reading, the tool snapshot (tools and their events, one transaction),
// then the sync state (start time, error cleared, the admin remembered as the sync owner), then
// the audit record. On any failure the tools are untouched and last_sync_error says why.
//
// The error is a kernel.DomainError for what is expected: forbidden, not_found, needs_connection
// (the admin must connect the server; HTTP 400) and sync_failed (message: the failure text;
// HTTP 502). Anything else (the database, a secret no key opens) is returned as it is and not
// recorded; SyncServerRecordingFailure records it.
func SyncServer(ctx context.Context, deps Deps, cmd SyncCommand) (SyncCounts, error) {
	counts, refusal, err := syncServer(ctx, deps, cmd)
	if err != nil {
		return SyncCounts{}, err
	}
	if refusal != nil {
		return SyncCounts{}, refusal
	}
	return counts, nil
}

// syncServer separates what is expected (a refusal: the answer to the caller) from what is not.
func syncServer(ctx context.Context, deps Deps, cmd SyncCommand) (SyncCounts, *kernel.DomainError, error) {
	startedAt := deps.Clock.Now()
	if cmd.Principal != nil {
		if err := requireAdmin(*cmd.Principal); err != nil {
			return refused(err)
		}
	}
	server, err := deps.Servers.FindBySlug(ctx, cmd.Slug)
	if err != nil {
		return SyncCounts{}, nil, err
	}
	if server == nil || server.Status() != domain.StatusActive {
		return refused(kernel.NewDomainError(domain.CodeNotFound, `active server "`+cmd.Slug+`" not found`))
	}
	var startedBy *string
	if cmd.Principal != nil {
		id := cmd.Principal.UserID
		startedBy = &id
	}
	audit := func(meta map[string]any) error {
		if cmd.Principal == nil {
			return nil
		}
		return record(ctx, deps, *cmd.Principal, "server.sync", server.Slug(), meta)
	}
	fail := func(ctx context.Context, failure domain.SyncFailure) (SyncCounts, *kernel.DomainError, error) {
		if err := deps.Servers.SaveSyncState(ctx, server.Slug(), domain.SyncFailureRecord(failure, startedAt)); err != nil {
			return SyncCounts{}, nil, err
		}
		if err := audit(map[string]any{"error": string(failure)}); err != nil {
			return SyncCounts{}, nil, err
		}
		code := domain.CodeSyncFailed
		if failure == domain.FailureNeedsConnection {
			code = domain.CodeNeedsConnection
		}
		return SyncCounts{}, kernel.NewDomainError(code, domain.SyncFailureText(failure)), nil
	}

	headers, failure, err := headersFor(ctx, deps, server, startedBy)
	if err != nil {
		return SyncCounts{}, nil, err
	}
	if failure != "" {
		return fail(ctx, failure)
	}
	listed, err := deps.Upstream.ListTools(ctx, kernel.UpstreamTarget{
		URL: server.URL(), Headers: headers, Timeout: time.Duration(server.TimeoutMs()) * time.Millisecond,
		MaxResponseBytes: deps.MaxResponseBytes,
	})
	if err != nil {
		// A cancellation before or during tools/list comes back as a non-upstream error (the client
		// never turns the caller's cancellation into an upstream failure) and records nothing.
		if ue, ok := kernel.AsUpstreamError(err); ok {
			return fail(context.WithoutCancel(ctx), domain.SyncFailureOfUpstream(ue.Kind))
		}
		return SyncCounts{}, nil, err
	}
	// tools/list has answered: what follows is written whole, so tools are never updated while the
	// sync state and the audit record are lost to a cancelled caller.
	ctx = context.WithoutCancel(ctx)
	snapshots := make([]domain.ToolSnapshot, len(listed))
	for i, u := range listed {
		snapshots[i] = domain.SnapshotFromUpstream(u)
	}
	applied, err := deps.Tools.ApplySnapshot(ctx, server.Slug(), snapshots, deps.Clock.Now())
	if err != nil {
		return SyncCounts{}, nil, err
	}
	if err := deps.Servers.SaveSyncState(ctx, server.Slug(), domain.SyncSuccess(startedBy, startedAt)); err != nil {
		return SyncCounts{}, nil, err
	}
	counts := SyncCounts{
		Added: len(applied.AddedRefs), Changed: len(applied.ChangedRefs) - len(applied.AddedRefs), Removed: len(applied.RemovedRefs),
	}
	if err := audit(map[string]any{"added": counts.Added, "changed": counts.Changed, "removed": counts.Removed}); err != nil {
		return SyncCounts{}, nil, err
	}
	return counts, nil, nil
}

func refused(err error) (SyncCounts, *kernel.DomainError, error) {
	if de, ok := kernel.AsDomainError(err); ok {
		return SyncCounts{}, de, nil
	}
	return SyncCounts{}, nil, err
}

// headersFor builds the upstream headers for synchronising this server, or says why there are
// none: a failure to record (never both). An error is unexpected.
func headersFor(ctx context.Context, deps Deps, server *domain.Server, startedBy *string) (kernel.UpstreamHeaders, domain.SyncFailure, error) {
	switch server.Auth().CredentialSource() {
	case domain.SourceNone:
		return domain.HeaderFor(server.Auth(), ""), "", nil
	case domain.SourcePersonal:
		owner := server.SyncCredentialsOwner(startedBy)
		active := false
		if owner != nil {
			var err error
			if active, err = deps.Credentials.IsActiveAdmin(ctx, *owner); err != nil {
				return nil, "", err
			}
		}
		if !domain.MayLend(owner, active) {
			return nil, domain.FailureNeedsConnection, nil
		}
		credential, err := deps.Credentials.PersonalSecret(ctx, toServerCallTarget(server), *owner)
		if err != nil {
			return nil, "", err
		}
		if credential.Failure != "" {
			return nil, domain.FailureOfCredential(credential.Failure), nil
		}
		return domain.HeaderFor(server.Auth(), credential.Secret), "", nil
	default:
		sealed := server.SealedSecret()
		if sealed == nil {
			return nil, domain.FailureNoSecret, nil
		}
		secret, err := deps.Box.Open(sealed)
		if err != nil {
			return nil, "", err
		}
		return domain.HeaderFor(server.Auth(), secret), "", nil
	}
}

// SyncServerRecordingFailure is SyncServer that absorbs an unexpected error (a database blip, a
// secret no key opens): it is logged without its message and recorded as the server's internal
// sync failure, and the answer is a DomainError with code "internal" and the stored text. Expected
// refusals pass through unchanged. A cancelled context (shutdown) is returned as it is and
// recorded as nothing.
func SyncServerRecordingFailure(ctx context.Context, deps Deps, cmd SyncCommand) (SyncCounts, error) {
	startedAt := deps.Clock.Now()
	counts, refusal, err := guardedSync(ctx, deps, cmd)
	if err == nil {
		if refusal != nil {
			return SyncCounts{}, refusal
		}
		return counts, nil
	}
	if ctx.Err() != nil {
		return SyncCounts{}, err
	}
	logError(deps, "catalog sync failed", "server", cmd.Slug, "err", kernel.ErrorLabel(err))
	if stateErr := deps.Servers.SaveSyncState(ctx, cmd.Slug, domain.SyncFailureRecord(domain.FailureInternal, startedAt)); stateErr != nil {
		logError(deps, "sync state not saved", "server", cmd.Slug, "err", kernel.ErrorLabel(stateErr))
	}
	return SyncCounts{}, kernel.NewDomainError(CodeInternal, domain.SyncFailureText(domain.FailureInternal))
}

// errPanicked stands for a recovered panic; its value is never kept (it may quote secrets).
type errPanicked struct{}

func (errPanicked) Error() string { return "panic during sync" }

// Code is the content-free label ErrorLabel logs.
func (errPanicked) Code() string { return "panic" }

// guardedSync is syncServer with a panic turned into an unexpected error.
func guardedSync(ctx context.Context, deps Deps, cmd SyncCommand) (counts SyncCounts, refusal *kernel.DomainError, err error) {
	defer func() {
		if recover() != nil {
			counts, refusal, err = SyncCounts{}, nil, errPanicked{}
		}
	}()
	return syncServer(ctx, deps, cmd)
}

func logError(deps Deps, msg string, args ...any) {
	if deps.Logger != nil {
		deps.Logger.Error(msg, args...)
	}
}

// SyncAllServers is the scheduler's run: every active server in turn (sequential: a few
// upstreams, and no burst against them every interval) as the scheduler (no admin, no audit).
// One failure does not stop the others, not even an unexpected one (recorded as internal). The
// outcomes are in slug order. A cancelled context ends the run early and is returned with the
// outcomes so far.
func SyncAllServers(ctx context.Context, deps Deps) ([]SyncOutcome, error) {
	listing, err := deps.Servers.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []SyncOutcome{}
	for _, l := range listing {
		if l.Server.Status() != domain.StatusActive {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		slug := l.Server.Slug()
		_, err := SyncServerRecordingFailure(ctx, deps, SyncCommand{Slug: slug})
		switch {
		case err == nil:
			out = append(out, SyncOutcome{Slug: slug, OK: true})
		case ctx.Err() != nil:
			return out, ctx.Err()
		default:
			code := CodeInternal
			if de, ok := kernel.AsDomainError(err); ok {
				code = de.Code
			}
			out = append(out, SyncOutcome{Slug: slug, Code: code})
		}
	}
	return out, nil
}

// RegisterServer creates a server and, as the creating admin, synchronises it at once. A failed
// synchronisation (needs_connection, upstream down, an unexpected error...) does not undo the
// creation and is not an error of the registration: it is recorded as the server's
// last_sync_error, which the returned view carries (the REST route answers created with that
// view; the CLI prints "sync failed: <LastSyncError>" and exits 1). The view is read after the
// sync, so LastSyncAt, LastSyncError and ToolCount are current. A context that ends while the
// sync waits on the upstream is the one exception: the view of the created server comes back with
// ErrFirstSyncInterrupted.
func RegisterServer(ctx context.Context, deps Deps, cmd CreateServerCommand) (ServerView, error) {
	// The creation (the row, its events, its audit records) is written whole: a cancelled caller
	// does not decide, half-way, whether the server exists.
	created, err := CreateServer(context.WithoutCancel(ctx), deps, cmd)
	if err != nil {
		return ServerView{}, err
	}
	// The server exists: whatever the sync answers, the creation stands.
	_, syncErr := SyncServerRecordingFailure(ctx, deps, SyncCommand{Principal: &cmd.Principal, Slug: created.Slug})
	_, answered := kernel.AsDomainError(syncErr)
	interrupted := syncErr != nil && !answered && ctx.Err() != nil
	view := created
	listing, err := deps.Servers.List(context.WithoutCancel(ctx))
	if err != nil {
		return ServerView{}, err
	}
	for _, l := range listing {
		if l.Server.Slug() == created.Slug {
			view = toServerView(l.Server, l.ToolCount)
			break
		}
	}
	if interrupted {
		return view, ErrFirstSyncInterrupted
	}
	return view, nil
}

// ErrFirstSyncInterrupted is what RegisterServer returns, together with the view of the server
// that was created, when the caller's context ended while the first synchronisation waited on the
// upstream: nothing about that attempt is recorded.
var ErrFirstSyncInterrupted = errors.New("first sync interrupted")
