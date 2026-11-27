package http_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	cathttp "github.com/ai-pipes/mcp-registry/internal/modules/catalog/http"
)

// formatIssues is how v1.0.1:apps/server/src/platform/cli-commands.ts prints the issues of a server file.
func formatIssues(issues []app.Issue) string {
	var b strings.Builder
	for _, i := range issues {
		p := i.Path
		if p == "" {
			p = "(root)"
		}
		b.WriteString("  " + p + ": " + i.Message + "\n")
	}
	return b.String()
}

func cliGoldenIssues(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "compat", "golden", "cli", name+".txt")) //nolint:gosec // G304: a constant name under the repository's golden files
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(raw), "is not a valid server:\n")
	if !ok {
		t.Fatalf("%s has no issue list", name)
	}
	return after
}

func mustParse(t *testing.T, body any) (app.ServerInput, []app.Issue) {
	t.Helper()
	raw, _ := json.Marshal(body)
	in, issues, err := cathttp.ParseServerFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return in, issues
}

func TestServerFileIssuesMatchTheCLIGoldens(t *testing.T) {
	b := bodyOf("none", set("url", "ftp://nope"), set("slug", "gold-url"))
	_, issues := mustParse(t, b)
	if got, want := formatIssues(issues), cliGoldenIssues(t, "create-server-invalid-url"); got != want {
		t.Fatalf("invalid url:\n got %q\nwant %q", got, want)
	}
	// A file with only a slug: the golden pins the paths in order; the wording is masked there.
	_, issues = mustParse(t, map[string]any{"slug": "gold-empty"})
	mask := regexp.MustCompile(`(?m)^  ([\w.()]+): .*$`)
	got := mask.ReplaceAllString(formatIssues(issues), "  $1: <issue>")
	if want := cliGoldenIssues(t, "create-server-incomplete"); got != want {
		t.Fatalf("incomplete:\n got %q\nwant %q", got, want)
	}
}

func TestServerFileIsParsedLikeJSONParse(t *testing.T) {
	for _, text := range []string{"", "   \n", "{nope", `{} {}`, "null{"} {
		if _, _, err := cathttp.ParseServerFile([]byte(text)); err == nil {
			t.Errorf("%q: no error", text)
		}
	}
	_, _, err := cathttp.ParseServerFile([]byte(`{"name": "top-secret-content",`))
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("error %v quotes the file", err)
	}
	for _, text := range []string{`[]`, `null`, `"x"`, `5`} {
		_, issues, err := cathttp.ParseServerFile([]byte(text))
		if err != nil || len(issues) != 1 || issues[0].Path != "" {
			t.Errorf("%s: %+v %v", text, issues, err)
		}
	}
}

func TestServerFileListsEveryIssueInSchemaOrder(t *testing.T) {
	raw := `{"slug":7,"name":"","description":5,"url":"ftp://x","auth":{"kind":"oauth_user"},"access":{"kind":"groups","groups":["",3]},"timeoutMs":5,"dailyLimit":0}`
	_, issues, err := cathttp.ParseServerFile([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, i := range issues {
		paths = append(paths, i.Path)
	}
	want := []string{"name", "description", "url", "auth.oauth", "auth.header", "auth.template", "access.groups.0", "access.groups.1", "timeoutMs", "dailyLimit", "slug"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths %v\nwant  %v", paths, want)
	}
}

func TestTheRESTDecoderTakesTheFirstIssueOfTheSameMachinery(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{
		`{"slug":7,"name":"","description":5,"url":"ftp://x","auth":{"kind":"oauth_user"},"access":{"kind":"groups","groups":["",3]},"timeoutMs":5}`,
		`{"name":"x","description":"","url":"https://x.example","auth":{"kind":"none"},"access":{"kind":"all"},"slug":""}`,
		`[]`,
	} {
		_, issues, err := cathttp.ParseServerFile([]byte(body))
		if err != nil || len(issues) == 0 {
			t.Fatal(issues, err)
		}
		r := e.admin("POST", "/api/servers", body)
		want := issues[0].Message
		if issues[0].Path != "" {
			want = issues[0].Path + ": " + want
		}
		wantReply(t, r, 422, errBody("invalid_request", want))
	}
}

func TestAValidServerFileIsWhatRESTHandsToTheUseCase(t *testing.T) {
	raw := `{"slug":"gitea","secret":"ignored","extra":1,"name":"Gitea","description":"d","url":" https://x.example/\tm ","auth":{"kind":"oauth_user","header":"Authorization","template":"Bearer {secret}","oauth":{"authorizeUrl":"https://x.example/a","tokenUrl":"https://x.example/t","clientId":"c","scopes":["s"],"extraAuthorizeParams":{"z":"1","a":"2","z":"3"}}},"access":{"kind":"groups","groups":["dev"]}}`
	in, issues, err := cathttp.ParseServerFile([]byte(raw))
	if err != nil || len(issues) != 0 {
		t.Fatal(issues, err)
	}
	timeout := (*int)(nil)
	want := app.ServerInput{Slug: "gitea", Fields: app.ServerFields{
		Name: "Gitea", Description: "d", URL: " https://x.example/\tm ",
		Auth: domain.AuthMode{Kind: domain.AuthOAuthUser, Header: "Authorization", Template: "Bearer {secret}", OAuth: &domain.OAuthClientConfig{ //nolint:gosec // G101: made-up OAuth endpoints of a test server, not credentials
			AuthorizeURL: "https://x.example/a", TokenURL: "https://x.example/t", ClientID: "c", Scopes: []string{"s"},
			ExtraAuthorizeParams: []domain.AuthorizeParam{{Key: "z", Value: "3"}, {Key: "a", Value: "2"}},
		}},
		Access: domain.Access{Kind: domain.AccessGroups, Groups: []string{"dev"}}, TimeoutMs: timeout,
	}}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("got  %+v\nwant %+v", in, want)
	}
}
