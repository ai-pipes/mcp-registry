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

type ptrUntagged struct {
	P *untagged `json:"p"`
}

// A field without a json tag would silently never match under json/v2's case-sensitive names
// ({"Role":"x"} or {"role":"x"} would not fill it): refuse the destination type loudly.
func TestDecodeJSONRefusesAnUntaggedExportedField(t *testing.T) {
	for name, dst := range map[string]any{
		"direct": &untagged{}, "nested": &nestedUntagged{}, "slice element": &sliceUntagged{}, "pointer": &ptrUntagged{},
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
