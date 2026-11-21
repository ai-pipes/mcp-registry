package kernel_test

import (
	"math"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

func TestJSTrimTrimsWhatJavaScriptTrims(t *testing.T) {
	for in, want := range map[string]string{
		"  a b \t\n":          "a b",
		"\u00a0\ufeffx\u2028": "x",
		"\u0085x\u0085":       "\u0085x\u0085", // NEL is not whitespace in JavaScript
		"":                    "",
		"\u200bx":             "\u200bx", // zero width space is not whitespace either
	} {
		if got := kernel.JSTrim(in); got != want {
			t.Errorf("JSTrim(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJSNumberIsNumberOfAString(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"", 0, true},
		{"  ", 0, true},
		{"12", 12, true},
		{" 7 ", 7, true},
		{"1e2", 100, true},
		{"0x10", 16, true},
		{"0b11", 3, true},
		{"0o7", 7, true},
		{"-3", -3, true},
		{".5", 0.5, true},
		{"1.5", 1.5, true},
		{"abc", 0, false},
		{"1_000", 0, false},
		{"-0x10", 0, false},
		{"1 2", 0, false},
	} {
		got, ok := kernel.JSNumber(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("JSNumber(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if f, ok := kernel.JSNumber("1e999"); !ok || !math.IsInf(f, 1) {
		t.Errorf("a huge literal is Infinity, got %v %v", f, ok)
	}
}
