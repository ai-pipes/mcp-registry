package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
)

// Operator-facing texts of rotate-secrets-key and reset-key-check (cli-commands.ts).
const (
	msgNoOldKey       = "SECRETS_KEY_OLD is not set: put the previous key there, the new one in SECRETS_KEY"
	msgYesRequired    = "--yes is required: reset-key-check removes the stored key check, so the next start records the configured SECRETS_KEY"
	msgKeyCheckGone   = "key check removed: the next start records the configured SECRETS_KEY"
	msgKeyCheckNone   = "key check: nothing to remove"
	msgNeitherKeyOpen = "neither key opens (left as they were):"
)

// keysWiring is the two key commands. They belong to no module: they hand the old and the new
// key to each module's own re-seal (the modules never see a key) and keep the platform's key
// check, which is platform.* data. The functions are fields so the command can be tested without a
// database.
type keysWiring struct {
	cfgKeys       func() (current, previous []byte, previousSet bool)
	resealServers func(ctx context.Context, from, to kernel.SecretBox) (catalog.ResealSummary, error)
	resealConns   func(ctx context.Context, from, to kernel.SecretBox) (connections.ResealSummary, error)
	resealCheck   func(ctx context.Context, k crypto.Keys) (string, error)
	removeCheck   func(ctx context.Context) (bool, error)
	audit         kernel.AuditLog
}

func newKeysWiring(rt Runtime, cat catalogWiring, con connectionsWiring) keysWiring {
	return keysWiring{
		cfgKeys: func() ([]byte, []byte, bool) {
			return rt.Config.SecretsKey.Reveal(), rt.Config.SecretsKeyOld.Reveal(), rt.Config.SecretsKeyOld.IsSet()
		},
		resealServers: cat.service.ResealServerSecrets,
		resealConns:   con.service.ResealConnections,
		resealCheck: func(ctx context.Context, k crypto.Keys) (string, error) {
			return crypto.ResealCheck(ctx, rt.Pool, k)
		},
		removeCheck: func(ctx context.Context) (bool, error) { return crypto.Remove(ctx, rt.Pool) },
		audit:       rt.Audit,
	}
}

func (w keysWiring) module() Module { return Module{Commands: w.commands()} }

// commands: the key guard is skipped for both, they handle the key themselves.
func (w keysWiring) commands() []CLICommand {
	return []CLICommand{
		{Name: "rotate-secrets-key", Usage: `  rotate-secrets-key
      Re-seal every stored secret (server secrets and people's tokens) from SECRETS_KEY_OLD
      to SECRETS_KEY. Safe to repeat.
`, Guard: GuardNone, Run: w.rotate},
		{Name: "reset-key-check", Usage: `  reset-key-check --yes
      Remove the stored key check, so the next start records the configured SECRETS_KEY. Use it
      only after restoring the right key, or when the key is lost for good (stored server secrets
      and personal connections then stay unreadable and must be entered again).
`, Guard: GuardNone, Run: w.reset},
	}
}

func resealLine(label string, resealed, current int) string {
	return fmt.Sprintf("%s: %d re-sealed, %d already under the new key", label, resealed, current)
}

// keyCheckText is what the stdout line says about the key check's verdict.
func keyCheckText(verdict string) string {
	switch verdict {
	case kernel.ResealResealed:
		return "re-sealed"
	case kernel.ResealCurrent:
		return "already current"
	case crypto.KeyCheckAbsent:
		return "none stored"
	default:
		return "cannot be read with either key"
	}
}

// interrupted turns an error caused by the command's own cancelled context into the one
// interrupt line (every step is idempotent; a repeat finishes the job).
func interrupted(ctx context.Context, err error) error {
	// The command's own context decides: a driver error need not wrap the cancel.
	if ctx.Err() != nil {
		return CommandError(msgInterrupted)
	}
	return err
}

// auditTimeout bounds the audit write of a key operation that has already changed state.
const auditTimeout = 5 * time.Second

