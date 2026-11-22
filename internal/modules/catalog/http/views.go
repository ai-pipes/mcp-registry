package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/httpx"
)

// The response shapes are the contract's (packages/contracts/src/{servers,tools}.ts), field for
// field and in the order the TypeScript server wrote them. Nothing else reaches the wire: no
// version, no secret, only secretSet.

type serverDTO struct {
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	URL           string    `json:"url"`
	Auth          authDTO   `json:"auth"`
	SecretSet     bool      `json:"secretSet"`
	Access        accessDTO `json:"access"`
	TimeoutMs     int       `json:"timeoutMs"`
	DailyLimit    *int      `json:"dailyLimit"`
	Status        string    `json:"status"`
	LastSyncAt    *string   `json:"lastSyncAt"`
	LastSyncError *string   `json:"lastSyncError"`
	ToolCount     int       `json:"toolCount"`
}

type authDTO struct {
	Kind     string    `json:"kind"`
	OAuth    *oauthDTO `json:"oauth,omitempty"`
	Header   string    `json:"header,omitempty"`
	Template string    `json:"template,omitempty"`
}

type oauthDTO struct {
	AuthorizeURL         string         `json:"authorizeUrl"`
	TokenURL             string         `json:"tokenUrl"`
	RevokeURL            *string        `json:"revokeUrl,omitempty"`
	ClientID             string         `json:"clientId"`
	Scopes               []string       `json:"scopes"`
	ExtraAuthorizeParams *orderedParams `json:"extraAuthorizeParams,omitempty"`
}

type accessDTO struct {
	Kind   string   `json:"kind"`
	Groups []string `json:"groups,omitempty"`
}

// orderedParams is a JSON object whose members keep the order of the list.
type orderedParams []domain.AuthorizeParam

func (p orderedParams) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		for j, s := range []string{kv.Key, kv.Value} {
			if j == 1 {
				b.WriteByte(':')
			}
			raw, err := marshal(s)
			if err != nil {
				return nil, err
			}
			b.Write(raw)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshal is JSON.stringify: no HTML escaping, no trailing newline.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// iso is JavaScript's Date.toISOString: milliseconds and a Z.
func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func toServerDTO(v app.ServerView) serverDTO {
	d := serverDTO{
		Slug: v.Slug, Name: v.Name, Description: v.Description, URL: v.URL, Auth: toAuthDTO(v.Auth),
		SecretSet: v.SecretSet, Access: accessDTO{Kind: string(v.Access.Kind)}, TimeoutMs: v.TimeoutMs, DailyLimit: v.DailyLimit,
		Status: string(v.Status), LastSyncError: v.LastSyncError, ToolCount: v.ToolCount,
	}
	if v.Access.Kind == domain.AccessGroups {
		d.Access.Groups = append([]string{}, v.Access.Groups...)
	}
	if v.LastSyncAt != nil {
		s := iso(*v.LastSyncAt)
		d.LastSyncAt = &s
	}
	return d
}

func toAuthDTO(a domain.AuthMode) authDTO {
	d := authDTO{Kind: string(a.Kind), Header: a.Header, Template: a.Template}
	if a.OAuth != nil {
		o := &oauthDTO{
			AuthorizeURL: a.OAuth.AuthorizeURL, TokenURL: a.OAuth.TokenURL, RevokeURL: a.OAuth.RevokeURL,
			ClientID: a.OAuth.ClientID, Scopes: append([]string{}, a.OAuth.Scopes...),
		}
		if a.OAuth.ExtraAuthorizeParams != nil {
			p := orderedParams(a.OAuth.ExtraAuthorizeParams)
			o.ExtraAuthorizeParams = &p
		}
		d.OAuth = o
	}
	return d
}

type annotationsDTO struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

func toAnnotationsDTO(a domain.ToolAnnotations) annotationsDTO {
	return annotationsDTO{ReadOnlyHint: a.ReadOnlyHint, DestructiveHint: a.DestructiveHint, IdempotentHint: a.IdempotentHint, OpenWorldHint: a.OpenWorldHint}
}

type catalogToolDTO struct {
	Ref         string         `json:"ref"`
	Title       *string        `json:"title"`
	Description string         `json:"description"`
	Annotations annotationsDTO `json:"annotations"`
}

type catalogServerDTO struct {
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Tools       []catalogToolDTO `json:"tools"`
}

func toCatalogDTO(entries []app.CatalogEntry) []catalogServerDTO {
	out := make([]catalogServerDTO, 0, len(entries))
	for _, e := range entries {
		s := catalogServerDTO{Slug: e.Slug, Name: e.Name, Description: e.Description, Tools: make([]catalogToolDTO, 0, len(e.Tools))}
		for _, t := range e.Tools {
			s.Tools = append(s.Tools, catalogToolDTO{Ref: t.Ref, Title: t.Title, Description: t.Description, Annotations: toAnnotationsDTO(t.Annotations)})
		}
		out = append(out, s)
	}
	return out
}

// adminToolDTO is a catalog tool plus its name, its schemas (as stored, byte for byte up to
// white space) and the hidden flag.
type adminToolDTO struct {
	Ref          string          `json:"ref"`
	Title        *string         `json:"title"`
	Description  string          `json:"description"`
	Annotations  annotationsDTO  `json:"annotations"`
	Name         string          `json:"name"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Hidden       bool            `json:"hidden"`
}

func toAdminToolDTOs(tools []app.ToolView) []adminToolDTO {
	out := make([]adminToolDTO, 0, len(tools))
	for _, t := range tools {
		d := adminToolDTO{
			Ref: t.Ref, Title: t.Title, Description: t.Description, Annotations: toAnnotationsDTO(t.Annotations),
			Name: t.Name, InputSchema: t.InputSchema, OutputSchema: t.OutputSchema, Hidden: t.Hidden,
		}
		if len(d.InputSchema) == 0 {
			d.InputSchema = json.RawMessage(`{}`)
		}
		if len(d.OutputSchema) == 0 {
			d.OutputSchema = nil // null
		}
		out = append(out, d)
	}
	return out
}

type syncDTO struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

// writeJSON writes v like JSON.stringify: no HTML escaping, no trailing newline.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		httpx.WriteError(w, "internal", "Internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
}
