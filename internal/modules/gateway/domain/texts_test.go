package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

func TestFixedTexts(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"notFound", domain.TextNotFound("gitea.x"), `Tool "gitea.x" is not available. Use search_tools to find another.`},
		{
			"invalid", domain.TextInvalidArguments("gitea.create_issue", "/title", "must be string"),
			`Invalid arguments at /title: must be string. Call describe_tool("gitea.create_issue") for the schema.`,
		},
		{
			"needsConnection", domain.TextNeedsConnection("Gitea", "https://mcp.example.com"),
			"Gitea is not connected. Ask the user to open https://mcp.example.com/connections and connect Gitea.",
		},
		{
			"expired", domain.TextExpired("Gitea", "https://mcp.example.com"),
			"Gitea connection expired. Ask the user to reconnect at https://mcp.example.com/connections.",
		},
		{
			"rejected", domain.TextRejected("Gitea"),
			"Gitea rejected the gateway credentials. Ask the administrator to check the server key.",
		},
		{
			"quota", domain.TextQuota("Gitea", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)),
			"Daily limit for Gitea reached. Resets at 2026-10-31T00:00:00.000Z.",
		},
		{"transport", domain.TextTransport("Gitea"), "Gitea is unavailable."},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}

func TestQuotaTextUsesUTC(t *testing.T) {
	zone := time.FixedZone("x", 2*3600)
	got := domain.TextQuota("G", time.Date(2026, 10, 31, 2, 0, 0, 0, zone))
	if got != "Daily limit for G reached. Resets at 2026-10-31T00:00:00.000Z." {
		t.Fatal(got)
	}
}

func TestTimeoutAndSizeNumbers(t *testing.T) {
	timeouts := map[int]string{
		30000: "30 s", 1500: "1.5 s", 1000: "1 s", 1125: "1.13 s", 125: "0.13 s", 1005: "1 s",
		1234: "1.23 s", 1235: "1.24 s", 100: "0.1 s", 10: "0.01 s", 4: "0 s", 5: "0.01 s", 600000: "600 s",
		2500: "2.5 s", 999: "1 s", 61000: "61 s",
	}
	for ms, want := range timeouts {
		if got := domain.TextTimeout("Gitea", ms); got != "Gitea did not respond in "+want+"." {
			t.Errorf("%d ms: %q", ms, got)
		}
	}
	sizes := map[int64]string{
		20 * 1024 * 1024: "20 MiB", 65536: "0.06 MiB", 131072: "0.13 MiB", 1572864: "1.5 MiB",
		1024 * 1024: "1 MiB", 1: "0 MiB", 5242: "0 MiB", 5300: "0.01 MiB", 10485760: "10 MiB",
	}
	for n, want := range sizes {
		if got := domain.TextTooLarge("Gitea", n); got != "Gitea returned a response larger than "+want+"." {
			t.Errorf("%d B: %q", n, got)
		}
	}
}

func TestUpstreamRejectedText(t *testing.T) {
	rej := func(code int, msg string) string {
		return domain.TextUpstreamRejected("Gitea", domain.Rejection{Code: code, Message: msg})
	}
	if got := rej(-32602, "Invalid params: owner is required"); got != "Gitea rejected the call (JSON-RPC -32602): Invalid params: owner is required" {
		t.Fatal(got)
	}
	for _, empty := range []string{"", "  \n ", "\u00a0\ufeff\u2028"} {
		if got := rej(-32000, empty); got != "Gitea rejected the call (JSON-RPC -32000)." {
			t.Errorf("%q: %q", empty, got)
		}
	}
	if got := rej(1, "  padded  "); got != "Gitea rejected the call (JSON-RPC 1): padded" {
		t.Fatal(got)
	}
	// U+0085 is not JS whitespace: kept
	if got := rej(1, "\u0085x"); got != "Gitea rejected the call (JSON-RPC 1): \u0085x" {
		t.Fatal(got)
	}
	head := "Gitea rejected the call (JSON-RPC 1): "
	l500 := strings.Repeat("a", 500)
	if got := rej(1, l500); got != head+l500 {
		t.Fatal("500 units must be kept whole")
	}
	l501 := strings.Repeat("a", 501)
	if got := rej(1, l501); got != head+strings.Repeat("a", 499)+"…" {
		t.Fatal("501 units are cut to 499 + …")
	}
	if got := rej(1, strings.Repeat("b", 5000)); got != head+strings.Repeat("b", 499)+"…" {
		t.Fatal("long message")
	}
}

func TestUpstreamRejectedCutCountsUTF16AndDropsASplitPair(t *testing.T) {
	head := "Gitea rejected the call (JSON-RPC 1): "
	// 498 units + one astral rune (2 units) = 500 units: kept whole
	whole := strings.Repeat("a", 498) + "😀"
	if got := domain.TextUpstreamRejected("Gitea", domain.Rejection{Code: 1, Message: whole}); got != head+whole {
		t.Fatal("500 units with a pair must be kept")
	}
	// 499 units + astral rune = 501 units: cut at 499, the rune does not fit
	over := strings.Repeat("a", 499) + "😀"
	if got := domain.TextUpstreamRejected("Gitea", domain.Rejection{Code: 1, Message: over}); got != head+strings.Repeat("a", 499)+"…" {
		t.Fatal("cut before the pair")
	}
	// 498 units + astral + more: the pair ends at 500, 499 would split it: the half is dropped
	split := strings.Repeat("a", 498) + "😀" + "b"
	if got := domain.TextUpstreamRejected("Gitea", domain.Rejection{Code: 1, Message: split}); got != head+strings.Repeat("a", 498)+"…" {
		t.Fatalf("a surrogate half must be dropped, got len %d", len(got))
	}
	// BMP letters outside ASCII are one unit each
	if got := domain.TextUpstreamRejected("G", domain.Rejection{Code: 1, Message: strings.Repeat("я", 500)}); !strings.HasSuffix(got, strings.Repeat("я", 500)) {
		t.Fatal("500 BMP units kept")
	}
}

func TestBoundName(t *testing.T) {
	if got := domain.BoundName(strings.Repeat("x", 201)); len(got) != 200 {
		t.Fatal(len(got))
	}
	if got := domain.BoundName("gitea"); got != "gitea" {
		t.Fatal(got)
	}
	if got := domain.BoundName(""); got != "" {
		t.Fatal(got)
	}
	// no ellipsis; a pair split at the boundary loses its half
	s := strings.Repeat("a", 199) + "😀"
	if got := domain.BoundName(s); got != strings.Repeat("a", 199) {
		t.Fatalf("got %d bytes", len(got))
	}
	s = strings.Repeat("a", 198) + "😀"
	if got := domain.BoundName(s); got != s {
		t.Fatal("200 units kept")
	}
	if domain.MaxLoggedName != 200 || domain.MaxRejectionText != 500 {
		t.Fatal("limits")
	}
}
