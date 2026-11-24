package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/platform/crypto"
)

type recordingAudit struct{ entries []kernel.AuditEntry }

func (a *recordingAudit) Record(_ context.Context, e kernel.AuditEntry) error {
	a.entries = append(a.entries, e)
	return nil
}

// fakeKeys is a keysWiring over fakes, with the keys of a rotation configured.
func fakeKeys(oldSet bool) (keysWiring, *recordingAudit, []byte, []byte) {
	cur, old := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	a := &recordingAudit{}
	w := keysWiring{
		cfgKeys: func() ([]byte, []byte, bool) {
			if !oldSet {
				return cur, nil, false
			}
			return cur, old, true
		},
		resealServers: func(context.Context, kernel.SecretBox, kernel.SecretBox) (catalog.ResealSummary, error) {
			return catalog.ResealSummary{Failed: []string{}}, nil
		},
		resealConns: func(context.Context, kernel.SecretBox, kernel.SecretBox) (connections.ResealSummary, error) {
			return connections.ResealSummary{Failed: []string{}}, nil
		},
		resealCheck: func(context.Context, crypto.Keys) (string, error) { return crypto.KeyCheckAbsent, nil },
		removeCheck: func(context.Context) (bool, error) { return false, nil },
		audit:       a,
	}
	return w, a, cur, old
}

func runKeysCmd(ctx context.Context, t *testing.T, w keysWiring, name string, args ...string) (stdout string, err error) {
	t.Helper()
	var out bytes.Buffer
	for _, c := range w.commands() {
		if c.Name == name {
			err = c.Run(ctx, CLIIO{Stdout: &out, Stderr: &bytes.Buffer{}}, args)
			return out.String(), err
		}
	}
	t.Fatalf("no command %s", name)
	return "", nil
}

