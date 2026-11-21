package httpx_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

func decode(t *testing.T, body string, dst any) error {
	t.Helper()
	return httpx.DecodeJSON(httptest.NewRequest("POST", "/x", strings.NewReader(body)), dst)
}

type goodBody struct {
	Name  string   `json:"name"`
	Admin *bool    `json:"admin"`
	Inner goodIn   `json:"inner"`
	List  []goodIn `json:"list"`
	Skip  string   `json:"-"`
	priv  string
}

type goodIn struct {
	ID string `json:"id,omitempty"`
}

func TestDecodeJSONAcceptsFullyTaggedStructs(t *testing.T) {
	var b goodBody
	if err := decode(t, `{"name":"a","inner":{"id":"7"},"list":[{"id":"8"}]}`, &b); err != nil {
		t.Fatal(err)
	}
	if b.Name != "a" || b.Inner.ID != "7" || b.List[0].ID != "8" {
		t.Errorf("%+v", b)
	}
	_ = b.priv
}

type untagged struct {
	Name string `json:"name"`
	Role string
}

type nestedUntagged struct {
	In struct {
		Deep string
	} `json:"in"`
}

type sliceUntagged struct {
	Items []untagged `json:"items"`
}

type nameless struct {
	Role string `json:",omitempty"`
}

type inlineOnly struct {
	Role string `json:",inline"`
}

type mapUntagged struct {
	M map[string]untagged `json:"m"`
}

type Embedded struct{ Deep string }

type embedsUntagged struct {
	Embedded
	Name string `json:"name"`
}

type EmbeddedOK struct {
	Deep string `json:"deep"`
}

type embedsTagged struct {
	EmbeddedOK
	Name string `json:"name"`
}

type dashAndNames struct {
	A string `json:"-"`
	B string `json:"b,omitempty"`
}

type ptrUntagged struct {
	P *untagged `json:"p"`
}

// A field without a json tag would silently never match under json/v2's case-sensitive names
// ({"Role":"x"} or {"role":"x"} would not fill it): refuse the destination type loudly.
func TestDecodeJSONRefusesAnUntaggedExportedField(t *testing.T) {
	for name, dst := range map[string]any{
		"direct": &untagged{}, "nested": &nestedUntagged{}, "slice element": &sliceUntagged{}, "pointer": &ptrUntagged{},
		"a tag without a name": &nameless{}, "an option-only tag": &inlineOnly{}, "map value": &mapUntagged{}, "embedded struct": &embedsUntagged{},
	} {
		func() {
			defer func() {
				r := recover()
				msg, _ := r.(string)
				if !strings.Contains(msg, "json tag") {
					t.Errorf("%s: recovered %v, want a panic naming the missing json tag", name, r)
				}
			}()
			_ = decode(t, `{}`, dst)
		}()
	}
}

func TestDecodeJSONAcceptsDashNamedOptionsAndTaggedEmbeddedStructs(t *testing.T) {
	var a dashAndNames
	if err := decode(t, `{"b":"x"}`, &a); err != nil || a.B != "x" {
		t.Fatalf("%+v %v", a, err)
	}
	var e embedsTagged
	if err := decode(t, `{"deep":"d","name":"n"}`, &e); err != nil || e.Deep != "d" || e.Name != "n" {
		t.Fatalf("%+v %v", e, err)
	}
}
