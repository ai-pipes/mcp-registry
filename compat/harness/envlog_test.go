//go:build compat

package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseAppEnv(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"", nil, false},
		{" , ", nil, false},
		{"BREAK=csp", map[string]string{"BREAK": "csp"}, false},
		{"A=1, B=x=y ,C=", map[string]string{"A": "1", "B": "x=y", "C": ""}, false},
		{"NOEQUALS", nil, true},
		{"=value", nil, true},
		{"PUBLIC_URL=http://elsewhere", nil, true},
		{"BREAK=csp,DATABASE_URL=x", nil, true},
	} {
		got, err := parseAppEnv(c.in)
		if (err != nil) != c.wantErr || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseAppEnv(%q) = %v, %v; want %v, error %v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestAppEnvReadsTheEnvironment(t *testing.T) {
	t.Setenv("COMPAT_APP_ENV", "BREAK=origin")
	got, err := appEnv()
	if err != nil || got["BREAK"] != "origin" {
		t.Fatalf("appEnv() = %v, %v", got, err)
	}
	t.Setenv("COMPAT_APP_ENV", "broken")
	if _, err := appEnv(); err == nil {
		t.Fatal("a pair without = was accepted")
	}
}

func TestLogFileName(t *testing.T) {
	for in, want := range map[string]string{
		"TestPlatformCSRF":              "TestPlatformCSRF-ab12.log",
		"TestCatalog/a taken slug: 409": "TestCatalog_a_taken_slug_409-ab12.log",
		"":                              "stack-ab12.log",
		"///":                           "stack-ab12.log",
	} {
		if got := logFileName(in, "ab12"); got != want {
			t.Errorf("logFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteLogScrubsAndCreatesTheDirectory(t *testing.T) {
	s := &Stack{id: "ff00", env: map[string]string{"SECRETS_KEY": "topsecretvalue", "PUBLIC_URL": "http://localhost:1"}}
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	path, err := s.writeLog(dir, "TestX/sub", "line one\nkey=topsecretvalue at http://localhost:1\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "TestX_sub-ff00.log"); path != want {
		t.Fatalf("path %q, want %q", path, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); strings.Contains(got, "topsecretvalue") || !strings.Contains(got, "key=<redacted>") || !strings.Contains(got, "http://localhost:1") {
		t.Fatalf("log not scrubbed as expected: %q", got)
	}
}

func TestSaveLogDoesNothingWithoutADirectory(t *testing.T) {
	t.Setenv("COMPAT_LOG_DIR", "")
	(&Stack{id: "00", app: "does-not-exist"}).saveLog("TestY") // must not touch docker or the disk
}

func TestScrubRemovesCredentialShapes(t *testing.T) {
	s := &Stack{env: map[string]string{}}
	link := "ll_" + strings.Repeat("a1_-", 11)
	token := "sk_" + strings.Repeat("Zz9", 15)
	got := s.scrub("link " + link + " and token " + token + " but sk_short and ll_x stay, as does task_" + strings.Repeat("b", 30))
	if strings.Contains(got, link) || strings.Contains(got, token) || strings.Count(got, "<redacted>") != 2 {
		t.Fatalf("scrub: %q", got)
	}
	if !strings.Contains(got, "sk_short") || !strings.Contains(got, "task_"+strings.Repeat("b", 30)) {
		t.Fatalf("scrub removed too much: %q", got)
	}
}

func TestFinishSavesTheLogOnlyForAFailedTest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COMPAT_LOG_DIR", dir)
	newStack := func() *Stack {
		return &Stack{
			id: "ee11", app: "no-such-container", env: map[string]string{"SECRETS_KEY": "kkkkkkkk"},
			fetchLog: func(string) (string, error) {
				return "boot ok\nkey kkkkkkkk token sk_" + strings.Repeat("Q", 43) + "\n", nil
			},
		}
	}
	newStack().finish("TestPassed/sub", false)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a passing test wrote %d log files", len(entries))
	}
	newStack().finish("TestFailed/sub", true)
	raw, err := os.ReadFile(filepath.Join(dir, "TestFailed_sub-ee11.log"))
	if err != nil {
		t.Fatalf("a failed test wrote no log: %v", err)
	}
	if got := string(raw); strings.Contains(got, "kkkkkkkk") || strings.Contains(got, "sk_QQ") || !strings.Contains(got, "boot ok") {
		t.Fatalf("log: %q", got)
	}
	failing := newStack()
	failing.fetchLog = func(string) (string, error) { return "", os.ErrNotExist }
	failing.finish("TestNoContainer", true)
	if _, err := os.Stat(filepath.Join(dir, "TestNoContainer-ee11.log")); err == nil {
		t.Fatal("a log was written although the app container has no log")
	}
}
