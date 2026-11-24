package domain

import (
	"strconv"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const (
	defaultTimeoutMs = 120_000
	maxTimeoutMs     = 3_600_000
	maxName          = 100
	maxDescription   = 2000
	maxSecret        = 4096
)

// editable is the part of a server an admin edits; validate is the one place that checks it.
type editable struct {
	name        string
	description string
	url         string
	auth        AuthMode
	access      Access
	timeoutMs   int
	dailyLimit  *int
}

func invalidServer(message string) error { return newError(CodeInvalidServer, message) }

// validate checks the fields in the order name, description, URL, timeout, daily limit, auth,
// access and returns them normalised (name and description trimmed; the URL exactly as given).
func validate(f editable) (editable, error) {
	name, description := trim(f.name), trim(f.description)
	if n := utf16Len(name); n < 1 || n > maxName {
		return editable{}, invalidServer("server name must be 1-" + strconv.Itoa(maxName) + " characters")
	}
	if utf16Len(description) > maxDescription {
		return editable{}, invalidServer("server description must be at most " + strconv.Itoa(maxDescription) + " characters")
	}
	if !ValidHTTPURL(f.url) {
		return editable{}, invalidServer("upstream URL must be a valid http(s) URL")
	}
	if f.timeoutMs < 1 || f.timeoutMs > maxTimeoutMs {
		return editable{}, invalidServer("timeout must be 1-" + strconv.Itoa(maxTimeoutMs) + " ms")
	}
	if f.dailyLimit != nil && *f.dailyLimit < 1 {
		return editable{}, invalidServer("daily limit must be a positive integer or empty")
	}
	auth, err := ParseAuthMode(f.auth)
	if err != nil {
		return editable{}, err
	}
	access, err := ParseAccess(f.access)
	if err != nil {
		return editable{}, err
	}
	return editable{
		name: name, description: description, url: f.url, auth: auth, access: access,
		timeoutMs: f.timeoutMs, dailyLimit: copyInt(f.dailyLimit),
	}, nil
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func samePtr(a, b *string) bool { return sameString(a, b) }

// secretDestinationChanged: whether the stored secret would now be sent somewhere it was not
// stored for: another auth kind; for shared_secret another upstream URL (the key goes there);
// for oauth_user another token URL, revoke URL or client ID (the client secret goes to the token
// endpoint, and to revoke). Header, template, scopes, authorize URL and the upstream URL of an
// oauth_user server (it gets the person's token, not this secret) do not move the secret.
// URLs are compared as plain strings, without normalisation.
func secretDestinationChanged(from, to editable) bool {
	if from.auth.Kind != to.auth.Kind {
		return true
	}
	switch from.auth.Kind {
	case AuthSharedSecret:
		return from.url != to.url
	case AuthOAuthUser:
		a, b := from.auth.OAuth, to.auth.OAuth
		return a.TokenURL != b.TokenURL || !samePtr(a.RevokeURL, b.RevokeURL) || a.ClientID != b.ClientID
	}
	return false
}

// ServerState is a stored server, for RehydrateServer.
type ServerState struct {
	Slug string
	// Version is the stored row's version when it was read (optimistic concurrency): the
	// repository writes an edit only if the row is still at it. 0 for a new server.
	Version      int64
	Name         string
	Description  string
	URL          string
	Auth         AuthMode
	SealedSecret kernel.SecretBytes // the zero value: no secret (ciphertext is not printed either)
	Access       Access
	TimeoutMs    int
	DailyLimit   *int
	Status       ServerStatus
	LastSyncAt   *time.Time
	// LastSyncError is the text of the last failure; nil after a success.
	LastSyncError *string
	SyncUserID    *string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Server is the aggregate: an upstream MCP server in the catalog. It holds the secret only
// sealed (by kernel.SecretBox, outside the domain) and never seals or opens anything itself.
// Events it produces are drained with PullEvents and written to the outbox in the same
// transaction as the aggregate.
type Server struct {
	s      ServerState
	events []kernel.Event
	// secretDirty: the secret was set, cleared or re-sealed since the server was loaded. The
	// repository writes the secret only then: a stale copy must not put back a secret someone
	// else changed meanwhile (another admin, a key rotation).
	secretDirty bool
}

// NewServerParams are the values of a new server. TimeoutMs nil means the default (120 s);
// DailyLimit nil means no limit.
type NewServerParams struct {
	Slug        string
	Name        string
	Description string
	URL         string
	Auth        AuthMode
	Access      Access
	TimeoutMs   *int
	DailyLimit  *int
	Now         time.Time
}

// NewServer validates and creates a server (active, no secret, version 0) and records one
// server_changed event.
func NewServer(p NewServerParams) (*Server, error) {
	slug, err := ParseSlug(p.Slug)
	if err != nil {
		return nil, err
	}
	timeout := defaultTimeoutMs
	if p.TimeoutMs != nil {
		timeout = *p.TimeoutMs
	}
	v, err := validate(editable{
		name: p.Name, description: p.Description, url: p.URL, auth: p.Auth, access: p.Access,
		timeoutMs: timeout, dailyLimit: p.DailyLimit,
	})
	if err != nil {
		return nil, err
	}
	s := &Server{
		s: ServerState{
			Slug: slug, Name: v.name, Description: v.description, URL: v.url, Auth: v.auth, Access: v.access,
			TimeoutMs: v.timeoutMs, DailyLimit: v.dailyLimit, Status: StatusActive, CreatedAt: p.Now, UpdatedAt: p.Now,
		},
		secretDirty: true,
	}
	s.markChanged()
	return s, nil
}

// RehydrateServer restores a stored server: no validation, no events, secret unchanged.
func RehydrateServer(st ServerState) *Server {
	return &Server{s: st.clone()}
}

func (st ServerState) clone() ServerState {
	st.Auth = st.Auth.Clone()
	st.Access = st.Access.Clone()
	st.DailyLimit = copyInt(st.DailyLimit)
	st.LastSyncAt = copyTime(st.LastSyncAt)
	st.LastSyncError = copyString(st.LastSyncError)
	st.SyncUserID = copyString(st.SyncUserID)
	return st
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// CheckSecret checks a secret to be stored: non-blank, at most 4096 UTF-16 units. It is not
// trimmed: the caller seals it as given.
func CheckSecret(plain string) error {
	if trim(plain) == "" || utf16Len(plain) > maxSecret {
		return invalidServer("secret must be non-blank and at most " + strconv.Itoa(maxSecret) + " characters")
	}
	return nil
}

// Slug returns the server's Slug.
func (s *Server) Slug() string { return s.s.Slug }

// Version returns the server's Version.
func (s *Server) Version() int64 { return s.s.Version }

// Name returns the server's Name.
func (s *Server) Name() string { return s.s.Name }

// Description returns the server's Description.
func (s *Server) Description() string { return s.s.Description }

// URL returns the server's URL.
func (s *Server) URL() string { return s.s.URL }

// Auth returns the server's Auth.
func (s *Server) Auth() AuthMode { return s.s.Auth.Clone() }

// Access returns the server's Access.
func (s *Server) Access() Access { return s.s.Access.Clone() }

// TimeoutMs returns the server's TimeoutMs.
func (s *Server) TimeoutMs() int { return s.s.TimeoutMs }

// DailyLimit returns the server's DailyLimit.
func (s *Server) DailyLimit() *int { return copyInt(s.s.DailyLimit) }

// Status returns the server's Status.
func (s *Server) Status() ServerStatus { return s.s.Status }

// LastSyncAt returns the server's LastSyncAt.
func (s *Server) LastSyncAt() *time.Time { return copyTime(s.s.LastSyncAt) }

// LastSyncError returns the server's LastSyncError.
func (s *Server) LastSyncError() *string { return copyString(s.s.LastSyncError) }

// SyncUserID returns the server's SyncUserID.
func (s *Server) SyncUserID() *string { return copyString(s.s.SyncUserID) }

// CreatedAt returns the server's CreatedAt.
func (s *Server) CreatedAt() time.Time { return s.s.CreatedAt }

// UpdatedAt returns the server's UpdatedAt.
func (s *Server) UpdatedAt() time.Time { return s.s.UpdatedAt }

// SecretSet returns the server's SecretSet.
func (s *Server) SecretSet() bool { return s.s.SealedSecret.IsSet() }

// SecretGoesTo reports whether the stored secret belongs to the configuration url + auth (one a
// caller read earlier): true unless an edit from this server's configuration to that one would
// have cleared the secret (secretDestinationChanged). Since every edit that moves the secret
// clears it in the same write, the stored secret always belongs to the stored configuration, so
// this holds for the secret read together with it whatever happened in between.
func (s *Server) SecretGoesTo(url string, auth AuthMode) bool {
	return !secretDestinationChanged(editable{url: s.s.URL, auth: s.s.Auth}, editable{url: url, auth: auth})
}

// SecretChanged returns the server's SecretChanged.
func (s *Server) SecretChanged() bool { return s.secretDirty }

// CanBeUsedBy reports whether the actor may use this server (see CanUse).
func (s *Server) CanBeUsedBy(a Actor) bool { return CanUse(s.s.Access, s.s.Status, a) }

// SealedSecret is the encrypted secret (nil: none), a copy, for the repository and for opening it
// in a use case. Never serialise it.
func (s *Server) SealedSecret() []byte {
	if !s.s.SealedSecret.IsSet() {
		return nil
	}
	if b := s.s.SealedSecret.Reveal(); b != nil {
		return b
	}
	return []byte{}
}

// SealedSecretOf is the stored form of a sealed secret: nil is "no secret", any other value
// (empty included) is kept, copied.
func SealedSecretOf(sealed []byte) kernel.SecretBytes {
	if sealed == nil {
		return kernel.SecretBytes{}
	}
	return kernel.NewSecretBytes(sealed)
}

// Changes is a partial edit: only the fields marked with Some are changed.
type Changes struct {
	Name        Opt[string]
	Description Opt[string]
	URL         Opt[string]
	Auth        Opt[AuthMode]
	Access      Opt[Access]
	TimeoutMs   Opt[int]
	DailyLimit  Opt[*int] // Some[*int](nil) clears the limit
}

// Update is an all-or-nothing edit of the given fields. A change of name, description or access
// tells the search index. A change of auth kind, or of the place the secret is sent to
// (secretDestinationChanged), drops the stored secret; secretCleared says so, for the audit.
func (s *Server) Update(c Changes, now time.Time) (secretCleared bool, err error) {
	current := editable{
		name: s.s.Name, description: s.s.Description, url: s.s.URL, auth: s.s.Auth, access: s.s.Access,
		timeoutMs: s.s.TimeoutMs, dailyLimit: s.s.DailyLimit,
	}
	v, err := validate(editable{
		name: c.Name.or(current.name), description: c.Description.or(current.description), url: c.URL.or(current.url),
		auth: c.Auth.or(current.auth), access: c.Access.or(current.access),
		timeoutMs: c.TimeoutMs.or(current.timeoutMs), dailyLimit: c.DailyLimit.or(current.dailyLimit),
	})
	if err != nil {
		return false, err
	}
	searchable := v.name != s.s.Name || v.description != s.s.Description || !v.access.Equal(s.s.Access)
	secretCleared = s.s.SealedSecret.IsSet() && secretDestinationChanged(current, v)
	s.s.Name, s.s.Description, s.s.URL, s.s.Auth, s.s.Access = v.name, v.description, v.url, v.auth, v.access
	s.s.TimeoutMs, s.s.DailyLimit, s.s.UpdatedAt = v.timeoutMs, v.dailyLimit, now
	if secretCleared {
		s.s.SealedSecret = kernel.SecretBytes{}
		s.secretDirty = true
	}
	if searchable {
		s.markChanged()
	}
	return secretCleared, nil
}

// ReplaceSealedSecret stores the same secret sealed under another key (SECRETS_KEY rotation).
// Not an edit: updatedAt and the events stay as they are.
func (s *Server) ReplaceSealedSecret(sealed []byte) {
	s.s.SealedSecret = SealedSecretOf(sealed)
	s.secretDirty = true
}

// SetSealedSecret stores a sealed secret. Only a mode that holds one accepts it; an empty value
// is refused (use ClearSecret).
func (s *Server) SetSealedSecret(sealed []byte, now time.Time) error {
	if !s.s.Auth.HoldsSecret() {
		return invalidServer(`auth mode "` + string(s.s.Auth.Kind) + `" does not use a server secret`)
	}
	if len(sealed) == 0 {
		return invalidServer("secret must be non-blank and at most " + strconv.Itoa(maxSecret) + " characters")
	}
	s.s.SealedSecret = SealedSecretOf(sealed)
	s.s.UpdatedAt = now
	s.secretDirty = true
	return nil
}

// ClearSecret removes the stored secret; always allowed.
func (s *Server) ClearSecret(now time.Time) {
	s.s.SealedSecret = kernel.SecretBytes{}
	s.s.UpdatedAt = now
	s.secretDirty = true
}

// SetStatus reports false when nothing changed (no event, nothing to save).
func (s *Server) SetStatus(status ServerStatus, now time.Time) bool {
	if s.s.Status == status {
		return false
	}
	s.s.Status = status
	s.s.UpdatedAt = now
	s.markChanged()
	return true
}

// SyncCredentialsOwner is whose personal connection a synchronisation uses: the admin who
// started it, or, for the scheduler (nil), the last admin who synchronised the server successfully.
func (s *Server) SyncCredentialsOwner(startedBy *string) *string {
	if startedBy != nil {
		return copyString(startedBy)
	}
	return copyString(s.s.SyncUserID)
}

// ApplySync records a synchronisation attempt. LastSyncAt is when the latest attempt started; an
// attempt that started before it is ignored (false), so a slow old run cannot undo a newer one
// (an equal start is accepted). A success clears the error; only a run that names SyncedBy
// changes SyncUserID.
func (s *Server) ApplySync(r SyncRecord) bool {
	if last := s.s.LastSyncAt; last != nil && r.StartedAt.Before(*last) {
		return false
	}
	started := r.StartedAt
	s.s.LastSyncAt = &started
	s.s.LastSyncError = copyString(r.Error)
	if r.SyncedBy != nil {
		s.s.SyncUserID = copyString(r.SyncedBy)
	}
	return true
}

// markChanged records server_changed; one pending event is enough however many fields changed
// before the save.
func (s *Server) markChanged() {
	for _, e := range s.events {
		if e.Type == EventServerChanged {
			return
		}
	}
	s.events = append(s.events, ServerChangedEvent(s.s.Slug))
}

// PullEvents drains the events recorded since the last call.
func (s *Server) PullEvents() []kernel.Event {
	out := s.events
	s.events = nil
	return out
}
