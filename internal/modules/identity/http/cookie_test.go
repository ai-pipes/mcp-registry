package http_test

import "net/http"

// reqCookie is a cookie as a browser sends it back: a name and a value only.
func reqCookie(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value} //nolint:gosec // G124: a request cookie carries no attributes
}
