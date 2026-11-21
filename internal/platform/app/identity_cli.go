package app

import (
	"context"
	"fmt"
	"io"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

// commands are the operator commands of the identity module. The usage entries are USAGE of
// cli-commands.ts; create-user follows the decided deviation create-user-keeps-role.
func (w identityWiring) commands() []CLICommand {
	return []CLICommand{
		{Name: "create-user", Usage: `  create-user --sub <sub> --name <name> [--admin | --member]
      Create (or update) a person as their first SSO login would: member, or admin with
      --admin. Idempotent by sub. An existing person keeps their role unless --admin (promote)
      or --member (demote) is given. For the smoke test and emergency access.
`, Guard: GuardWarn, Run: w.createUser},
		{Name: "create-token", Usage: `  create-token <sub> --name <name>
      Create an API token for <sub>; prints the token and the claude mcp add command.
`, Guard: GuardWarn, Run: w.createToken},
		{Name: "login-link", Usage: `  login-link <sub>
      Print a one-time sign-in link (valid 15 minutes) for an existing, enabled person. For a
      server without OIDC_*, or emergency access.
`, Guard: GuardWarn, Run: w.loginLink},
	}
}

func out(cio CLIIO, line string) { _, _ = io.WriteString(cio.Stdout, line+"\n") }

// createUser is `create-user --sub <sub> --name <name> [--admin | --member]`. The checks come in
// the reference's order: the parser, the argument count, --sub, --name; then the combination of
// the role flags (new).
func (w identityWiring) createUser(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{Strings: []string{"sub", "name"}, Bools: []string{"admin", "member"}}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(0); err != nil {
		return err
	}
	sub, err := p.required("sub")
	if err != nil {
		return err
	}
	name, err := p.required("name")
	if err != nil {
		return err
	}
	role := idapp.RoleKeep
	switch {
	case p.flags["admin"] && p.flags["member"]:
		return UsageError("--admin and --member cannot be used together")
	case p.flags["admin"]:
		role = idapp.RoleAdmin
	case p.flags["member"]:
		role = idapp.RoleMember
	}
	r, err := idapp.ProvisionUser(ctx, w.deps, idapp.ProvisionInput{Sub: sub, Name: name, Role: role})
	if err != nil {
		return err
	}
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	out(cio, fmt.Sprintf("%s %s \"%s\" (sub %s, id %s)", verb, r.Role, kernel.JSTrim(name), kernel.JSTrim(sub), r.ID))
	return nil
}

// createToken is `create-token <sub> --name <name>`: the token is printed once, then the command
// that connects Claude Code. The audit actor is the token's owner.
func (w identityWiring) createToken(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{Strings: []string{"name"}}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(1); err != nil {
		return err
	}
	name, err := p.required("name")
	if err != nil {
		return err
	}
	principal, err := idapp.ActAs(ctx, w.deps, p.positionals[0])
	if err != nil {
		return err
	}
	t, err := idapp.CreateToken(ctx, w.deps, principal, name)
	if err != nil {
		return err
	}
	out(cio, t.Token)
	out(cio, fmt.Sprintf(`claude mcp add --transport http mcp-registry %s --header "Authorization: Bearer %s"`,
		resolveURL(w.rt.Config.PublicURL, "/mcp"), t.Token))
	_, _ = io.WriteString(cio.Stderr, "The token is shown only once; store it now.\n")
	return nil
}

// loginLink is `login-link <sub>`: exactly one line on stdout, the link.
func (w identityWiring) loginLink(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(1); err != nil {
		return err
	}
	l, err := idapp.IssueLoginLink(ctx, w.deps, idapp.IssueLoginLinkInput{Sub: p.positionals[0]})
	if err != nil {
		return err
	}
	out(cio, domain.LoginLinkURL(w.rt.Config.PublicURL, l.Code))
	return nil
}
