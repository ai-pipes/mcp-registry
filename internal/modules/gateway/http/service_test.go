package http_test

// The root package's Service (gateway.New, the use cases, the retention job), tested here because
// its ports name the other modules' types, which the root package (and its tests) may not import.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

type stubCatalog struct{}

func (stubCatalog) ToolFor(context.Context, gateway.Principal, string) (catalog.ToolView, bool, error) {
	return catalog.ToolView{}, false, nil
}

func (stubCatalog) ServerFor(context.Context, string) (*catalog.ServerCallTarget, error) {
	return nil, nil
}

func (stubCatalog) ServerSecret(context.Context, catalog.ServerCallTarget) (kernel.Secret, catalog.SecretLookup, error) {
	return kernel.Secret{}, catalog.SecretNotSet, nil
}

type stubCreds struct{}

func (stubCreds) ResolveForSending(context.Context, string, catalog.ServerCallTarget, bool) (connections.CredentialResult, error) {
	return connections.CredentialResult{}, nil
}
func (stubCreds) MarkExpired(context.Context, connections.MarkExpiredCommand) error { return nil }

type stubValidator struct{}

func (stubValidator) Validate(string, json.RawMessage, json.RawMessage) gateway.ArgsCheck {
	return gateway.ArgsCheck{OK: true}
}

type stubLog struct {
	rows      []gateway.CallLogEntry
	deleteErr error
	cutoff    time.Time
}

func (l *stubLog) Record(_ context.Context, e gateway.CallLogEntry) error {
	l.rows = append(l.rows, e)
	return nil
}

func (l *stubLog) List(context.Context, gateway.CallLogFilter) ([]gateway.CallRecord, error) {
	return []gateway.CallRecord{{ID: "7"}}, nil
}

func (l *stubLog) DeleteOlderThan(_ context.Context, t time.Time) (int64, error) {
	l.cutoff = t
	return 4, l.deleteErr
}

type stubQuota struct{ day string }

func (q *stubQuota) TryConsume(context.Context, gateway.QuotaRequest) (bool, error) { return true, nil }

func (q *stubQuota) DeleteDaysBefore(_ context.Context, day string) (int64, error) {
	q.day = day
	return 2, nil
}

type stubUpstream struct{}

func (stubUpstream) ListTools(context.Context, kernel.UpstreamTarget) ([]kernel.UpstreamTool, error) {
	return nil, nil
}

func (stubUpstream) CallTool(context.Context, kernel.UpstreamTarget, string, json.RawMessage) (kernel.UpstreamCallResult, error) {
	return nil, nil
}

type stubSecrets struct{}

func (stubSecrets) Equal(a, b kernel.Secret) bool { return a.Reveal() == b.Reveal() }

type records struct{ lines []string }

func (r *records) Info(msg string, args ...any) {
	r.lines = append(r.lines, "INFO "+msg+" "+strings.TrimSpace(fmt.Sprintln(args...)))
}

func (r *records) Error(msg string, args ...any) {
	r.lines = append(r.lines, "ERROR "+msg+" "+strings.TrimSpace(fmt.Sprintln(args...)))
}

func fullDeps() (gateway.Deps, *stubLog, *stubQuota) {
	log, quota := &stubLog{}, &stubQuota{}
	return gateway.Deps{
		Catalog: stubCatalog{}, Credentials: stubCreds{}, Validator: stubValidator{}, CallLog: log, Quota: quota,
		Upstream: stubUpstream{}, Clock: kernel.FixedClock{T: time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)},
		Logger: &records{}, Secrets: stubSecrets{}, PublicURL: "http://localhost:3000", MaxResponseBytes: 1 << 20,
	}, log, quota
}

