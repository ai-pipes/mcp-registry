package domain_test

import (
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestQuotaDayIsTheUTCDay(t *testing.T) {
	day, reset := domain.QuotaDayOf(at(t, "2026-10-30T13:45:00Z"))
	if day != "2026-10-30" || !reset.Equal(at(t, "2026-10-31T00:00:00Z")) {
		t.Fatalf("got %s %s", day, reset)
	}
}

func TestQuotaDayIgnoresTheOffset(t *testing.T) {
	if d, _ := domain.QuotaDayOf(at(t, "2026-10-30T23:30:00-00:00")); d != "2026-10-30" {
		t.Fatal(d)
	}
	if d, _ := domain.QuotaDayOf(at(t, "2026-10-31T01:30:00+02:00")); d != "2026-10-30" {
		t.Fatal(d)
	}
}

func TestQuotaDayMidnightStartsTheNewDay(t *testing.T) {
	if d, _ := domain.QuotaDayOf(at(t, "2026-10-31T00:00:00Z")); d != "2026-10-31" {
		t.Fatal(d)
	}
}

func TestQuotaDayCrossesMonthAndYear(t *testing.T) {
	day, reset := domain.QuotaDayOf(at(t, "2026-12-31T22:00:00Z"))
	if day != "2026-12-31" || !reset.Equal(at(t, "2027-01-01T00:00:00Z")) {
		t.Fatalf("got %s %s", day, reset)
	}
}

func TestQuotaResetAtOfAStoredDay(t *testing.T) {
	r, err := domain.QuotaResetAt("2028-02-28")
	if err != nil || !r.Equal(at(t, "2028-02-29T00:00:00Z")) {
		t.Fatalf("got %s %v", r, err)
	}
	if _, err := domain.QuotaResetAt("not-a-day"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestQuotaOldestKept(t *testing.T) {
	if got := domain.QuotaOldestKept(at(t, "2026-10-30T00:30:00Z")); got != "2026-10-29" {
		t.Fatal(got)
	}
	if got := domain.QuotaOldestKept(at(t, "2026-01-01T23:00:00Z")); got != "2025-12-31" {
		t.Fatal(got)
	}
}
