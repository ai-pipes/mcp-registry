package infra

import (
	"reflect"
	"testing"
)

func TestParseWWWAuthenticate(t *testing.T) {
	type c = challenge
	cases := []struct {
		header string
		want   []challenge
	}{
		{`Bearer error="invalid_token"`, []c{{"bearer", map[string]string{"error": "invalid_token"}}}},
		{`Bearer realm="x", error=invalid_token, error_description="a \"quoted\" word"`, []c{{"bearer", map[string]string{"realm": "x", "error": "invalid_token", "error_description": `a "quoted" word`}}}},
		{`Basic realm="api"`, []c{{"basic", map[string]string{"realm": "api"}}}},
		{`Bearer`, []c{{"bearer", map[string]string{}}}},
		{`Bearer error="a", Basic realm="b"`, []c{{"bearer", map[string]string{"error": "a"}}, {"basic", map[string]string{"realm": "b"}}}},
		{`Negotiate abc123==`, []c{{"negotiate", map[string]string{}}}},
		{`Bearer ERROR="x"`, []c{{"bearer", map[string]string{"error": "x"}}}},
		{``, nil},
		{`;;;`, nil},
		{`Bearer error="unterminated`, nil},
		{`Bearer@ error="x"`, nil},
	}
	for _, tc := range cases {
		got := parseWWWAuthenticate(tc.header)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parse(%q) = %#v, want %#v", tc.header, got, tc.want)
		}
	}
}
