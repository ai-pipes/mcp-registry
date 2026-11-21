package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

func TestLoginLinkExpiry(t *testing.T) {
	exp := domain.LoginLinkExpiry(t0)
	if !exp.Equal(time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)) {
		t.Fatalf("expiry = %v", exp)
	}
	if domain.IsLoginLinkExpired(exp, exp.Add(-time.Millisecond)) {
		t.Error("1ms before expiry")
	}
	if !domain.IsLoginLinkExpired(exp, exp) {
		t.Error("at expiry (>=)")
	}
}

func TestLoginCodeShape(t *testing.T) {
	if got := domain.NewLoginCode("abc"); got != "ll_abc" {
		t.Fatalf("NewLoginCode = %q", got)
	}
	cases := map[string]bool{
		"ll_abc": true, "sk_abc": false, "": false,
		"ll_" + strings.Repeat("a", 97):  true,
		"ll_" + strings.Repeat("a", 100): false,
	}
	for in, want := range cases {
		if got := domain.LooksLikeLoginCode(in); got != want {
			t.Errorf("LooksLikeLoginCode(%d chars) = %v, want %v", len(in), got, want)
		}
	}
}

func TestLoginLinkURL(t *testing.T) {
	cases := [][2]string{
		{"https://mcp.example.com", "https://mcp.example.com/login#link=ll_x"},
		{"https://mcp.example.com/", "https://mcp.example.com/login#link=ll_x"},
		{"https://mcp.example.com//", "https://mcp.example.com//login#link=ll_x"}, // exactly one slash
		{"https://mcp.example.com/base/", "https://mcp.example.com/base/login#link=ll_x"},
		{"", "/login#link=ll_x"},
	}
	for _, c := range cases {
		if got := domain.LoginLinkURL(c[0], "ll_x"); got != c[1] {
			t.Errorf("LoginLinkURL(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func person(over func(*domain.Person)) domain.Person {
	last := t0
	p := domain.Person{Sub: "local-admin", CreatedAt: t0, LastLoginAt: &last}
	if over != nil {
		over(&p)
	}
	return p
}

func TestNeedsBootstrapLink(t *testing.T) {
	later := t0.Add(time.Nanosecond)
	earlier := t0.Add(-time.Second)
	cases := []struct {
		name   string
		people []domain.Person
		want   domain.BootstrapAction
	}{
		{"no people", nil, domain.BootstrapCreate},
		{"empty slice", []domain.Person{}, domain.BootstrapCreate},
		{"local admin, last login = created", []domain.Person{person(nil)}, domain.BootstrapReissue},
		{"local admin, never signed in (nil)", []domain.Person{person(func(p *domain.Person) { p.LastLoginAt = nil })}, domain.BootstrapReissue},
		{"local admin, last login before created", []domain.Person{person(func(p *domain.Person) { p.LastLoginAt = &earlier })}, domain.BootstrapReissue},
		{"local admin signed in after creation", []domain.Person{person(func(p *domain.Person) { p.LastLoginAt = &later })}, domain.BootstrapNone},
		{"local admin disabled", []domain.Person{person(func(p *domain.Person) { p.Disabled = true })}, domain.BootstrapNone},
		{"another sub", []domain.Person{person(func(p *domain.Person) { p.Sub = "ann" })}, domain.BootstrapNone},
		{"two people, first is local admin", []domain.Person{person(nil), person(func(p *domain.Person) { p.Sub = "ann" })}, domain.BootstrapNone},
		{"two people, both local-admin", []domain.Person{person(nil), person(nil)}, domain.BootstrapNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.NeedsBootstrapLink(c.people, "local-admin"); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	if domain.BootstrapCreate != "create" || domain.BootstrapReissue != "reissue" || domain.BootstrapNone != "none" {
		t.Fatal("action values")
	}
}
