package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

func admin() app.Principal { return identity.Principal{UserID: "u-admin", Role: identity.RoleAdmin} }

func TestListCallsRefusesMembersWithoutReadingTheLog(t *testing.T) {
	h := newHarness()
	_, err := app.ListCalls(context.Background(), h.d, app.ListCallsQuery{Principal: h.alice})
	de, ok := kernel.AsDomainError(err)
	if !ok || de.Code != "forbidden" || de.Message != "administrators only" || h.log.listed {
		t.Fatalf("err %v listed %v", err, h.log.listed)
	}
}

func TestListCallsDefaultsAndFilters(t *testing.T) {
	h := newHarness()
	rows, err := app.ListCalls(context.Background(), h.d, app.ListCallsQuery{Principal: admin(), Server: sp("gitea")})
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("rows %#v err %v", rows, err)
	}
	if f := h.log.filter; f.Limit != 100 || f.Server == nil || *f.Server != "gitea" || f.UserID != nil || f.Outcome != nil || f.Before != nil || f.BeforeID != nil {
		t.Fatalf("filter %+v", f)
	}
	oc := domain.OutcomeOK
	before := time.Date(2026, 10, 30, 12, 0, 0, 123987000, time.UTC)
	h.log.stored = []app.CallRecord{{ID: "7"}}
	rows, err = app.ListCalls(context.Background(), h.d, app.ListCallsQuery{Principal: admin(), UserID: sp("u"), Outcome: &oc, Before: &before, BeforeID: sp("9"), Limit: 5})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	f := h.log.filter
	if f.Limit != 5 || *f.UserID != "u" || *f.Outcome != oc || *f.BeforeID != "9" || !f.Before.Equal(before.Truncate(time.Millisecond)) || f.Before.Nanosecond() != 123000000 {
		t.Fatalf("filter %+v", f)
	}
}

func TestPruneGatewayHistory(t *testing.T) {
	h := newHarness()
	h.d.Clock = kernel.FixedClock{T: time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)}
	r, err := app.PruneGatewayHistory(context.Background(), h.d, 90)
	if err != nil || r != (app.PruneResult{Calls: 4, QuotaDays: 2}) {
		t.Fatalf("%+v %v", r, err)
	}
	if want := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC); !h.log.cutoff.Equal(want) {
		t.Fatalf("cutoff %v, want %v", h.log.cutoff, want)
	}
	if h.quota.before != "2026-10-29" {
		t.Fatalf("quota day %q", h.quota.before)
	}
}

func TestPruneStopsWhenTheLogDeleteFails(t *testing.T) {
	h := newHarness()
	h.log.delErr = errors.New("x")
	if _, err := app.PruneGatewayHistory(context.Background(), h.d, 90); err == nil || h.quota.delCalls != 0 {
		t.Fatalf("err %v, quota deletes %d", err, h.quota.delCalls)
	}
}
