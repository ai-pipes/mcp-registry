//go:build compat

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFixed(t *testing.T) {
	if got, err := ParseFixed(""); err != nil || len(got) != 0 {
		t.Fatalf("unset: %v %v", got, err)
	}
	all, err := ParseFixed("all")
	if err != nil || len(all) != len(AllDeviations) {
		t.Fatalf("all: %v %v", all, err)
	}
	for _, d := range AllDeviations {
		if !all[d.ID] {
			t.Errorf("all lacks %s", d.ID)
		}
	}
	got, err := ParseFixed(" create-user-keeps-role , reindex-wording ")
	if err != nil || len(got) != 2 || !got[DevCreateUserKeepsRole] || !got[DevReindexWording] || got[DevOAuthStateKept] {
		t.Fatalf("list: %v %v", got, err)
	}
	for _, bad := range []string{"nope", "reindex-wording,nope", "reindex-wording,", "ALL", "all,reindex-wording", "audit-order", "root-no-cache"} {
		_, err := ParseFixed(bad)
		if err == nil || !strings.Contains(err.Error(), "valid ids") || !strings.Contains(err.Error(), string(DevReindexWording)) || !strings.Contains(err.Error(), FixedEnv) {
			t.Errorf("%q: error %v", bad, err)
		}
	}
}

func TestFixedFollowsTheEnvironment(t *testing.T) {
	t.Setenv(FixedEnv, "oauth-state-kept")
	f, err := fixedFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s := &Stack{fixed: f}
	if !s.Fixed(DevOAuthStateKept) || s.Fixed(DevReindexWording) {
		t.Fatalf("Fixed: %v", f)
	}
	if (&Stack{}).Fixed(DevOAuthStateKept) {
		t.Fatal("a stack without COMPAT_FIXED reports a deviation as fixed")
	}
	t.Setenv(FixedEnv, "")
	if f, err := fixedFromEnv(); err != nil || len(f) != 0 {
		t.Fatalf("COMPAT_FIXED set and empty: %v %v, want none (the reference)", f, err)
	}
	_ = os.Unsetenv(FixedEnv) // t.Setenv above restores it after the test
	if f, err := fixedFromEnv(); err != nil || len(f) != len(AllDeviations) {
		t.Fatalf("COMPAT_FIXED unset: %v %v, want all (the image built from this tree)", f, err)
	}
	t.Setenv(FixedEnv, "bogus")
	if _, err := fixedFromEnv(); err == nil {
		t.Fatal("an unknown id was accepted")
	}
	if _, err := boot(Options{}, false); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("boot with an unknown id: %v", err)
	}
}

func TestEveryDeviationHasADescriptionAndIsUsedByAScenario(t *testing.T) {
	seen := map[Deviation]bool{}
	for _, d := range AllDeviations {
		if seen[d.ID] || d.ID == "" || !strings.Contains(d.Description, "->") {
			t.Errorf("registry entry %+v is duplicated, empty or has no old -> new description", d)
		}
		seen[d.ID] = true
	}
	// The constant of each id is the one that names it; find it by value through the sources.
	files, err := filepath.Glob(filepath.Join("..", "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("scenario files: %v %v", files, err)
	}
	var corpus strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: fixed glob of the suite's own sources
		if err != nil {
			t.Fatal(err)
		}
		corpus.Write(b)
	}
	src, err := os.ReadFile("deviations.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range AllDeviations {
		name := constantNameOf(t, string(src), d.ID)
		// The call, not a mention in a comment or a description.
		if !strings.Contains(corpus.String(), "Fixed(harness."+name+")") {
			t.Errorf("deviation %s is listed but no compat/*_test.go scenario calls Fixed(harness.%s)", d.ID, name)
		}
	}
}

// constantNameOf finds `Name Deviation = "id"` in the source of deviations.go.
func constantNameOf(t *testing.T, src string, id Deviation) string {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		f := strings.Fields(line)
		if len(f) == 4 && f[1] == "Deviation" && f[2] == "=" && f[3] == `"`+string(id)+`"` {
			return f[0]
		}
	}
	t.Fatalf("no constant for deviation %q in deviations.go", id)
	return ""
}
