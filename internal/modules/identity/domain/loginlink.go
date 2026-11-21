package domain

import (
	"strings"
	"time"
)

const (
	// LoginLinkTTL is how long a login link is valid after it is issued.
	LoginLinkTTL = 15 * time.Minute
	// LoginCodeMark starts every login-link code.
	LoginCodeMark = "ll_"
	// MaxLoginCodeLength is the longest string that is looked up as a login code.
	MaxLoginCodeLength = 100
)

// LoginLinkExpiry is when a link issued at now stops working.
func LoginLinkExpiry(now time.Time) time.Time { return now.Add(LoginLinkTTL) }

// IsLoginLinkExpired reports whether a link is expired from its expiry instant on (now >= expiresAt).
func IsLoginLinkExpired(expiresAt, now time.Time) bool { return !now.Before(expiresAt) }

// NewLoginCode marks a random part as a login code.
func NewLoginCode(randomPart string) string { return LoginCodeMark + randomPart }

// LooksLikeLoginCode is a cheap shape check before a code reaches the database.
func LooksLikeLoginCode(code string) bool {
	return utf16Len(code) <= MaxLoginCodeLength && strings.HasPrefix(code, LoginCodeMark)
}

// LoginLinkURL puts the code in the URL fragment, which browsers do not send to the server.
// Exactly one trailing slash of publicURL is dropped.
func LoginLinkURL(publicURL, code string) string {
	return strings.TrimSuffix(publicURL, "/") + "/login#link=" + code
}

// Person is what NeedsBootstrapLink needs to know about a person.
type Person struct {
	Sub         string
	Disabled    bool
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// BootstrapAction is the decision of NeedsBootstrapLink.
type BootstrapAction string

// The three decisions: provision the first admin, issue a new link for the unused one, or do nothing.
const (
	BootstrapCreate  BootstrapAction = "create"
	BootstrapReissue BootstrapAction = "reissue"
	BootstrapNone    BootstrapAction = "none"
)

// NeedsBootstrapLink decides whether a server without OIDC must hand out a first-admin link at
// start: create when there are no people at all, reissue while the only person is the enabled
// local admin who has never signed in (so a lost first-start link cannot lock the install out),
// else none. Provisioning stamps lastLoginAt = createdAt; any real sign-in moves it later.
func NeedsBootstrapLink(people []Person, localAdminSub string) BootstrapAction {
	if len(people) == 0 {
		return BootstrapCreate
	}
	only := people[0]
	if len(people) != 1 || only.Sub != localAdminSub || only.Disabled {
		return BootstrapNone
	}
	if only.LastLoginAt == nil || !only.LastLoginAt.After(only.CreatedAt) {
		return BootstrapReissue
	}
	return BootstrapNone
}
