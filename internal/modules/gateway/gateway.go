// Package gateway is the public surface of the gateway module: call_tool (order of checks,
// argument validation, credentials, the daily limit, the upstream call, the call log), the call
// log for administrators and the retention of the gateway's history. The MCP layer and the REST
// route of the call log (the http subpackage) and the composition use the module only through
// this package.
//
// This package's tests live in gateway/http (service_test.go): its ports name the catalog's,
// connections' and identity's types, and the architecture rule for the root package
// (.go-arch-lint.yml, which also checks test files) forbids the root and its tests to import
// other modules.
package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

// The types the composition and the transport exchange with the module. They are aliases, so a
// value of one is a value of the module's own type.
type (
	// Principal is who is calling: the identity module's principal.
	Principal = app.Principal
	// Deps is what the use cases need; the composition fills it (adapters in the infra package).
	Deps = app.Deps
	// ToolCatalog is what the gateway reads of the catalog.
	ToolCatalog = app.ToolCatalog
	// PersonalCredentials is a person's own secret for a server (*connections.Service).
	PersonalCredentials = app.PersonalCredentials
	// ArgsValidator checks a call's arguments against the tool's input schema.
	ArgsValidator = app.ArgsValidator
	// ArgsCheck is the verdict of an ArgsValidator.
	ArgsCheck = app.ArgsCheck
	// SecretComparer compares two secrets in constant time.
	SecretComparer = app.SecretComparer
	// CallLog is the call log store.
	CallLog = app.CallLog
	// CallLogEntry is one row of the call log (never arguments or results).
	CallLogEntry = app.CallLogEntry
	// CallLogFilter selects call log rows.
	CallLogFilter = app.CallLogFilter
	// CallRecord is a stored call log row with its id.
	CallRecord = app.CallRecord
	// Quota is the daily counter store.
	Quota = app.Quota
	// QuotaRequest is one unit against a daily limit.
	QuotaRequest = app.QuotaRequest
	// Logger receives the failures the use cases absorb (*slog.Logger fits).
	Logger = app.Logger
	// CallToolCommand is one call_tool: who, which tool, the agent's raw arguments.
	CallToolCommand = app.CallToolCommand
	// ListCallsQuery is a page of the call log; nil filters are not applied.
	ListCallsQuery = app.ListCallsQuery
	// PruneResult is what a retention run deleted.
	PruneResult = app.PruneResult
	// ToolResult is the agent's answer to call_tool; its JSON method gives the wire bytes (never
	// pass it through json.Marshal, which would HTML-escape the upstream's bytes).
	ToolResult = domain.ToolResult
	// CallOutcome is how a call ended, as the call log records it.
	CallOutcome = domain.CallOutcome
)

// The outcomes of a call.
const (
	OutcomeOK                    = domain.OutcomeOK
	OutcomeUpstreamError         = domain.OutcomeUpstreamError
	OutcomeTimeout               = domain.OutcomeTimeout
	OutcomeTransportError        = domain.OutcomeTransportError
	OutcomeDeniedNotFound        = domain.OutcomeDeniedNotFound
	OutcomeDeniedInvalidArgs     = domain.OutcomeDeniedInvalidArgs
	OutcomeDeniedQuota           = domain.OutcomeDeniedQuota
	OutcomeDeniedNeedsConnection = domain.OutcomeDeniedNeedsConnection
	OutcomeDeniedExpired         = domain.OutcomeDeniedExpired
)

// DefaultCallsLimit is the page size of the call log when the caller names none.
const DefaultCallsLimit = app.DefaultCallsLimit

// AllOutcomes lists every outcome in the order of the database CHECK (a copy).
func AllOutcomes() []CallOutcome { return domain.AllOutcomes() }

// ParseOutcome is the outcome named s (exact case).
func ParseOutcome(s string) (CallOutcome, bool) { return domain.ParseOutcome(s) }

// Service is the module's surface for the transport and the composition. Build it once with New;
// it is safe for concurrent use as far as its ports are.
type Service struct{ d app.Deps }

// New builds the service. Every port and PublicURL and MaxResponseBytes must be set (BaseContext
// may be nil: calls are then never cancelled): a missing one is an error naming it, so a
// half-wired composition fails at start and not on the first call.
func New(d Deps) (*Service, error) {
	for _, c := range []struct {
		name    string
		missing bool
	}{
		{"Catalog", d.Catalog == nil},
		{"Credentials", d.Credentials == nil},
		{"Validator", d.Validator == nil},
		{"CallLog", d.CallLog == nil},
		{"Quota", d.Quota == nil},
		{"Upstream", d.Upstream == nil},
		{"Clock", d.Clock == nil},
		{"Logger", d.Logger == nil},
		{"Secrets", d.Secrets == nil},
		{"PublicURL", d.PublicURL == ""},
		{"MaxResponseBytes", d.MaxResponseBytes <= 0},
	} {
		if c.missing {
			return nil, errors.New("gateway: Deps." + c.name + " is not set")
		}
	}
	return &Service{d: d}, nil
}

// CallTool runs one tool of an upstream for a person (see app.CallTool). A nil error comes with
// every answer the agent should see, refusals included; an error is an unexpected failure, already
// in the call log as transport_error, which the transport answers with "Internal error.". The call
// and its log row survive the request's context (they end with Deps.BaseContext).
func (s *Service) CallTool(ctx context.Context, c CallToolCommand) (ToolResult, error) {
	return app.CallTool(ctx, s.d, c)
}

// ListCalls is a page of the call log, newest first; a member gets a forbidden DomainError.
func (s *Service) ListCalls(ctx context.Context, q ListCallsQuery) ([]CallRecord, error) {
	return app.ListCalls(ctx, s.d, q)
}

// PruneHistory deletes call log rows older than retentionDays 24-hour days and the quota counters
// of the days before yesterday (UTC).
func (s *Service) PruneHistory(ctx context.Context, retentionDays int) (PruneResult, error) {
	return app.PruneGatewayHistory(ctx, s.d, retentionDays)
}

// RetentionEvery is the period of the retention job (it also runs at start).
const RetentionEvery = 24 * time.Hour

// The records of the retention job (scheduler.ts).
const (
	MsgRetentionDone   = "gateway retention: done"
	MsgRetentionFailed = "gateway retention: run failed"
)

// RetentionLogger receives the retention job's records (*slog.Logger fits).
type RetentionLogger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

// RetentionJob is the body of the scheduled retention job: PruneHistory, then one record, "gateway
// retention: done" with the counts or "gateway retention: run failed" with the error's label (never
// its text). It returns nil either way: the job logs its own failure and the schedule goes on.
func (s *Service) RetentionJob(retentionDays int, log RetentionLogger) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		res, err := s.PruneHistory(ctx, retentionDays)
		if err != nil {
			log.Error(MsgRetentionFailed, "err", kernel.ErrorLabel(err))
			return nil
		}
		log.Info(MsgRetentionDone, "calls", res.Calls, "quotaDays", res.QuotaDays)
		return nil
	}
}
