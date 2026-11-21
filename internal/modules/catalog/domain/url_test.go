package domain_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func TestValidHTTPURL(t *testing.T) {
	ok := []string{
		"https://mcp.example.com/gitea", "http://localhost:3000/mcp", "HTTP://EXAMPLE.COM", "https://a.example/b?c=d#e",
		"https://user:pw@example.com/", "https://[::1]:8080/x", "http://127.0.0.1/", "http://10.0.0.1:65535",
		"http://example.com:", "http:/x", "http:x", "https:\\\\example.com\\a", "http:///example.com",
		" https://example.com ", "https://exa\tmple.com/\n", "\x01https://example.com", "https://xn--e1afmkfd.xn--p1ai/",
		"https://пример.рф/", "https://a_b.example/", "http://example.com:0", "https://example.com/пример path",
		"https://[2001:db8::1]/", "http://1.2.3.4.example/", "http://0x7f.1/", "https://ex%61mple.com/", "http://a#b@c", "http://0x.example/x/", "http://1.2.3", "http://0300.0250.0.1", "http://1.2.3.4.", "http://256", "http://0xg", "http://1e3", "http://0x", "http://[::ffff:1.2.3.4]/", "http://example.com./", "http://u:p@w:1@h/", "http://1.2.3.4..", "http://a b@c.d/", "https://a.b/c\\d",
	}
	for _, u := range ok {
		if !domain.ValidHTTPURL(u) {
			t.Errorf("%q should parse", u)
		}
	}
	bad := []string{
		"", "nope", "not a url", "ftp://x.example", "file:///etc/passwd", "javascript:alert(1)", "mailto:a@b.c",
		"//example.com", "/path", "example.com", "http://", "http://:80", "https://user@", "http://ex ample.com",
		"http://example.com:65536", "http://example.com:80a", "http://example.com:-1", "http://[::1", "http://[zz]/",
		"http://[::1]x", "http://a<b", "http://a>b", "http://a^b", "http://a|b", "http://a[b",
		"http://%zz.example", "http://a%2fb.example", "http://256.1.1.1", "http://1.2.3.4.5",
		"http://99999999999", "h ttp://x", "1http://x", "http//x", ":http://x", "http\x00://x", "http://4294967296", "http://1.2.3.256", "http://09", "http://[::1%25eth0]/", "http://[1:2:3:4:5:6:7:8:9]/", "http://@/", "http://a.b:99999", "https:///", "https://a\x00b",
	}
	for _, u := range bad {
		if domain.ValidHTTPURL(u) {
			t.Errorf("%q should not parse", u)
		}
	}
}
