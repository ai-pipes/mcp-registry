package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	cathttp "github.com/ai-pipes/mcp-registry/internal/modules/catalog/http"
	idapp "github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
)

// msgFirstSyncFailed is the stderr line of the decided deviation create-server-sync-exit: the
// reference exits 0 when the server is created but its first synchronisation fails; this server
// exits 1. The reference's report (the created line, which carries `sync failed: <text>`) is
// still on stdout.
const msgFirstSyncFailed = "the server was created, but its first sync failed"

// commands are the operator commands of the catalog (create-server and sync of cli-commands.ts).
// create-server is the only command that seals a new secret, so a key mismatch refuses it.
func (w catalogWiring) commands() []CLICommand {
	return []CLICommand{
		{Name: "create-server", Usage: `  create-server --json <file> --as <admin-sub> [--secret-env <VAR>]
      Create a server from a ServerInput JSON file and synchronise it at once. The secret
      (shared key or OAuth client secret) is read from the variable <VAR> or from stdin,
      never from the command line, and is never printed.
`, Guard: GuardRefuse, Run: w.createServer},
		{Name: "sync", Usage: `  sync <slug> [--as <admin-sub>]
      Synchronise the server's tools; without --as, as the scheduler does.
`, Guard: GuardWarn, Run: w.syncServer},
	}
}

// readServerInput is readServerInput of cli-commands.ts: the file, parsed with the schema of the
// REST body (without a secret), every issue reported in schema order.
func readServerInput(file string) (catalog.ServerInput, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // G304: the operator names the file on purpose
	if err != nil {
		return catalog.ServerInput{}, CommandError("cannot read " + file)
	}
	in, issues, err := cathttp.ParseServerFile(raw)
	if err != nil {
		return catalog.ServerInput{}, CommandError(file + " is not valid JSON")
	}
	if len(issues) > 0 {
		lines := make([]string, len(issues))
		for i, is := range issues {
			path := is.Path
			if path == "" {
				path = "(root)"
			}
			lines[i] = path + ": " + is.Message
		}
		return catalog.ServerInput{}, CommandError(file + " is not a valid server:\n  " + strings.Join(lines, "\n  "))
	}
	return in, nil
}

// createServer is `create-server --json <file> --as <sub> [--secret-env VAR]`, in the reference's
// order: the arguments, the file (judged completely before the person is looked up), the person,
// the secret, then registerServer (create and the first sync). The audit actor is the --as person.
func (w catalogWiring) createServer(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{Strings: []string{"json", "as", "secret-env"}}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(0); err != nil {
		return err
	}
	file, err := p.required("json")
	if err != nil {
		return err
	}
	sub, err := p.required("as")
	if err != nil {
		return err
	}
	in, err := readServerInput(file)
	if err != nil {
		return err
	}
	principal, err := idapp.ActAs(ctx, w.id.deps, sub)
	if err != nil {
		return err
	}
	var secretEnv *string
	if v, ok := p.values["secret-env"]; ok {
		secretEnv = &v
	}
	secret, err := readSecret(cio, secretEnv, in.Fields.Auth)
	if err != nil {
		return err
	}
	// A signal does not cut the creation and its audit records in half.
	view, err := w.service.RegisterServer(context.WithoutCancel(ctx), catalog.CreateServerCommand{
		Principal: principal, Input: in, Secret: secret,
	})
	if err != nil {
		return err
	}
	mode := string(view.Auth.Kind)
	if view.SecretSet {
		mode += ", secret set"
	}
	report := fmt.Sprintf("%d tool(s)", view.ToolCount)
	if view.LastSyncError != nil {
		report = "sync failed: " + *view.LastSyncError
	}
	out(cio, fmt.Sprintf("created server \"%s\" (%s); %s", view.Slug, mode, report))
	if view.LastSyncError != nil {
		return CommandError(msgFirstSyncFailed)
	}
	return nil
}

// syncServer is `sync <slug> [--as <sub>]`: without --as the principal is nil, as the scheduler
// runs it (no audit record, no change of the sync owner).
func (w catalogWiring) syncServer(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{Strings: []string{"as"}}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(1); err != nil {
		return err
	}
	var principal *catalog.Principal
	if sub, ok := p.values["as"]; ok {
		pr, err := idapp.ActAs(ctx, w.id.deps, sub)
		if err != nil {
			return err
		}
		principal = &pr
	}
	slug := p.positionals[0]
	counts, err := w.service.SyncServer(context.WithoutCancel(ctx), catalog.SyncCommand{Principal: principal, Slug: slug})
	if err != nil {
		return err
	}
	out(cio, fmt.Sprintf("synced \"%s\": %d added, %d changed, %d removed", slug, counts.Added, counts.Changed, counts.Removed))
	return nil
}
