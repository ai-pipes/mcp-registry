package domain_test

import (
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

var (
	t0 = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
)

func ptr[T any](v T) *T { return &v }

// code returns the DomainError code of err, or "" when err is nil or not a DomainError.
func code(err error) string {
	if err == nil {
		return ""
	}
	de, ok := kernel.AsDomainError(err)
	if !ok {
		return "<not a DomainError>"
	}
	return de.Code
}

func wantCode(t *testing.T, err error, want string) {
	t.Helper()
	if got := code(err); got != want {
		t.Fatalf("error code = %q (%v), want %q", got, err, want)
	}
}

func wantMsg(t *testing.T, err error, wantCode, wantMessage string) {
	t.Helper()
	de, ok := kernel.AsDomainError(err)
	if !ok || de.Code != wantCode || de.Message != wantMessage {
		t.Fatalf("error = %v, want %s: %q", err, wantCode, wantMessage)
	}
}
