package pgtest

import (
	"os"
	"regexp"
	"testing"
)

func quoted(t *testing.T, source []byte, re *regexp.Regexp) string {
	t.Helper()
	m := re.FindSubmatch(source)
	if m == nil {
		t.Fatal("no image constant found")
	}
	return string(m[1])
}

func TestImageMatchesCompatHarness(t *testing.T) {
	mine, err := os.ReadFile("pgtest.go")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := os.ReadFile("../../../compat/harness/stack.go")
	if err != nil {
		t.Fatal(err)
	}
	a := quoted(t, mine, regexp.MustCompile(`(?m)^const image = "([^"]+)"`))
	b := quoted(t, theirs, regexp.MustCompile(`(?m)^const dbImage = "([^"]+)"`))
	if a != b {
		t.Fatalf("pgtest.image and compat/harness dbImage differ:\n  %s\n  %s", a, b)
	}
}