func TestKeyCommandsSkipTheGuardAndHaveTheirUsage(t *testing.T) {
	w, _, _, _ := fakeKeys(true)
	got := map[string]CLICommand{}
	for _, c := range w.commands() {
		got[c.Name] = c
	}
	if len(got) != 2 {
		t.Fatalf("%d commands", len(got))
	}
	for _, name := range []string{"rotate-secrets-key", "reset-key-check"} {
		c := got[name]
		if c.Guard != GuardNone || c.Run == nil || !strings.HasPrefix(c.Usage, "  "+name) {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if !strings.Contains(got["reset-key-check"].Usage, "reset-key-check --yes\n") {
		t.Error("the reset usage names --yes")
	}
}

// Trap 1: the re-seal runs between two boxes that each hold ONE key. The process's own box also
// opens with the old key; used as the "new" side it makes every old secret "already current".
func TestRotateHandsTheModulesTwoSingleKeyBoxes(t *testing.T) {
	w, _, cur, old := fakeKeys(true)
	oldBox, _ := crypto.NewAESGCMBox(old, nil)
	sealedOld, err := oldBox.Seal("plain")
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	check := func(from, to kernel.SecretBox) {
		seen++
		if p, err := from.Open(sealedOld); err != nil || p != "plain" {
			t.Errorf("from does not open what the old key sealed")
		}
		if _, err := to.Open(sealedOld); err == nil {
			t.Error("to opens a value of the OLD key: it is not a single-key box (everything would be already current)")
		}
		fresh, err := to.Seal("x")
		if err != nil {
			t.Fatal(err)
		}
		curBox, _ := crypto.NewAESGCMBox(cur, nil)
		if _, err := curBox.Open(fresh); err != nil {
			t.Error("to does not seal with the new key")
		}
		if _, err := from.Open(fresh); err == nil {
			t.Error("from opens a value of the NEW key: it is not a single-key box")
		}
	}
	w.resealServers = func(_ context.Context, f, to kernel.SecretBox) (catalog.ResealSummary, error) {
		check(f, to)
		return catalog.ResealSummary{Failed: []string{}}, nil
	}
	w.resealConns = func(_ context.Context, f, to kernel.SecretBox) (connections.ResealSummary, error) {
		check(f, to)
		return connections.ResealSummary{Failed: []string{}}, nil
	}
	if _, err := runKeysCmd(context.Background(), t, w, "rotate-secrets-key"); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Errorf("the boxes were checked %d times", seen)
	}
}

func TestRotateOrderOutputAndAudit(t *testing.T) {
	w, audit, _, _ := fakeKeys(true)
	w.resealServers = func(context.Context, kernel.SecretBox, kernel.SecretBox) (catalog.ResealSummary, error) {
		return catalog.ResealSummary{Resealed: 2, Current: 1, Failed: []string{"a", "b"}}, nil
	}
	w.resealConns = func(context.Context, kernel.SecretBox, kernel.SecretBox) (connections.ResealSummary, error) {
		return connections.ResealSummary{Resealed: 0, Current: 4, Failed: []string{"u1/s1"}}, nil
	}
	w.resealCheck = func(context.Context, crypto.Keys) (string, error) { return kernel.ResealUnreadable, nil }
	stdout, err := runKeysCmd(context.Background(), t, w, "rotate-secrets-key")
	want := "server secrets: 2 re-sealed, 1 already under the new key\nconnections: 0 re-sealed, 4 already under the new key\nkey check: cannot be read with either key\n"
	if stdout != want {
		t.Errorf("stdout %q", stdout)
	}
	var ce *CommandErr
	if !errors.As(err, &ce) || ce.Msg != "neither key opens (left as they were):\n  server a\n  server b\n  connection u1/s1\n  key check" {
		t.Errorf("%v", err)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("%d audit entries", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "secrets.rotate" || e.ActorID != nil || e.Target != nil {
		t.Errorf("%+v", e)
	}
	srv := e.Meta["servers"].(map[string]any)
	con := e.Meta["connections"].(map[string]any)
	if e.Meta["via"] != "cli" || e.Meta["keyCheck"] != "unreadable" || srv["failed"] != 2 || srv["resealed"] != 2 || srv["current"] != 1 ||
		con["failed"] != 1 || con["resealed"] != 0 || con["current"] != 4 {
		t.Errorf("%+v", e.Meta)
	}
}

func TestRotateKeyCheckLines(t *testing.T) {
	for verdict, text := range map[string]string{
		kernel.ResealResealed: "re-sealed", kernel.ResealCurrent: "already current",
		crypto.KeyCheckAbsent: "none stored", kernel.ResealUnreadable: "cannot be read with either key",
	} {
		w, audit, _, _ := fakeKeys(true)
		w.resealCheck = func(context.Context, crypto.Keys) (string, error) { return verdict, nil }
		stdout, _ := runKeysCmd(context.Background(), t, w, "rotate-secrets-key")
		if !strings.HasSuffix(stdout, "key check: "+text+"\n") || audit.entries[0].Meta["keyCheck"] != verdict {
			t.Errorf("%s: %q %v", verdict, stdout, audit.entries)
		}
	}
}

// Q2 of the inventory: a key check whose compare-and-set was lost is reported as current.
func TestRotateLostKeyCheckSwapIsAlreadyCurrent(t *testing.T) {
	w, audit, _, _ := fakeKeys(true)
	w.resealCheck = func(context.Context, crypto.Keys) (string, error) { return kernel.ResealCurrent, nil }
	stdout, err := runKeysCmd(context.Background(), t, w, "rotate-secrets-key")
	if err != nil || !strings.HasSuffix(stdout, "key check: already current\n") || audit.entries[0].Meta["keyCheck"] != "current" {
		t.Errorf("%v %q %v", err, stdout, audit.entries)
	}
}

func TestRotateWithoutTheOldKeyIsACommandErrorBeforeAnyWork(t *testing.T) {
	w, audit, _, _ := fakeKeys(false)
	w.resealServers = func(context.Context, kernel.SecretBox, kernel.SecretBox) (catalog.ResealSummary, error) {
		t.Error("the catalog was touched")
		return catalog.ResealSummary{}, nil
	}
	stdout, err := runKeysCmd(context.Background(), t, w, "rotate-secrets-key")
	var ce *CommandErr
	if !errors.As(err, &ce) || ce.Msg != "SECRETS_KEY_OLD is not set: put the previous key there, the new one in SECRETS_KEY" || stdout != "" || len(audit.entries) != 0 {
		t.Errorf("%v %q %v", err, stdout, audit.entries)
	}
}

// The arguments are judged first: a positional is a usage error even without SECRETS_KEY_OLD.
func TestRotateParsesBeforeLookingAtTheKey(t *testing.T) {
	w, audit, _, _ := fakeKeys(false)
	for args, msg := range map[string]string{
		"x":       "expected 0 argument(s), got 1",
		"--other": "Unknown option '--other'",
	} {
		_, err := runKeysCmd(context.Background(), t, w, "rotate-secrets-key", args)
		var ue *UsageErr
		if !errors.As(err, &ue) || !strings.HasPrefix(ue.Msg, msg) {
			t.Errorf("%s: %v", args, err)
		}
	}
	if len(audit.entries) != 0 {
		t.Error("audited")
	}
}

// An interrupt before the key check step has completed: one line, no success lines, no audit.
func TestRotateInterrupted(t *testing.T) {
	for name, edit := range map[string]func(*keysWiring, context.CancelFunc){
		"catalog": func(w *keysWiring, cancel context.CancelFunc) {
			w.resealServers = func(ctx context.Context, _, _ kernel.SecretBox) (catalog.ResealSummary, error) {
				cancel()
				return catalog.ResealSummary{}, ctx.Err()
			}
		},
		"connections": func(w *keysWiring, cancel context.CancelFunc) {
			w.resealConns = func(ctx context.Context, _, _ kernel.SecretBox) (connections.ResealSummary, error) {
				cancel()
				return connections.ResealSummary{}, errors.Join(errors.New("lock"), ctx.Err())
			}
		},
		"cancelled between the steps": func(w *keysWiring, cancel context.CancelFunc) {
			w.resealConns = func(_ context.Context, _, _ kernel.SecretBox) (connections.ResealSummary, error) {
				cancel()
				return connections.ResealSummary{Failed: []string{}}, nil
			}
		},
		"an error that does not wrap the cancel": func(w *keysWiring, cancel context.CancelFunc) {
			w.resealCheck = func(context.Context, crypto.Keys) (string, error) {
				cancel()
				return "", errors.New("conn closed")
			}
		},
		"key check": func(w *keysWiring, cancel context.CancelFunc) {
			w.resealCheck = func(ctx context.Context, _ crypto.Keys) (string, error) {
				cancel()
				return "", ctx.Err()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, audit, _, _ := fakeKeys(true)
			ctx, cancel := context.WithCancel(context.Background())
			edit(&w, cancel)
			stdout, err := runKeysCmd(ctx, t, w, "rotate-secrets-key")
			var ce *CommandErr
			if !errors.As(err, &ce) || ce.Msg != "interrupted" || len(audit.entries) != 0 {
				t.Errorf("%v %v", err, audit.entries)
			}
			if ce != nil && ce.Msg != "interrupted" || stdout != "" {
				t.Errorf("success lines %q", stdout)
			}
		})
	}
}

// Once the key check step has returned, the state has changed: the audit is written on a context
// that cannot be cancelled and the command reports normally.
func TestRotateAuditSurvivesACancel(t *testing.T) {
	for name, edit := range map[string]func(*keysWiring, context.CancelFunc, *bool){
		"cancelled during the audit write": func(w *keysWiring, cancel context.CancelFunc, seen *bool) {
			w.audit = auditErr(func(ctx context.Context) error { cancel(); *seen = true; return ctx.Err() })
		},
		"cancelled after the audit write": func(w *keysWiring, cancel context.CancelFunc, seen *bool) {
			w.audit = auditErr(func(ctx context.Context) error { *seen = true; err := ctx.Err(); cancel(); return err })
		},
		"cancelled as the key check returned": func(w *keysWiring, cancel context.CancelFunc, seen *bool) {
			w.resealCheck = func(context.Context, crypto.Keys) (string, error) { cancel(); return crypto.KeyCheckAbsent, nil }
			w.audit = auditErr(func(ctx context.Context) error { *seen = true; return ctx.Err() })
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, _, _, _ := fakeKeys(true)
			ctx, cancel := context.WithCancel(context.Background())
			seen := false
			edit(&w, cancel, &seen)
			stdout, err := runKeysCmd(ctx, t, w, "rotate-secrets-key")
			if err != nil || !seen || !strings.Contains(stdout, "key check: none stored") || !strings.Contains(stdout, "connections:") {
				t.Errorf("err %v audited %v stdout %q", err, seen, stdout)
			}
		})
	}
}

func TestResetParsesBeforeRequiringYes(t *testing.T) {
	w, audit, _, _ := fakeKeys(true)
	w.removeCheck = func(context.Context) (bool, error) { t.Error("removed"); return true, nil }
	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{nil, "--yes is required: reset-key-check removes the stored key check, so the next start records the configured SECRETS_KEY"},
		{[]string{"x"}, "expected 0 argument(s), got 1"},
		{[]string{"x", "--yes"}, "expected 0 argument(s), got 1"},
		{[]string{"--yes=1"}, "Option '--yes' does not take an argument"},
		{[]string{"--no"}, "Unknown option '--no'"},
	} {
		_, err := runKeysCmd(context.Background(), t, w, "reset-key-check", tc.args...)
		var ue *UsageErr
		if !errors.As(err, &ue) || !strings.HasPrefix(ue.Msg, tc.msg) {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
	if len(audit.entries) != 0 {
		t.Error("audited")
	}
}

func TestResetAuditsBeforeThePrintAlsoWhenNothingWasRemoved(t *testing.T) {
	for _, removed := range []bool{true, false} {
		w, audit, _, _ := fakeKeys(true)
		w.removeCheck = func(context.Context) (bool, error) { return removed, nil }
		var out bytes.Buffer
		w.audit = auditFunc(func(e kernel.AuditEntry) {
			if out.Len() != 0 {
				t.Error("the output came before the audit record")
			}
			audit.entries = append(audit.entries, e)
		})
		c := w.commands()[1]
		if err := c.Run(context.Background(), CLIIO{Stdout: &out}, []string{"--yes"}); err != nil {
			t.Fatal(err)
		}
		want := "key check: nothing to remove\n"
		if removed {
			want = "key check removed: the next start records the configured SECRETS_KEY\n"
		}
		e := audit.entries[0]
		if out.String() != want || e.Action != "secrets.key_check_reset" || e.ActorID != nil || e.Target != nil ||
			e.Meta["via"] != "cli" || e.Meta["removed"] != removed || len(e.Meta) != 2 {
			t.Errorf("%q %+v", out.String(), e)
		}
	}
}

type auditErr func(ctx context.Context) error

func (f auditErr) Record(ctx context.Context, _ kernel.AuditEntry) error { return f(ctx) }

type auditFunc func(kernel.AuditEntry)

func (f auditFunc) Record(_ context.Context, e kernel.AuditEntry) error { f(e); return nil }

// The removal has returned: the audit is written on a context that cannot be cancelled and the
// command reports normally, whenever the cancel arrives.
func TestResetAuditSurvivesACancel(t *testing.T) {
	for name, after := range map[string]bool{"during the audit write": false, "after the audit write": true} {
		t.Run(name, func(t *testing.T) {
			w, _, _, _ := fakeKeys(true)
			ctx, cancel := context.WithCancel(context.Background())
			w.removeCheck = func(context.Context) (bool, error) { return true, nil }
			seen := false
			w.audit = auditErr(func(ctx context.Context) error {
				if !after {
					cancel()
				}
				seen = true
				err := ctx.Err()
				if after {
					cancel()
				}
				return err
			})
			stdout, err := runKeysCmd(ctx, t, w, "reset-key-check", "--yes")
			if err != nil || !seen || !strings.Contains(stdout, "key check removed") {
				t.Errorf("err %v audited %v stdout %q", err, seen, stdout)
			}
		})
	}
}

func TestResetInterrupted(t *testing.T) {
	w, audit, _, _ := fakeKeys(true)
	ctx, cancel := context.WithCancel(context.Background())
	w.removeCheck = func(ctx context.Context) (bool, error) { cancel(); return false, ctx.Err() }
	stdout, err := runKeysCmd(ctx, t, w, "reset-key-check", "--yes")
	var ce *CommandErr
	if !errors.As(err, &ce) || ce.Msg != "interrupted" || stdout != "" || len(audit.entries) != 0 {
		t.Errorf("%v %q", err, stdout)
	}
}
