//go:build darwin || linux

package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These tests read a secret through a real pseudo-terminal: the slave is the process's terminal,
// the test types on the master.

type readResult struct {
	line string
	err  error
}

// lflags is the local-mode word of the terminal (ECHO, ICANON, ISIG...).
func lflags(t *testing.T, f *os.File) uint64 {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), getTermios) //nolint:gosec // G115: a descriptor is a small non-negative number
	if err != nil {
		t.Fatal(err)
	}
	return uint64(tio.Lflag)
}

// typing starts a read on a fresh terminal and returns once the terminal has left the line mode,
// so that what the test types next is the read's input and nothing is echoed. A deadline bounds
// every wait.
type typing struct {
	master, slave *os.File
	res           chan readResult
	before        uint64
	cancel        context.CancelFunc
}

const patience = 10 * time.Second

func startTyping(t *testing.T) *typing {
	t.Helper()
	master, slave := openPTY(t)
	// The master goes first so that a read left behind sees the end of input and ends.
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ty := &typing{master: master, slave: slave, res: make(chan readResult, 1), before: lflags(t, slave), cancel: cancel}
	if ty.before&unix.ECHO == 0 || ty.before&unix.ICANON == 0 {
		t.Fatalf("a new terminal echoes and edits lines: %#x", ty.before)
	}
	go func() {
		line, err := fdTerminal{in: slave}.ReadSecretLine(ctx)
		ty.res <- readResult{line, err}
	}()
	deadline := time.NewTimer(patience)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for lflags(t, slave)&unix.ICANON != 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("the read never took the terminal")
		}
	}
	if got := lflags(t, slave); got&unix.ECHO != 0 {
		t.Fatalf("echo is on while the secret is typed: %#x", got)
	}
	return ty
}

func (ty *typing) typeKeys(t *testing.T, keys string) {
	t.Helper()
	if _, err := ty.master.WriteString(keys); err != nil {
		t.Fatal(err)
	}
}

func (ty *typing) result(t *testing.T) readResult {
	t.Helper()
	select {
	case r := <-ty.res:
		return r
	case <-time.After(patience):
		t.Fatal("the read did not return")
		return readResult{}
	}
}

func (ty *typing) restored(t *testing.T) {
	t.Helper()
	if got := lflags(t, ty.slave); got != ty.before {
		t.Errorf("the terminal state is not restored: %#x, was %#x", got, ty.before)
	}
}

func TestATypedLineIsReadWithoutEchoAndTheTerminalIsRestored(t *testing.T) {
	ty := startTyping(t)
	ty.typeKeys(t, "hunx\x7fter2\r") // a backspace edits
	if r := ty.result(t); r.err != nil || r.line != "hunter2" {
		t.Fatalf("%+v", r)
	}
	ty.restored(t)
}

func TestEnterAsNewlineAlsoEndsTheLine(t *testing.T) {
	ty := startTyping(t)
	ty.typeKeys(t, "abc\n")
	if r := ty.result(t); r.err != nil || r.line != "abc" {
		t.Fatalf("%+v", r)
	}
	ty.restored(t)
}

func TestCtrlDOnAnEmptyLineIsNoSecret(t *testing.T) {
	ty := startTyping(t)
	ty.typeKeys(t, "\x04")
	if r := ty.result(t); r.err != nil || r.line != "" {
		t.Fatalf("%+v", r)
	}
	ty.restored(t)
}

func TestCtrlDInsideALineIsIgnored(t *testing.T) {
	ty := startTyping(t)
	ty.typeKeys(t, "ab\x04c\r")
	if r := ty.result(t); r.err != nil || r.line != "abc" {
		t.Fatalf("%+v", r)
	}
}

func TestCtrlCAbortsTheRead(t *testing.T) {
	ty := startTyping(t)
	ty.typeKeys(t, "ab\x03")
	if r := ty.result(t); !errors.Is(r.err, errInterrupted) || r.line != "" {
		t.Fatalf("%+v", r)
	}
	ty.restored(t)
}

func TestACancelledContextEndsTheReadAndRestoresTheTerminal(t *testing.T) {
	ty := startTyping(t)
	ty.typeKeys(t, "half")
	ty.cancel()
	r := ty.result(t)
	if !errors.Is(r.err, context.Canceled) || r.line != "" {
		t.Fatalf("%+v: the partial line must not be returned", r)
	}
	ty.restored(t)
}

func TestAContextCancelledBeforeTheReadDoesNotTouchTheTerminal(t *testing.T) {
	master, slave := openPTY(t)
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	before := lflags(t, slave)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (fdTerminal{in: slave}).ReadSecretLine(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if lflags(t, slave) != before {
		t.Error("the terminal was changed")
	}
}

func TestAPseudoTerminalIsATerminal(t *testing.T) {
	master, slave := openPTY(t)
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	if terminalOf(slave) == nil {
		t.Error("a pty slave is a terminal")
	}
}
