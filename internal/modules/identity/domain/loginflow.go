package domain

import (
	"strings"
	"time"
)

const (
	// LoginStateTTL is how long a started OIDC login stays valid.
	LoginStateTTL = 10 * time.Minute
	// FailedCallbackAuditLimit is how many failed code exchanges per client IP and window are
	// written to the audit log; later ones are still refused, just not recorded.
	FailedCallbackAuditLimit = 20
	// FailedCallbackWindowSec is that window in seconds.
	FailedCallbackWindowSec = 60
)

// LoginStateExpiry is when a login attempt started at now stops being valid.
func LoginStateExpiry(now time.Time) time.Time { return now.Add(LoginStateTTL) }

// IsLoginStateExpired reports whether an attempt is expired from its expiry instant on (now >= expiresAt).
func IsLoginStateExpired(expiresAt, now time.Time) bool { return !now.Before(expiresAt) }

// NormalizeReturnTo allows only a same-origin relative path as a post-login target; anything else
// becomes "/". CR/LF and other control characters (below 0x20, and 0x7f) would split headers.
func NormalizeReturnTo(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `/\`) {
		return "/"
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	return raw
}
