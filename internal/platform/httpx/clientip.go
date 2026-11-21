package httpx

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP resolves the client address for ClientIPOf: the socket peer, or with trustProxy the
// X-Forwarded-For hop `hops` entries from the right, i.e. the address the outermost of `hops`
// trusted proxies saw (proxies append on the right, everything further left is client-controlled).
// The header may arrive as several lines: they are joined in order. A list that is too short or a
// hop that is not an IP address falls back to the peer. Without trustProxy the header is ignored,
// otherwise anyone could pick their own rate-limit key.
func ClientIP(trustProxy bool, hops int) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := peerAddr(r)
			if trustProxy {
				if hop, ok := forwardedFor(r.Header.Values("X-Forwarded-For"), hops); ok {
					ip = hop
				}
			}
			if s := stateOf(r); s != nil {
				s.ip = ip
			} else {
				r = r.WithContext(contextWithState(r, &reqState{ip: ip}))
			}
			next.ServeHTTP(w, r)
		})
	}
}

func forwardedFor(lines []string, hops int) (string, bool) {
	if len(lines) == 0 || hops < 1 {
		return "", false
	}
	list := strings.Split(strings.Join(lines, ","), ",")
	if hops > len(list) {
		return "", false
	}
	hop := strings.TrimSpace(list[len(list)-hops])
	if net.ParseIP(hop) == nil {
		return "", false
	}
	return hop, true
}

// ClientIPOf returns the client address resolved by ClientIP (or by RequestID: the socket peer);
// without either middleware it is the socket peer of r.
func ClientIPOf(r *http.Request) string {
	if s := stateOf(r); s != nil && s.ip != "" {
		return s.ip
	}
	return peerAddr(r)
}
