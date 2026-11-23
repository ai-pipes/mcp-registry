package infra

import (
	"encoding/json"
	"os"
	"testing"
)

type encRow struct {
	In  string `json:"in"`
	Out string `json:"out"`
}

// testdata/formencode.json was produced with node: new URLSearchParams({v: s}).toString().slice(2),
// the function the reference encodes the Basic credentials with.
func TestFormEncodeMatchesWHATWG(t *testing.T) {
	raw, err := os.ReadFile("testdata/formencode.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []encRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 100 {
		t.Fatalf("only %d rows", len(rows))
	}
	for _, r := range rows {
		if got := formEncode(r.In); got != r.Out {
			t.Errorf("formEncode(%q) = %q, want %q", r.In, got, r.Out)
		}
	}
}

func TestFormEncodeDiffersFromQueryEscapeWhereTheReferenceDoes(t *testing.T) {
	if got := formEncode("*"); got != "*" {
		t.Errorf("* = %q, want it unescaped", got)
	}
	if got := formEncode("~"); got != "%7E" {
		t.Errorf("~ = %q, want %%7E", got)
	}
	if got := formEncode("a b+c"); got != "a+b%2Bc" {
		t.Errorf("got %q", got)
	}
}

func TestFormEncodeReplacesInvalidUTF8LikeUSVString(t *testing.T) {
	if got := formEncode("a\xffb"); got != "a%EF%BF%BDb" {
		t.Errorf("got %q", got)
	}
}

func TestFormDecodeRoundTripsAndFollowsWHATWG(t *testing.T) {
	for _, s := range []string{"a b+c", "~*é日😀", "100%", ""} {
		if got := formDecode(formEncode(s)); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
	}
	if got := formDecode("a+b%20c%zz%4"); got != "a b c%zz%4" {
		t.Errorf("got %q", got)
	}
}

func TestBasicHeaderTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/basic.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
		Header string `json:"header"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 10 {
		t.Fatal("too few rows")
	}
	for _, r := range rows {
		if got := basicHeader(r.ID, r.Secret); got != r.Header {
			t.Errorf("basicHeader(%q, %q) = %q, want %q", r.ID, r.Secret, got, r.Header)
		}
	}
}