func TestNewRefusesAHalfWiredComposition(t *testing.T) {
	for _, c := range []struct {
		name string
		drop func(*gateway.Deps)
	}{
		{"Catalog", func(d *gateway.Deps) { d.Catalog = nil }},
		{"Credentials", func(d *gateway.Deps) { d.Credentials = nil }},
		{"Validator", func(d *gateway.Deps) { d.Validator = nil }},
		{"CallLog", func(d *gateway.Deps) { d.CallLog = nil }},
		{"Quota", func(d *gateway.Deps) { d.Quota = nil }},
		{"Upstream", func(d *gateway.Deps) { d.Upstream = nil }},
		{"Clock", func(d *gateway.Deps) { d.Clock = nil }},
		{"Logger", func(d *gateway.Deps) { d.Logger = nil }},
		{"Secrets", func(d *gateway.Deps) { d.Secrets = nil }},
		{"PublicURL", func(d *gateway.Deps) { d.PublicURL = "" }},
		{"MaxResponseBytes", func(d *gateway.Deps) { d.MaxResponseBytes = 0 }},
	} {
		d, _, _ := fullDeps()
		c.drop(&d)
		if _, err := gateway.New(d); err == nil || !strings.Contains(err.Error(), "Deps."+c.name) {
			t.Errorf("without %s: %v", c.name, err)
		}
	}
	d, _, _ := fullDeps()
	if _, err := gateway.New(d); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRunsTheUseCases(t *testing.T) {
	d, log, _ := fullDeps()
	svc, err := gateway.New(d)
	if err != nil {
		t.Fatal(err)
	}
	member := gateway.Principal{UserID: "u1", Role: identity.RoleMember, Via: identity.ViaToken, TokenID: "t1"}
	res, err := svc.CallTool(t.Context(), gateway.CallToolCommand{Principal: member, Ref: "gitea.nope"})
	if err != nil || !strings.Contains(string(res.JSON()), `is not available`) {
		t.Fatalf("CallTool: %s %v", res.JSON(), err)
	}
	if len(log.rows) != 1 || log.rows[0].Outcome != gateway.OutcomeDeniedNotFound || log.rows[0].TokenID != "t1" {
		t.Fatalf("call log: %+v", log.rows)
	}
	if _, err := svc.ListCalls(t.Context(), gateway.ListCallsQuery{Principal: member}); err == nil {
		t.Fatal("a member lists the call log")
	}
	admin := member
	admin.Role = identity.RoleAdmin
	if rows, err := svc.ListCalls(t.Context(), gateway.ListCallsQuery{Principal: admin}); err != nil || len(rows) != 1 {
		t.Fatalf("ListCalls: %v %v", rows, err)
	}
	if got, ok := gateway.ParseOutcome("denied:quota"); !ok || got != gateway.OutcomeDeniedQuota || len(gateway.AllOutcomes()) != 9 {
		t.Fatalf("outcomes: %v %v %v", got, ok, gateway.AllOutcomes())
	}
}

func TestRetentionJobLogsWhatItDeletedOrWhyItFailed(t *testing.T) {
	d, log, quota := fullDeps()
	svc, err := gateway.New(d)
	if err != nil {
		t.Fatal(err)
	}
	rec := &records{}
	job := svc.RetentionJob(90, rec)
	if err := job(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC); !log.cutoff.Equal(want) || quota.day != "2026-10-29" {
		t.Fatalf("cutoff %v day %q", log.cutoff, quota.day)
	}
	if len(rec.lines) != 1 || rec.lines[0] != "INFO gateway retention: done calls 4 quotaDays 2" {
		t.Fatalf("records %q", rec.lines)
	}
	log.deleteErr = errors.New("secret text that must not be logged")
	rec.lines = nil
	if err := job(t.Context()); err != nil {
		t.Fatalf("a failed run returns %v: it logs its own failure", err)
	}
	if len(rec.lines) != 1 || !strings.HasPrefix(rec.lines[0], "ERROR gateway retention: run failed err ") || strings.Contains(rec.lines[0], "secret text") {
		t.Fatalf("records %q", rec.lines)
	}
	if gateway.RetentionEvery != 24*time.Hour {
		t.Fatal(gateway.RetentionEvery)
	}
}
