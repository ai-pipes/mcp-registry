package domain_test

import (
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func TestParseSlug(t *testing.T) {
	for _, ok := range []string{"gitea", "my-server-2", strings.Repeat("a", 32), "ab", "a-", "a--b"} {
		if got, err := domain.ParseSlug(ok); err != nil || got != ok {
			t.Errorf("%q: %q %v", ok, got, err)
		}
	}
	for _, bad := range []string{"Gitea", "g", "", "1abc", "-ab", "a b", "a.b", "a_b", strings.Repeat("a", 33), "gitea\n", "é1"} {
		_, err := domain.ParseSlug(bad)
		wantMsg(t, err, "invalid_slug", "server slug must be 2-32 characters: lowercase letters, digits and dashes, starting with a letter")
	}
}

func TestParseToolRef(t *testing.T) {
	cases := []struct{ raw, server, tool string }{
		{"gitea.issues.create", "gitea", "issues.create"},
		{"youtrack.get-file_contents", "youtrack", "get-file_contents"},
	}
	for _, c := range cases {
		s, tool, err := domain.ParseToolRef(c.raw)
		if err != nil || s != c.server || tool != c.tool {
			t.Errorf("%q: %q %q %v", c.raw, s, tool, err)
		}
	}
	for _, bad := range []string{"gitea", ".x", "gitea.", "", "Gitea.x", "a.x"} {
		_, _, err := domain.ParseToolRef(bad)
		wantMsg(t, err, "invalid_tool_ref", "tool name must look like <server>.<tool>")
	}
}

func TestFormatToolRef(t *testing.T) {
	if got := domain.FormatToolRef("gitea", "issues.create"); got != "gitea.issues.create" {
		t.Fatal(got)
	}
	s, tool, err := domain.ParseToolRef(domain.FormatToolRef("a1", "x.y.z"))
	if err != nil || s != "a1" || tool != "x.y.z" {
		t.Fatal(s, tool, err)
	}
}
