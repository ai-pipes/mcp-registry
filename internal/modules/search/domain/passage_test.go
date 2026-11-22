package domain_test

import (
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

func base() domain.PassageSource {
	return domain.PassageSource{
		ServerName:        "YouTrack",
		ServerDescription: "Issue tracker",
		ToolName:          "create_issue",
		Title:             "Create issue",
		Description:       "Creates a new issue.",
	}
}

func TestBuildPassageJoinsServerToolNameWithTitleAndDescription(t *testing.T) {
	got := domain.BuildPassage(base())
	want := "YouTrack. Issue tracker\ncreate_issue Create issue\nCreates a new issue."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildPassageOmitsAnAbsentTitleWithoutAStraySpace(t *testing.T) {
	s := base()
	s.Title = ""
	want := "YouTrack. Issue tracker\ncreate_issue\nCreates a new issue."
	if got := domain.BuildPassage(s); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildPassageKeepsAnEmptyServerDescriptionAndTrimsNothing(t *testing.T) {
	s := base()
	s.ServerDescription = ""
	s.Description = "  padded \n"
	want := "YouTrack. \ncreate_issue Create issue\n  padded \n"
	if got := domain.BuildPassage(s); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildPassageTruncatesToMaxPassageChars(t *testing.T) {
	s := base()
	s.Description = strings.Repeat("x", 5000)
	if got := domain.UTF16Len(domain.BuildPassage(s)); got != domain.MaxPassageChars {
		t.Fatalf("length %d, want %d", got, domain.MaxPassageChars)
	}
}

// The prefix "S. D\nn\n" is 7 units; the emoji then occupies units 1999 and 2000 (0-based), so the
// cut at 2000 units keeps only its high surrogate.
func splitSource(tail string) domain.PassageSource {
	return domain.PassageSource{
		ServerName: "S", ServerDescription: "D", ToolName: "n",
		Description: strings.Repeat("x", 1992) + tail,
	}
}

func TestBuildPassageCountsAstralCharactersAsTwoUnits(t *testing.T) {
	// 1992 + one emoji (2 units) + 7 = 2001 units: the emoji does not fit as a whole.
	got := domain.BuildPassage(splitSource("\U0001F600 tail"))
	// 1999 units kept plus the stand-in, which replaces one lone surrogate (it is itself two units)
	if n := domain.UTF16Len(strings.TrimSuffix(got, domain.SplitPairStandIn)); n != domain.MaxPassageChars-1 {
		t.Fatalf("length %d, want %d", n, domain.MaxPassageChars-1)
	}
	// A pair that fits is kept whole: 1991 x's leave room for the emoji.
	s := splitSource("\U0001F600 tail")
	s.Description = strings.Repeat("x", 1991) + "\U0001F600 tail"
	if got := domain.BuildPassage(s); !strings.Contains(got, "\U0001F600") || strings.Contains(got, domain.SplitPairStandIn) {
		t.Fatalf("a pair that fits must stay whole: %q", got[len(got)-12:])
	}
}

func TestBuildPassageCutInsideASurrogatePairHandsTheEmbedderTheStandIn(t *testing.T) {
	// JavaScript keeps the lone high surrogate; the tokenizer turns it into <unk>, exactly as it
	// does the stand-in U+F0000 (measured with transformers.js). Go strings cannot hold the lone half.
	got := domain.BuildPassage(splitSource("\U0001F600 tail"))
	if !strings.HasSuffix(got, "x"+domain.SplitPairStandIn) {
		t.Fatalf("passage ends %q, want the stand-in last", got[len(got)-6:])
	}
	if strings.Contains(got, "\uFFFD") {
		t.Fatal("the embedder text must not contain U+FFFD (the normaliser maps it to a space)")
	}
	if domain.SplitPairStandIn != "\U000F0000" {
		t.Fatalf("stand-in is %U", []rune(domain.SplitPairStandIn)[0])
	}
}

func TestBuildPassageExactFitAndCutBeforeAnAstralCharacter(t *testing.T) {
	// exactly 2000 units, astral char ends at the limit: nothing cut
	s := splitSource("")
	s.Description = strings.Repeat("x", 1991) + "\U0001F600"
	got := domain.BuildPassage(s)
	if !strings.HasSuffix(got, "\U0001F600") || domain.UTF16Len(got) != 2000 {
		t.Fatalf("exact fit changed: len %d", domain.UTF16Len(got))
	}
	// the astral char starts exactly at the limit: dropped whole, no stand-in
	s.Description = strings.Repeat("x", 1993) + "\U0001F600"
	got = domain.BuildPassage(s)
	if strings.Contains(got, domain.SplitPairStandIn) || strings.Contains(got, "\U0001F600") || domain.UTF16Len(got) != 2000 {
		t.Fatalf("cut before the pair: len %d", domain.UTF16Len(got))
	}
}

func TestBMPCharactersCountAsOneUnit(t *testing.T) {
	s := base()
	s.Description = strings.Repeat("ж", 3000) // 2 bytes each, 1 unit each
	got := domain.BuildPassage(s)
	if domain.UTF16Len(got) != domain.MaxPassageChars {
		t.Fatalf("length %d", domain.UTF16Len(got))
	}
	if strings.Contains(got, "\uFFFD") || strings.Contains(got, domain.SplitPairStandIn) {
		t.Fatal("a BMP cut must be clean")
	}
}

func TestUTF16Len(t *testing.T) {
	for in, want := range map[string]int{"": 0, "abc": 3, "ж": 1, "€": 1, "\U0001F600": 2, "a\U0001F600b": 4, "\U000F0000": 2} {
		if got := domain.UTF16Len(in); got != want {
			t.Errorf("UTF16Len(%q) = %d, want %d", in, got, want)
		}
	}
}

func schema(t *testing.T, raw string) string {
	t.Helper()
	got, err := domain.SchemaText([]byte(raw))
	if err != nil {
		t.Fatalf("SchemaText(%s): %v", raw, err)
	}
	return got
}

func TestSchemaTextListsParameterNamesAndDescriptionsNestedOnesIncluded(t *testing.T) {
	raw := `{"type":"object","properties":{
		"title":{"type":"string","description":"Issue title"},
		"labels":{"type":"array","items":{"type":"object","properties":{"color":{}}}},
		"owner":{"type":"object","properties":{"login":{"description":"User login"}}}}}`
	if got, want := schema(t, raw), "title Issue title labels color owner login User login"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSchemaTextIsEmptyForAnythingWithoutUsableProperties(t *testing.T) {
	for _, raw := range []string{`{"type":"object"}`, `{"properties":"nope"}`, `{"properties":[1,2]}`, `{}`, `[]`, `"s"`, `5`, `null`, `true`} {
		if got := schema(t, raw); got != "" {
			t.Errorf("SchemaText(%s) = %q, want empty", raw, got)
		}
	}
	if got := schema(t, `{"properties":{"a":null,"b":5}}`); got != "a b" {
		t.Errorf("got %q, want %q", got, "a b")
	}
}

func TestSchemaTextSkipsTheRootDescriptionButNotTheRootItems(t *testing.T) {
	if got := schema(t, `{"description":"root","properties":{"a":{"description":"A"}}}`); got != "a A" {
		t.Errorf("root description leaked: %q", got)
	}
	if got := schema(t, `{"items":{"description":"item","properties":{"x":{}}}}`); got != "item x" {
		t.Errorf("root items: %q", got)
	}
	// tuple-form items (an array) are not a schema object: skipped
	if got := schema(t, `{"items":[{"description":"nope"}]}`); got != "" {
		t.Errorf("tuple items: %q", got)
	}
}

func TestSchemaTextReadsOnlyDescriptionPropertiesAndItems(t *testing.T) {
	raw := `{"properties":{"a":{"title":"T","enum":["e"],"anyOf":[{"description":"d"}],"$defs":{"x":{"description":"y"}},"type":"string","description":"only this"}}}`
	if got := schema(t, raw); got != "a only this" {
		t.Errorf("got %q", got)
	}
	// non-string description is ignored; an empty string still takes a slot
	if got := schema(t, `{"properties":{"a":{"description":5},"b":{"description":""},"c":{}}}`); got != "a b  c" {
		t.Errorf("got %q", got)
	}
}

func TestSchemaTextOrderWithinASchemaIsDescriptionPropertiesItems(t *testing.T) {
	raw := `{"properties":{"a":{"items":{"description":"I"},"properties":{"p":{}},"description":"D"}}}`
	if got := schema(t, raw); got != "a D p I" {
		t.Errorf("got %q", got)
	}
}

func TestSchemaTextKeepsDocumentOrderOfPlainKeys(t *testing.T) {
	if got := schema(t, `{"properties":{"query":{},"assignee":{},"id":{}}}`); got != "query assignee id" {
		t.Errorf("got %q", got)
	}
}

func TestSchemaTextPutsIntegerKeysFirstInAscendingOrder(t *testing.T) {
	if got := schema(t, `{"properties":{"b":{},"10":{},"a":{},"2":{}}}`); got != "2 10 b a" {
		t.Errorf("got %q, want %q", got, "2 10 b a")
	}
	if got := schema(t, `{"properties":{"z":{},"0":{},"1":{},"y":{}}}`); got != "0 1 z y" {
		t.Errorf("got %q", got)
	}
}

func TestSchemaTextNonCanonicalIntegersAreOrdinaryKeys(t *testing.T) {
	// "01", "-1", "1.5", "1e2", "+1" and 2^32-1 and above are not array indices (node-verified);
	// 2^32-2 is.
	raw := `{"properties":{"b":{},"4294967295":{},"4294967294":{},"a":{},"01":{},"-1":{},"1.5":{},"4294967296":{},"9007199254740993":{},"0":{},"1e2":{},"+1":{}}}`
	want := "0 4294967294 b 4294967295 a 01 -1 1.5 4294967296 9007199254740993 1e2 +1"
	if got := schema(t, raw); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestSchemaTextAppliesTheIntegerRuleAtEveryLevel(t *testing.T) {
	raw := `{"properties":{"b":{"properties":{"y":{},"3":{},"x":{},"1":{}}},"2":{}}}`
	if got := schema(t, raw); got != "2 b 1 3 y x" {
		t.Errorf("got %q", got)
	}
}

func TestSchemaTextDuplicateKeysKeepTheFirstPositionAndTheLastValue(t *testing.T) {
	// JSON.parse semantics (node-verified): {"a":1,"b":2,"a":3} -> keys a,b with a = 3.
	raw := `{"properties":{"a":{"description":"first"},"b":{},"a":{"description":"last"}}}`
	if got := schema(t, raw); got != "a last b" {
		t.Errorf("got %q", got)
	}
}

func TestSchemaTextDecodesEscapesAndKeepsUnicode(t *testing.T) {
	raw := `{"properties":{"na\u006de":{"description":"line\nbreak \u00e9 \ud83d\ude00"}}}`
	if got := schema(t, raw); got != "name line\nbreak é \U0001F600" {
		t.Errorf("got %q", got)
	}
}

func TestSchemaTextIsCappedAndAStraddledPairBecomesTheReplacementCharacter(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"param_` + itoa(i) + `":{"description":"` + strings.Repeat("d", 20) + `"}`)
	}
	b.WriteString(`}}`)
	if n := domain.UTF16Len(schema(t, b.String())); n != domain.MaxPassageChars {
		t.Errorf("length %d, want %d", n, domain.MaxPassageChars)
	}
	// Node writes a lone surrogate to Postgres as U+FFFD, so the stored body ends with it.
	raw := `{"properties":{"a":{"description":"` + strings.Repeat("x", 1997) + `\ud83d\ude00 tail"}}}`
	got := schema(t, raw) // "a " is 2 units, then 1997 x: the emoji straddles unit 2000
	if !strings.HasSuffix(got, "x\uFFFD") || domain.UTF16Len(got) != domain.MaxPassageChars {
		t.Errorf("straddled pair: ends %q, len %d", got[len(got)-5:], domain.UTF16Len(got))
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for ; i > 0; i /= 10 {
		out = append([]byte{digits[i%10]}, out...)
	}
	return string(out)
}

func TestSchemaTextRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{``, `{`, `{"properties":{"a":}}`, `{"a":1} x`, `{"a":1}{}`, `nope`, `{"properties":{"a":{"description":"x"}}`} {
		if _, err := domain.SchemaText([]byte(raw)); err == nil {
			t.Errorf("SchemaText(%q): want an error", raw)
		}
	}
}
