package app

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

// DefaultCallsLimit is the page size when the caller names none.
const DefaultCallsLimit = 100

// ListCallsQuery is a page of the call log. Nil filters are not applied; Limit 0 is
// DefaultCallsLimit.
type ListCallsQuery struct {
	Principal Principal
	UserID    *string
	Server    *string
	Outcome   *domain.CallOutcome
	Before    *time.Time
	BeforeID  *string
	Limit     int
}

// ListCalls is the call log, newest first, for administrators only (a member is refused with
// forbidden before the log is read). Before is cut to the millisecond, the precision the log
// stores.
func ListCalls(ctx context.Context, d Deps, q ListCallsQuery) ([]CallRecord, error) {
	if q.Principal.Role != identity.RoleAdmin {
		return nil, kernel.NewDomainError("forbidden", "administrators only")
	}
	f := CallLogFilter{UserID: q.UserID, Server: q.Server, Outcome: q.Outcome, BeforeID: q.BeforeID, Limit: q.Limit}
	if f.Limit == 0 {
		f.Limit = DefaultCallsLimit
	}
	if q.Before != nil {
		b := q.Before.Truncate(time.Millisecond)
		f.Before = &b
	}
	rows, err := d.CallLog.List(ctx, f)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []CallRecord{}
	}
	return rows, nil
}
