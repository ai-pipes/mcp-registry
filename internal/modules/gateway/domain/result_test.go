package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

func TestProjectKeepsOnlyTheThreeFields(t *testing.T) {
	raw := `{"_meta":{"a":1},"resultType":"complete","extra":true,
	  "content":[{"type":"text","text":"hi"},{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}],
	  "structuredContent":{"n":1},"isError":true}`
	r, err := domain.ProjectUpstreamResult([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError == nil || !*r.IsError {
		t.Fatal("isError lost")
	}
	out := r.JSON()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["_meta"] != nil || got["extra"] != nil {
		t.Fatalf("keys: %s", out)
	}
	if string(got["structuredContent"]) != `{"n":1}` || string(got["isError"]) != `true` {
		t.Fatalf("%s", out)
	}
	if domain.OutcomeOfResult(r.IsError) != domain.OutcomeUpstreamError {
		t.Fatal("outcome")
	}
}

func TestProjectContentIsKeptByteForByte(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"a <b> & \u2028 é"}]}`
	r, err := domain.ProjectUpstreamResult([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Content) != `[{"type":"text","text":"a <b> & \u2028 é"}]` {
		t.Fatalf("%s", r.Content)
	}
	if r.StructuredContent != nil || r.IsError != nil {
		t.Fatal("absent fields appeared")
	}
	out := r.JSON()
	if string(out) != `{"content":[{"type":"text","text":"a <b> & \u2028 é"}]}` {
		t.Fatalf("%s", out)
	}
}

func TestProjectDropsStructuredContentThatIsNotAnObject(t *testing.T) {
	for _, sc := range []string{`[1]`, `"s"`, `1`, `true`, `null`} {
		r, err := domain.ProjectUpstreamResult([]byte(`{"content":[],"structuredContent":` + sc + `}`))
		if err != nil {
			t.Fatalf("%s: %v", sc, err)
		}
		if r.StructuredContent != nil {
			t.Errorf("%s kept", sc)
		}
	}
	r, err := domain.ProjectUpstreamResult([]byte(`{"content":[],"structuredContent":{}}`))
	if err != nil || string(r.StructuredContent) != `{}` {
		t.Fatalf("empty object must be kept: %v %s", err, r.StructuredContent)
	}
}

func TestProjectIsErrorOnlyWhenDefined(t *testing.T) {
	r, err := domain.ProjectUpstreamResult([]byte(`{"content":[],"isError":false}`))
	if err != nil || r.IsError == nil || *r.IsError {
		t.Fatalf("false must be kept: %v", err)
	}
	if domain.OutcomeOfResult(r.IsError) != domain.OutcomeOK {
		t.Fatal("false is ok")
	}
	out := r.JSON()
	if string(out) != `{"content":[],"isError":false}` {
		t.Fatalf("%s", out)
	}
}

func TestProjectRefusesAResultThatIsNotWellFormed(t *testing.T) {
	bad := []string{
		`{"content":"x"}`, `{"content":{}}`, `{"content":null}`, `{"content":1}`,
		`{"content":[],"isError":"yes"}`, `{"content":[],"isError":null}`, `{"content":[],"isError":1}`,
		`[]`, `null`, `"s"`, `1`, ``, `{`,
	}
	for _, raw := range bad {
		if _, err := domain.ProjectUpstreamResult([]byte(raw)); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestProjectAbsentContentIsEmptyList(t *testing.T) {
	r, err := domain.ProjectUpstreamResult([]byte(`{"structuredContent":{"a":1}}`))
	if err != nil || string(r.Content) != `[]` {
		t.Fatalf("%v %s", err, r.Content)
	}
}

func TestProjectDuplicateKeysKeepTheLast(t *testing.T) {
	r, err := domain.ProjectUpstreamResult([]byte(`{"content":[1],"content":[2]}`))
	if err != nil || string(r.Content) != `[2]` {
		t.Fatalf("%v %s", err, r.Content)
	}
}

func TestFailureResultIsOneTextBlock(t *testing.T) {
	out := domain.FailureResult(`say "hi" <x>`).JSON()
	if string(out) != `{"content":[{"type":"text","text":"say \"hi\" <x>"}],"isError":true}` {
		t.Fatalf("%s", out)
	}
}
