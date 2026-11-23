package domain

// AuthKind is how a server authenticates to its upstream. The values are the catalog's kinds;
// the gateway keeps its own copy because a domain package imports no other module.
type AuthKind string

// The kinds of upstream authentication.
const (
	AuthNone         AuthKind = "none"
	AuthSharedSecret AuthKind = "shared_secret"
	AuthOAuthUser    AuthKind = "oauth_user"
	AuthPATUser      AuthKind = "pat_user"
)
