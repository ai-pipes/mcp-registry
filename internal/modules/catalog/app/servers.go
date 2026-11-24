package app

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// CreateServerCommand creates a server. Secret is the initial secret for a mode that holds one
// (a shared key or the OAuth client secret); unset means none.
type CreateServerCommand struct {
	Principal Principal
	Input     ServerInput
	Secret    kernel.Secret
}

// CreateServer is for admins only; the contract's limits (the first breach is invalid_request), then the
// domain (slug, then the fields), then the secret (checked, sealed). The server and its
// server_changed event are written in one transaction; a taken slug is server_exists. Audit:
// server.create {auth: <kind>}, then server.secret_set when a secret was stored.
func CreateServer(ctx context.Context, deps Deps, cmd CreateServerCommand) (ServerView, error) {
	if err := requireAdmin(cmd.Principal); err != nil {
		return ServerView{}, err
	}
	fields, issues := normaliseFields(cmd.Input.Fields)
	issues = append(issues, slugIssues(cmd.Input.Slug)...)
	if len(issues) > 0 {
		return ServerView{}, invalidRequest(issues[0])
	}
	now := deps.Clock.Now()
	server, err := domain.NewServer(domain.NewServerParams{
		Slug: cmd.Input.Slug, Name: fields.Name, Description: fields.Description, URL: fields.URL, Auth: fields.Auth,
		Access: fields.Access, TimeoutMs: fields.TimeoutMs, DailyLimit: fields.DailyLimit, Now: now,
	})
	if err != nil {
		return ServerView{}, err
	}
	if cmd.Secret.IsSet() {
		if err := storeSecret(deps, server, cmd.Secret.Reveal(), now); err != nil {
			return ServerView{}, err
		}
	}
	if err := deps.Servers.Create(ctx, server, server.PullEvents()); err != nil {
		return ServerView{}, err
	}
	if err := record(ctx, deps, cmd.Principal, "server.create", server.Slug(), map[string]any{"auth": string(server.Auth().Kind)}); err != nil {
		return ServerView{}, err
	}
	if server.SecretSet() {
		if err := record(ctx, deps, cmd.Principal, "server.secret_set", server.Slug(), nil); err != nil {
			return ServerView{}, err
		}
	}
	return toServerView(server, 0), nil
}

// storeSecret checks, seals and sets a secret on the aggregate (a mode without a secret refuses it).
func storeSecret(deps Deps, server *domain.Server, plain string, now time.Time) error {
	if err := domain.CheckSecret(plain); err != nil {
		return err
	}
	sealed, err := deps.Box.Seal(plain)
	if err != nil {
		return err
	}
	return server.SetSealedSecret(sealed, now)
}

// UpdateServerCommand replaces the editable fields of a server (a PUT: omitted timeout and daily
// limit reset to their defaults).
type UpdateServerCommand struct {
	Principal Principal
	Slug      string
	Fields    ServerFields
}

// updateFields is what the audit record of an update lists: all seven editable fields, in this
// order (the TypeScript server logged the keys of its change set, which the REST route always
// filled completely).
var updateFields = []string{"name", "description", "url", "access", "timeoutMs", "dailyLimit", "auth"}

// UpdateServer is for admins only; the contract's limits, then the server is loaded (not_found), edited
// by the domain (all or nothing; a change of the secret's destination clears the secret) and
// saved together with its events at the version it was read at (server_conflict when someone
// else saved first). Audit: server.update {fields}, then server.secret_clear when the domain
// cleared the secret. The view carries the current visible tool count.
func UpdateServer(ctx context.Context, deps Deps, cmd UpdateServerCommand) (ServerView, error) {
	if err := requireAdmin(cmd.Principal); err != nil {
		return ServerView{}, err
	}
	fields, issues := normaliseFields(cmd.Fields)
	if len(issues) > 0 {
		return ServerView{}, invalidRequest(issues[0])
	}
	server, err := load(ctx, deps, cmd.Slug)
	if err != nil {
		return ServerView{}, err
	}
	cleared, err := server.Update(domain.Changes{
		Name: domain.Some(fields.Name), Description: domain.Some(fields.Description), URL: domain.Some(fields.URL),
		Auth: domain.Some(fields.Auth), Access: domain.Some(fields.Access), TimeoutMs: domain.Some(*fields.TimeoutMs),
		DailyLimit: domain.Some(fields.DailyLimit),
	}, deps.Clock.Now())
	if err != nil {
		return ServerView{}, err
	}
	if err := deps.Servers.Save(ctx, server, server.PullEvents()); err != nil {
		return ServerView{}, err
	}
	if err := record(ctx, deps, cmd.Principal, "server.update", server.Slug(), map[string]any{"fields": append([]string{}, updateFields...)}); err != nil {
		return ServerView{}, err
	}
	if cleared {
		if err := record(ctx, deps, cmd.Principal, "server.secret_clear", server.Slug(), nil); err != nil {
			return ServerView{}, err
		}
	}
	return viewWithCount(ctx, deps, server)
}

// SetSecretCommand sets the server's secret; an unset Secret clears it.
type SetSecretCommand struct {
	Principal Principal
	Slug      string
	Secret    kernel.Secret
}

// SetSecret is for admins only; the secret must be 1..4096 characters (invalid_request) and then pass
// the domain (non-blank; the mode must hold a secret). Saved at the read version; audit
// server.secret_set or server.secret_clear, never the value. Clearing a server that has no
// secret still saves and audits.
func SetSecret(ctx context.Context, deps Deps, cmd SetSecretCommand) error {
	if err := requireAdmin(cmd.Principal); err != nil {
		return err
	}
	if cmd.Secret.IsSet() {
		if issues := ValidateSecret(cmd.Secret.Reveal()); len(issues) > 0 {
			return invalidRequest(issues[0])
		}
	}
	server, err := load(ctx, deps, cmd.Slug)
	if err != nil {
		return err
	}
	action := "server.secret_clear"
	if !cmd.Secret.IsSet() {
		server.ClearSecret(deps.Clock.Now())
	} else {
		if err := storeSecret(deps, server, cmd.Secret.Reveal(), deps.Clock.Now()); err != nil {
			return err
		}
		action = "server.secret_set"
	}
	if err := deps.Servers.Save(ctx, server, server.PullEvents()); err != nil {
		return err
	}
	return record(ctx, deps, cmd.Principal, action, server.Slug(), nil)
}

// SetStatusCommand enables or disables a server.
type SetStatusCommand struct {
	Principal Principal
	Slug      string
	Status    domain.ServerStatus
}

// SetStatus is for admins only. Only a real change is saved (with its server_changed event) and
// audited as server.status {status}; an unchanged status is a silent success.
func SetStatus(ctx context.Context, deps Deps, cmd SetStatusCommand) error {
	if err := requireAdmin(cmd.Principal); err != nil {
		return err
	}
	if cmd.Status != domain.StatusActive && cmd.Status != domain.StatusDisabled {
		return invalidRequest(Issue{Path: "status", Message: "must be active or disabled"})
	}
	server, err := load(ctx, deps, cmd.Slug)
	if err != nil {
		return err
	}
	if !server.SetStatus(cmd.Status, deps.Clock.Now()) {
		return nil
	}
	if err := deps.Servers.Save(ctx, server, server.PullEvents()); err != nil {
		return err
	}
	return record(ctx, deps, cmd.Principal, "server.status", server.Slug(), map[string]any{"status": string(cmd.Status)})
}
