package domain_test

import (
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

func TestConstantsMatchTheReference(t *testing.T) {
	if domain.Candidates != 50 || domain.RRFK != 60 || domain.Lists != 2 || domain.MaxPassageChars != 2000 || domain.DefaultMinSimilarity != 0.83 {
		t.Fatal("constants changed")
	}
}

func TestPrepareQueryTrimsWithJavaScriptTrim(t *testing.T) {
	got, ok := domain.PrepareQuery("\uFEFF \u00A0 find issues \u2028\n", 5, []string{"gitea"})
	if !ok || got != "find issues" {
		t.Fatalf("got %q, %v", got, ok)
	}
	// U+0085 and U+200B are not JavaScript whitespace
	got, ok = domain.PrepareQuery("\u0085x\u200B", 5, []string{"gitea"})
	if !ok || got != "\u0085x\u200B" {
		t.Fatalf("got %q, %v", got, ok)
	}
}

func TestPrepareQueryRefusesBlankQueryNonPositiveLimitAndNoServers(t *testing.T) {
	cases := []struct {
		name    string
		q       string
		limit   int
		servers []string
	}{
		{"empty", "", 5, []string{"a"}},
		{"blank", " \t\n\uFEFF", 5, []string{"a"}},
		{"limit zero", "x", 0, []string{"a"}},
		{"negative limit", "x", -1, []string{"a"}},
		{"no servers", "x", 5, nil},
		{"empty servers", "x", 5, []string{}},
	}
	for _, c := range cases {
		if q, ok := domain.PrepareQuery(c.q, c.limit, c.servers); ok || q != "" {
			t.Errorf("%s: got %q, %v", c.name, q, ok)
		}
	}
}

func TestQueryFitsTheEmbedderCountsUTF16Units(t *testing.T) {
	if !domain.QueryFitsTheEmbedder(strings.Repeat("z", 2000)) || domain.QueryFitsTheEmbedder(strings.Repeat("z", 2001)) {
		t.Fatal("boundary at 2000 units")
	}
	// 1000 emoji are 2000 units: fits; 1001 do not (although 1001 runes < 2000)
	if !domain.QueryFitsTheEmbedder(strings.Repeat("\U0001F600", 1000)) || domain.QueryFitsTheEmbedder(strings.Repeat("\U0001F600", 1001)) {
		t.Fatal("astral characters count twice")
	}
	if !domain.QueryFitsTheEmbedder("") {
		t.Fatal("empty fits")
	}
}
