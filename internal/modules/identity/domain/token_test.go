package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

func TestLooksLikeToken(t *testing.T) {
	cases := map[string]bool{
		"sk_abc":                         true,
		"abc":                            false,
		"":                               false,
		"sk_" + strings.Repeat("a", 97):  true,
		"sk_" + strings.Repeat("a", 98):  false,
		"sk_" + strings.Repeat("a", 200): false,
		"SK_abc":                         false,
		// Length counts UTF-16 units like JavaScript: an astral rune is two.
		"sk_" + strings.Repeat("\U0001F600", 48): true,  // 3 + 96 = 99
		"sk_" + strings.Repeat("\U0001F600", 49): false, // 3 + 98 = 101
	}
	for in, want := range cases {
		if got := domain.LooksLikeToken(in); got != want {
			t.Errorf("LooksLikeToken(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTokenValueAndPrefix(t *testing.T) {
	if got := domain.NewTokenValue("abc"); got != "sk_abc" {
		t.Fatalf("NewTokenValue = %q", got)
	}
	if got := domain.TokenPrefix("sk_0123456789abcdef"); got != "sk_0123456" {
		t.Fatalf("TokenPrefix = %q", got)
	}
	if got := domain.TokenPrefix("sk_a"); got != "sk_a" {
		t.Fatalf("short prefix = %q", got)
	}
}

func TestShouldTouchLastUsed(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	if !domain.ShouldTouchLastUsed(nil, now) {
		t.Error("never used")
	}
	if !domain.ShouldTouchLastUsed(ago(60*time.Second), now) {
		t.Error("exactly 60s old")
	}
	if domain.ShouldTouchLastUsed(ago(60*time.Second-time.Millisecond), now) {
		t.Error("59.999s old")
	}
	if domain.ShouldTouchLastUsed(ago(-time.Hour), now) {
		t.Error("a future lastUsedAt is fresh")
	}
}
