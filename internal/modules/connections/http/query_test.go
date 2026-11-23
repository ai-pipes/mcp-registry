package http

import "testing"

// absent marks a key the reference's parser (Hono 4.13.18, utils/url.js getQueryParam, run on the
// WHATWG-serialised URL in node) does not find: it answers undefined.
const absent = "\x00absent"

// The table is the output of Hono's own getQueryParam for each raw query, for the keys code and
// state: split on & only (a ; is data), the first = of a pair splits name and value, + is a space,
// percent-decoding is lenient (a run of %XX that is not valid UTF-8 stays as written, a stray %
// stays), the first occurrence of a key wins (an empty one included), a fragment is cut off.
func TestQueryParamIsHonosParser(t *testing.T) {
	for _, c := range []struct{ query, code, state string }{
		{"code=a;b&state=s", "a;b", "s"},
		{"code=abc&state=%ZZ", "abc", "%ZZ"},
		{"code=%FF&state=s", "%FF", "s"},
		{"code=a+b&state=s", "a b", "s"},
		{"code=a%2Bb&state=s", "a+b", "s"},
		{"code=first&code=second&state=s", "first", "s"},
		{"code=&code=second&state=s", "", "s"},
		{"state=s&code", "", "s"},
		{"code&code=x&state=s", "", "s"},
		{"code=h%C3%A9llo&state=s", "héllo", "s"},
		{"code=héllo&state=s", "héllo", "s"},
		{"code=%E2%82%AC%FF%E2%82&state=s", "%E2%82%AC%FF%E2%82", "s"},
		{"code=a=b=c&state=s", "a=b=c", "s"},
		{"&&code=x&&state=s&", "x", "s"},
		{"c%6Fde=viaescape&state=s", "viaescape", "s"},
		{"co+de=plus&code=real&state=s", "real", "s"},
		{"code=%&state=%4", "%", "%4"},
		{"code=a%20b%2&state=s", "a b%2", "s"},
		{"state=s;code=x", absent, "s;code=x"},
		{"code=%F0%9F%98%80&state=%f0%9f%98%80", "😀", "😀"},
		{"code=a#b&state=s", "a", absent},
		{"code=1&state=2&code=3", "1", "2"},
		{"x=1&code=%00&state=s", "\x00", "s"},
		{"code=%E0%80%80&state=ok", "%E0%80%80", "ok"},
		{"a%2Bb=1&code=z&state=s", "z", "s"},
		{"code=a%252Bb&state=s", "a%2Bb", "s"},
	} {
		for key, want := range map[string]string{"code": c.code, "state": c.state} {
			got, ok := queryParam(c.query, key)
			if want == absent {
				if ok {
					t.Errorf("%q %s = %q, want absent", c.query, key, got)
				}
				continue
			}
			if !ok || got != want {
				t.Errorf("%q %s = %q (found %v), want %q", c.query, key, got, ok, want)
			}
		}
	}
}
