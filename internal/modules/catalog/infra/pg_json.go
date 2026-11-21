package infra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/jackc/pgx/v5"
)

// us is a time as Postgres stores it: UTC, microseconds.
func us(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func usPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := us(*t)
	return &v
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// one turns a single-row scan into (nil, nil) when there is no row.
func one[T any](v T, err error) (*T, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// orderedParams is the extra authorize parameters as one JSON OBJECT, as the TypeScript server
// stores them (`Record<string, string>`). The domain keeps them as a list in document order:
// writing emits the object with its keys in that order; reading returns them in the order of
// the text Postgres gives back for the jsonb value, which is jsonb's own key order (shorter keys
// first, then bytewise) and is the order TypeScript's Object.entries sees too.
type orderedParams []domain.AuthorizeParam

func (p orderedParams) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(kv.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(kv.Value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (p *orderedParams) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("extraAuthorizeParams is not an object")
	}
	out := orderedParams{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return errors.New("extraAuthorizeParams: bad key")
		}
		vt, err := dec.Token()
		if err != nil {
			return err
		}
		val, ok := vt.(string)
		if !ok {
			return errors.New("extraAuthorizeParams: value is not a string")
		}
		out = append(out, domain.AuthorizeParam{Key: key, Value: val})
	}
	*p = out
	return nil
}

// oauthJSON is `OAuthClientConfig` of the TypeScript server, key for key: optional keys are
// absent, never null.
type oauthJSON struct {
	AuthorizeURL         string         `json:"authorizeUrl"`
	TokenURL             string         `json:"tokenUrl"`
	ClientID             string         `json:"clientId"`
	Scopes               []string       `json:"scopes"`
	RevokeURL            *string        `json:"revokeUrl,omitempty"`
	ExtraAuthorizeParams *orderedParams `json:"extraAuthorizeParams,omitempty"`
}

// authJSON is `AuthMode` of the TypeScript server: `{kind:'none'}`, `{kind, header, template}`
// for shared_secret and pat_user, and `{kind, oauth, header, template}` for oauth_user.
type authJSON struct {
	Kind     string     `json:"kind"`
	OAuth    *oauthJSON `json:"oauth,omitempty"`
	Header   string     `json:"header,omitempty"`
	Template string     `json:"template,omitempty"`
}

func encodeAuth(m domain.AuthMode) ([]byte, error) {
	j := authJSON{Kind: string(m.Kind)}
	if m.Kind != domain.AuthNone {
		j.Header, j.Template = m.Header, m.Template
	}
	if m.Kind == domain.AuthOAuthUser && m.OAuth != nil {
		o := &oauthJSON{
			AuthorizeURL: m.OAuth.AuthorizeURL, TokenURL: m.OAuth.TokenURL, ClientID: m.OAuth.ClientID,
			Scopes: m.OAuth.Scopes, RevokeURL: m.OAuth.RevokeURL,
		}
		if o.Scopes == nil {
			o.Scopes = []string{}
		}
		if m.OAuth.ExtraAuthorizeParams != nil {
			p := orderedParams(m.OAuth.ExtraAuthorizeParams)
			o.ExtraAuthorizeParams = &p
		}
		j.OAuth = o
	}
	return json.Marshal(j)
}

// decodeAuth reads a stored auth mode without validating it (a stored row is trusted, as
// Server.rehydrate does). A shape the domain cannot hold is an error.
func decodeAuth(raw []byte) (domain.AuthMode, error) {
	var j authJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return domain.AuthMode{}, errors.New("stored auth is not a valid auth mode")
	}
	m := domain.AuthMode{Kind: domain.AuthKind(j.Kind), Header: j.Header, Template: j.Template}
	switch m.Kind {
	case domain.AuthNone, domain.AuthSharedSecret, domain.AuthPATUser:
	case domain.AuthOAuthUser:
		if j.OAuth == nil {
			return domain.AuthMode{}, errors.New("stored oauth_user auth has no oauth block")
		}
		o := &domain.OAuthClientConfig{
			AuthorizeURL: j.OAuth.AuthorizeURL, TokenURL: j.OAuth.TokenURL, ClientID: j.OAuth.ClientID,
			Scopes: j.OAuth.Scopes, RevokeURL: j.OAuth.RevokeURL,
		}
		if o.Scopes == nil {
			o.Scopes = []string{}
		}
		if j.OAuth.ExtraAuthorizeParams != nil {
			o.ExtraAuthorizeParams = []domain.AuthorizeParam(*j.OAuth.ExtraAuthorizeParams)
		}
		m.OAuth = o
	default:
		return domain.AuthMode{}, fmt.Errorf("stored auth has unknown kind %q", j.Kind)
	}
	return m, nil
}

// accessJSON is `Access`: `{kind:'all'}` or `{kind:'groups', groups:[...]}`.
type accessJSON struct {
	Kind   string   `json:"kind"`
	Groups []string `json:"groups,omitempty"`
}

func encodeAccess(a domain.Access) ([]byte, error) {
	if a.Kind != domain.AccessGroups {
		return json.Marshal(struct {
			Kind string `json:"kind"`
		}{string(a.Kind)})
	}
	groups := a.Groups
	if groups == nil {
		groups = []string{}
	}
	return json.Marshal(struct {
		Kind   string   `json:"kind"`
		Groups []string `json:"groups"`
	}{string(a.Kind), groups})
}

func decodeAccess(raw []byte) (domain.Access, error) {
	var j accessJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return domain.Access{}, errors.New("stored access is not valid")
	}
	switch domain.AccessKind(j.Kind) {
	case domain.AccessAll:
		return domain.Access{Kind: domain.AccessAll}, nil
	case domain.AccessGroups:
		g := j.Groups
		if g == nil {
			g = []string{}
		}
		return domain.Access{Kind: domain.AccessGroups, Groups: g}, nil
	default:
		return domain.Access{}, fmt.Errorf("stored access has unknown kind %q", j.Kind)
	}
}

// jsonText turns bytes for a jsonb parameter (`$n::text::jsonb`) into a value pgx sends as text;
// nil stays NULL.
func jsonText(raw []byte) *string {
	if raw == nil {
		return nil
	}
	s := string(raw)
	return &s
}

// storedJSON normalises a jsonb column read back as bytes: SQL NULL and a JSON null are both
// "absent" (nil).
func storedJSON(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return bytes.Clone(raw)
}

// decodeAnnotations reads the stored hints; like the snapshot, it keeps only booleans.
func decodeAnnotations(raw []byte) domain.ToolAnnotations {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return domain.ToolAnnotations{}
	}
	hint := func(key string) *bool {
		if v, ok := m[key].(bool); ok {
			return &v
		}
		return nil
	}
	return domain.ToolAnnotations{
		ReadOnlyHint: hint("readOnlyHint"), DestructiveHint: hint("destructiveHint"),
		IdempotentHint: hint("idempotentHint"), OpenWorldHint: hint("openWorldHint"),
	}
}