// recordDone writes the audit record of a key operation whose state change is complete. The
// command's context no longer matters here: a cancel must not leave a removed key check or a
// finished rotation without its record, so the write runs on a context that cannot be cancelled
// (5 s). A genuine failure of the write is an ordinary error.
func (w keysWiring) recordDone(ctx context.Context, e kernel.AuditEntry) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	return w.audit.Record(actx, e)
}

// rotate is `rotate-secrets-key`: arguments, SECRETS_KEY_OLD, the two single-key boxes, the
// catalog, connections, the key check (a cancel up to here is `error: interrupted`, nothing printed
// or audited), then the audit record on a context that cannot be cancelled, and only then the
// three lines and the failures (the reference prints the lines first; ruling 2 and its amendment).
// The boxes must each hold ONE key: the process's own box also opens with the old key, as the
// "new" side it would call every old secret already current.
func (w keysWiring) rotate(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(0); err != nil {
		return err
	}
	current, old, oldSet := w.cfgKeys()
	if !oldSet {
		return CommandError(msgNoOldKey)
	}
	from, err := crypto.NewAESGCMBox(old, nil)
	if err != nil {
		return fmt.Errorf("old key box: %w", err)
	}
	to, err := crypto.NewAESGCMBox(current, nil)
	if err != nil {
		return fmt.Errorf("new key box: %w", err)
	}

	servers, err := w.resealServers(ctx, from, to)
	if err != nil {
		return interrupted(ctx, err)
	}
	conns, err := w.resealConns(ctx, from, to)
	if err != nil {
		return interrupted(ctx, err)
	}
	if ctx.Err() != nil {
		return CommandError(msgInterrupted)
	}
	verdict, err := w.resealCheck(ctx, crypto.NewKeys(current, old))
	if err != nil {
		return interrupted(ctx, err)
	}

	// The state has changed: from here the context no longer matters (see recordDone).
	// Audited also when something failed; `failed` is a count, keyCheck the raw verdict.
	if err := w.recordDone(ctx, kernel.AuditEntry{Action: "secrets.rotate", Meta: map[string]any{
		"via":         "cli",
		"servers":     map[string]any{"resealed": servers.Resealed, "current": servers.Current, "failed": len(servers.Failed)},
		"connections": map[string]any{"resealed": conns.Resealed, "current": conns.Current, "failed": len(conns.Failed)},
		"keyCheck":    verdict,
	}}); err != nil {
		return err
	}

	// The work is done and recorded: now the three lines (an interrupt never leaves a success line
	// before that point).
	out(cio, resealLine("server secrets", servers.Resealed, servers.Current))
	out(cio, resealLine("connections", conns.Resealed, conns.Current))
	out(cio, "key check: "+keyCheckText(verdict))

	var failed []string
	for _, slug := range servers.Failed {
		failed = append(failed, "server "+slug)
	}
	for _, key := range conns.Failed {
		failed = append(failed, "connection "+key)
	}
	if verdict == kernel.ResealUnreadable {
		failed = append(failed, "key check")
	}
	if len(failed) == 0 {
		return nil
	}
	return CommandError(msgNeitherKeyOpen + "\n  " + strings.Join(failed, "\n  "))
}

// reset is `reset-key-check --yes`: the parser's errors win over the missing --yes; once the
// removal has returned the audit record is written (on a context that cannot be cancelled) before
// the output, also when nothing was removed.
func (w keysWiring) reset(ctx context.Context, cio CLIIO, args []string) error {
	p, err := argSpec{Bools: []string{"yes"}}.parse(args)
	if err != nil {
		return err
	}
	if err := p.expectPositionals(0); err != nil {
		return err
	}
	if !p.flags["yes"] {
		return UsageError(msgYesRequired)
	}
	removed, err := w.removeCheck(ctx)
	if err != nil {
		return interrupted(ctx, err)
	}
	if err := w.recordDone(ctx, kernel.AuditEntry{Action: "secrets.key_check_reset", Meta: map[string]any{
		"via": "cli", "removed": removed,
	}}); err != nil {
		return err
	}
	if removed {
		out(cio, msgKeyCheckGone)
	} else {
		out(cio, msgKeyCheckNone)
	}
	return nil
}
