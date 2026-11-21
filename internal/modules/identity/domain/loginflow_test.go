package domain_test

import (
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

func TestNormalizeReturnTo(t *testing.T) {
	cases := [][2]string{
		{"/servers/gitea", "/servers/gitea"},
		{"/tokens?x=1#h", "/tokens?x=1#h"},
		{"/admin/servers/new?x=1#y", "/admin/servers/new?x=1#y"},
		{"/", "/"},
		{"https://evil.example/", "/"},
		{"//evil", "/"},
		{"//", "/"},
		{"/\\evil", "/"},
		{"javascript:alert(1)", "/"},
		{"", "/"},
		{"servers", "/"},
		{"/a\r\nb", "/"},
		{"/a\nb", "/"},
		{"/a\tb", "/"},
		{"/a\x00b", "/"},
		{"/a\x1fb", "/"},
		{"/a\x7fb", "/"},
		{"/a b", "/a b"},
		{"/café", "/café"},
		{"/a\u0085b", "/a\u0085b"}, // C1 control: only < 0x20 and 0x7f count
	}
	for _, c := range cases {
		if got := domain.NormalizeReturnTo(c[0]); got != c[1] {
			t.Errorf("NormalizeReturnTo(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestLoginStateLifetime(t *testing.T) {
	exp := domain.LoginStateExpiry(t0)
	if exp.Sub(t0) != 10*time.Minute {
		t.Fatalf("ttl = %v", exp.Sub(t0))
	}
	if domain.IsLoginStateExpired(exp, exp.Add(-time.Nanosecond)) {
		t.Error("1ns before")
	}
	if !domain.IsLoginStateExpired(exp, exp) {
		t.Error("at expiry (>=)")
	}
	if domain.FailedCallbackAuditLimit != 20 || domain.FailedCallbackWindowSec != 60 {
		t.Error("callback audit throttle constants")
	}
}
