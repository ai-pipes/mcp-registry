// Package secrettest checks that a value carrying secrets prints none of them, however it is
// held. It is test support: only _test.go files import it.
package secrettest

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
)

// Verbs are the fmt verbs every holding is printed with.
var Verbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%o", "%b", "%c", "%U"}

// Forbidden is every form in which one of the plain values must not appear in an output: the value
// itself, its hex (lower and upper case), base64 (standard, raw, URL, raw URL), the start of its
// decimal byte list as %v of a []byte writes it and the start of its Go-syntax byte list as %#v
// writes it ("0x5a, 0x5a, ..."). Values shorter than 12 bytes are refused: a
// short value can turn up in an output by chance.
func Forbidden(t testing.TB, plain ...[]byte) []string {
	t.Helper()
	var out []string
	for _, p := range plain {
		if len(p) < 12 {
			t.Fatalf("secrettest: a plain value must have at least 12 bytes, got %d", len(p))
		}
		dec := strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%v", p), "["), "]")
		if len(dec) > 24 {
			dec = dec[:24]
		}
		gob := fmt.Sprintf("%#v", p)
		gob = gob[strings.Index(gob, "{")+1:]
		if len(gob) > 30 {
			gob = gob[:30]
		}
		out = append(out, gob, string(p), hex.EncodeToString(p), strings.ToUpper(hex.EncodeToString(p)),
			base64.StdEncoding.EncodeToString(p), base64.RawStdEncoding.EncodeToString(p),
			base64.URLEncoding.EncodeToString(p), base64.RawURLEncoding.EncodeToString(p), dec)
	}
	return out
}

// Strings is Forbidden for text values.
func Strings(t testing.TB, plain ...string) []string {
	t.Helper()
	bs := make([][]byte, len(plain))
	for i, p := range plain {
		bs[i] = []byte(p)
	}
	return Forbidden(t, bs...)
}

type (
	unexported[T any] struct{ v T }
	pointerIn[T any]  struct{ p *T }
	nested[T any]     struct{ in unexported[T] }
	sliceIn[T any]    struct{ s []T }
	mapIn[T any]      struct{ m map[string]T }
	anyIn             struct{ a any }
	exported[T any]   struct {
		V T
		P *T
		S []T
		M map[string]T
	}
)

// Holdings is v held every way the check covers, by name.
func Holdings[T any](v T) map[string]any {
	u := unexported[T]{v}
	return map[string]any{
		"bare":                 v,
		"pointer":              &v,
		"unexported field":     u,
		"pointer to holder":    &u,
		"pointer in field":     pointerIn[T]{&v},
		"pointer to ptr field": &pointerIn[T]{&v},
		"nested":               nested[T]{u},
		"slice":                []T{v},
		"slice in field":       sliceIn[T]{[]T{v}},
		"map value":            map[string]T{"k": v},
		"map in field":         mapIn[T]{map[string]T{"k": v}},
		"[]any":                []any{v},
		"any field":            anyIn{v},
		"slice of holders":     []unexported[T]{u},
		"map of holders":       map[string]unexported[T]{"k": u},
	}
}

// Leaks lists where v shows a forbidden string: "<holding> <verb>", "<holding> slog text",
// "<holding> slog json" and "json" (encoding/json of an exported holder: the value, a pointer, a
// slice and a map of it). Sorted.
func Leaks[T any](v T, forbidden []string) []string {
	shows := func(out string) bool {
		for _, f := range forbidden {
			if strings.Contains(out, f) {
				return true
			}
		}
		return false
	}
	var leaks []string
	for name, h := range Holdings(v) {
		for _, verb := range Verbs {
			if shows(fmt.Sprintf(verb, h)) {
				leaks = append(leaks, name+" "+verb)
			}
		}
		var tx, js bytes.Buffer
		slog.New(slog.NewTextHandler(&tx, nil)).Info("m", "v", h)
		slog.New(slog.NewJSONHandler(&js, nil)).Info("m", "v", h)
		if shows(tx.String()) {
			leaks = append(leaks, name+" slog text")
		}
		if shows(js.String()) {
			leaks = append(leaks, name+" slog json")
		}
	}
	j, err := json.Marshal(exported[T]{V: v, P: &v, S: []T{v}, M: map[string]T{"k": v}})
	switch {
	case err != nil:
		leaks = append(leaks, "json (cannot encode)")
	case shows(string(j)):
		leaks = append(leaks, "json")
	}
	sort.Strings(leaks)
	return leaks
}

// AssertUnprintable fails the test for every holding, verb, handler or encoding in which v shows
// one of the forbidden strings.
func AssertUnprintable[T any](t testing.TB, name string, v T, forbidden []string) {
	t.Helper()
	if leaks := Leaks(v, forbidden); len(leaks) > 0 {
		t.Errorf("%s shows a secret in %d ways: %s", name, len(leaks), Summary(leaks))
	}
}

// Summary groups the result of Leaks by holding: "bare: %v %s json; unexported field: ...".
func Summary(leaks []string) string {
	if len(leaks) == 0 {
		return "none"
	}
	by := map[string][]string{}
	var order []string
	for _, l := range leaks {
		holding, how := l, ""
		if i := strings.LastIndex(l, " %"); i >= 0 {
			holding, how = l[:i], l[i+1:]
		} else if i := strings.Index(l, " slog "); i >= 0 {
			holding, how = l[:i], l[i+1:]
		}
		if _, seen := by[holding]; !seen {
			order = append(order, holding)
		}
		by[holding] = append(by[holding], how)
	}
	parts := make([]string, 0, len(order))
	for _, h := range order {
		parts = append(parts, strings.TrimSpace(h+": "+strings.Join(by[h], " ")))
	}
	return strings.Join(parts, "; ")
}
