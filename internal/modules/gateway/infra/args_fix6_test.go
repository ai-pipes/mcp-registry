package infra

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func numberList(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprint(i)
	}
	return strings.Join(parts, ",")
}

func nameList(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"name%d"`, i)
	}
	return strings.Join(parts, ",")
}

// A long enum weighs its values: with many argument values the work is over the budget and the
// arguments go on unchecked; with a handful they are still validated.
func TestALongEnumCountsTowardsTheBudget(t *testing.T) {
	v, w := newValidator(t)
	s := `{"type":"object","properties":{"a":{"type":"array","items":{"enum":[` + numberList(10_000) + `]}}}}`
	many := `{"a":[` + numberList(9_999) + `,-1]}`
	if got := check(v, s, many); !got.OK {
		t.Fatalf("10 000 values: %+v, want not validated", got)
	}
	// A 10 000-entry enum is over the budget even for one value; a 1 000-entry one is checked for
	// a handful.
	small := `{"type":"object","properties":{"a":{"type":"array","items":{"enum":[` + numberList(1_000) + `]}}}}`
	wantRefused(t, check(v, small, `{"a":[1,2,-1]}`), "/a/2")
	if n := len(w.all()); n != 0 {
		t.Fatalf("warnings %v", w.all())
	}
}

// A long required list weighs its names (and the library builds the list of missing names for
// every object).
func TestALongRequiredListCountsTowardsTheBudget(t *testing.T) {
	v, w := newValidator(t)
	s := `{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","required":[` + nameList(20_000) + `]}}}}`
	many := `{"a":[` + strings.Repeat("{},", 7_999) + `{}]}`
	if got := check(v, s, many); !got.OK {
		t.Fatalf("8 000 objects: %+v, want not validated", got)
	}
	got := check(v, s, `{"a":[{},{}]}`)
	wantRefused(t, got, "/a/0")
	if n := utf8.RuneCountInString(got.Message); n > maxRefusalMessage {
		t.Fatalf("the refusal message has %d characters, want at most %d", n, maxRefusalMessage)
	}
	if n := len(w.all()); n != 0 {
		t.Fatalf("warnings %v", w.all())
	}
}

// dependentRequired and the string-list form of dependencies weigh their names too.
func TestDependentNameListsCountTowardsTheBudget(t *testing.T) {
	for name, s := range map[string]string{
		"dependentRequired": `{"type":"object","properties":{"a":{"type":"array","items":{"dependentRequired":{"k":[` + nameList(20_000) + `]}}}}}`,
		"dependencies":      `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"a":{"type":"array","items":{"dependencies":{"k":[` + nameList(20_000) + `]}}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := newValidator(t)
			if got := check(v, s, `{"a":[`+strings.Repeat(`{"k":1},`, 7_999)+`{"k":1}]}`); !got.OK {
				t.Fatalf("8 000 objects: %+v, want not validated", got)
			}
			wantRefused(t, check(v, s, `{"a":[{"k":1}]}`), "/a/0")
		})
	}
}

// A const or an enum entry that is itself a large value weighs its JSON values, not 1.
func TestAStructuredConstOrEnumEntryWeighsItsValues(t *testing.T) {
	big := "[" + numberList(30_000) + "]"
	for _, s := range []string{`{"const":` + big + `}`, `{"enum":[` + big + `]}`} {
		sch, err := compile(json.RawMessage(s))
		if err != nil {
			t.Fatal(err)
		}
		if _, work, err := expandedSize(sch); err != nil || work < 30_000 {
			t.Fatalf("%.20s: work %d, err %v", s, work, err)
		}
	}
}

// Every recorded tool schema is enforced and leaves room for at least 500 argument values.
func TestEveryRecordedToolSchemaIsEnforcedWithRoomForArguments(t *testing.T) {
	fixtures := os.DirFS(filepath.Join("..", "..", "..", "..", "apps", "server", "test", "fixtures", "tools"))
	for _, name := range []string{"gitea", "youtrack"} {
		raw, err := fs.ReadFile(fixtures, name+".json")
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Tools) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		v, w := newValidator(t)
		for _, tool := range doc.Tools {
			e := v.entryFor(name+"."+tool.Name, tool.InputSchema)
			if e.sch == nil || e.work*500 > maxValidationWork {
				t.Errorf("%s.%s: enforced %v, work %d", name, tool.Name, e.sch != nil, e.work)
			}
		}
		if len(w.all()) != 0 {
			t.Fatalf("%s: warnings %v", name, w.all())
		}
	}
}

func TestARefusalMessageIsCappedAtARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", maxRefusalMessage+10)
	got := cutRunes(long, maxRefusalMessage)
	if utf8.RuneCountInString(got) != maxRefusalMessage || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("%d runes, valid %v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if cutRunes("short", maxRefusalMessage) != "short" {
		t.Fatal("a short message changed")
	}
}
