package onnx

import (
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// TestJSNormalizeMatchesTheReference compares the port with Precompiled.normalize of
// @huggingface/tokenizers 0.2.0 (Node 22, Unicode 17) on the curated texts, the targeted Unicode
// cases and a seeded fuzz sample: control characters dropped, space-like characters mapped to a
// space, NFKC around U+FF5E. The expected strings are the reference's own output.
func TestJSNormalizeMatchesTheReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.Cases) < 1000 {
		t.Fatalf("the reference file holds %d cases, want the curated, targeted and fuzz sets", len(ref.Cases))
	}
	seen := map[string]int{}
	var failed int
	for i, c := range ref.Cases {
		seen[c.Group]++
		if got := JSNormalize(c.Text); got != c.Normalized {
			failed++
			if failed <= 5 {
				t.Errorf("case %d (%s): JSNormalize(%q) = %q, reference %q", i, c.Group, c.Text, got, c.Normalized)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d cases differ from the reference (Unicode %s)", failed, len(ref.Cases), ref.Unicode)
	}
	for _, g := range []string{"curated", "extra", "fuzz"} {
		if seen[g] == 0 {
			t.Errorf("group %s is missing from the reference file", g)
		}
	}
}

// r builds a string from code points, so no invisible character sits in this source file.
func r(cps ...rune) string { return string(cps) }

func TestJSNormalizeRules(t *testing.T) {
	spaceLike := []rune{0x09, 0x0a, 0x0c, 0x0d, 0xa0, 0x1680, 0x2000, 0x2007, 0x200f, 0x2028, 0x2029, 0x202f, 0x205f, 0x2581, 0x3000, 0xfeff, 0xfffd}
	cases := []struct{ name, in, want string }{
		{"control characters are dropped", "a\x01b\x08c\x0bd\x0ee\x1ff\x7fg" + r(0x8f) + "h" + r(0x9f) + "i", "abcdefghi"},
		{"NUL is kept (replaced later)", "a\x00b", "a\x00b"},
		{"NEL and soft hyphen are in neither set", "a" + r(0x85) + "b" + r(0xad) + "c", "a" + r(0x85) + "b" + r(0xad) + "c"},
		{"NFKC folds compatibility characters", r(0xff46, 0xff55, 0xff4c, 0xff4c, 0x2460, 0xfb01), "full1fi"},
		{"NFKC composes", "e" + r(0x301), r(0xe9)},
		{"the fullwidth tilde is kept and splits the NFKC runs", "a" + r(0xff5e) + "b", "a" + r(0xff5e) + "b"},
		{"invalid UTF-8 reads as U+FFFD, which is a space", "a\xffb", "a b"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := JSNormalize(c.in); got != c.want {
			t.Errorf("%s: JSNormalize(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	for _, cp := range spaceLike {
		if got := JSNormalize("a" + string(cp) + "b"); got != "a b" {
			t.Errorf("U+%04X: JSNormalize = %q, want a space between", cp, got)
		}
	}
}

func TestPrepareTextReplacesNULByAPrivateUseCharacter(t *testing.T) {
	if got, want := PrepareText("a\x00b\x00"), "a\U000F0000b\U000F0000"; got != want {
		t.Fatalf("PrepareText = %q, want %q", got, want)
	}
	if strings.ContainsRune(PrepareText("x\x00\u0000y"), 0) {
		t.Fatal("a NUL reached the tokenizer's C string")
	}
	if got := PrepareText("plain ascii"); got != "plain ascii" {
		t.Fatalf("PrepareText changed plain text: %q", got)
	}
}

// TestNormalisationTablesAreTheReferencesUnicodeVersion pins the Unicode version of golang.org/x/text
// (NFKC) to the one of the Node that made testdata/reference.json: a newer or older table could
// change the normalised text, and so the token ids and vectors, without any test noticing.
func TestNormalisationTablesAreTheReferencesUnicodeVersion(t *testing.T) {
	ref := loadReference(t)
	got := strings.TrimSuffix(norm.Version, ".0")
	if got != ref.Unicode {
		t.Fatalf("golang.org/x/text normalises with Unicode %s, the reference (Node %s) with Unicode %s. "+
			"If x/text was upgraded, regenerate testdata/reference.json and pooling.json with the scripts under "+
			"testdata/gen/ (see testdata/README.md) on a Node of Unicode %s, and review any difference in the normaliser pairs; "+
			"if the reference changed, pin x/text to a release with matching tables", norm.Version, ref.Node, ref.Unicode, got)
	}
}
