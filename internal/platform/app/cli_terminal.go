package app

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"unicode/utf8"

	"golang.org/x/term"
)

// fdTerminal reads a line from a terminal with echo off. The terminal is put in raw mode for the
// read: the program sees every key itself, so Ctrl-C (0x03) and Ctrl-D (0x04) are bytes, not
// signals or the kernel's end-of-file, and the same loop serves both. The state saved before is
// restored on every way out, including a cancelled context.
type fdTerminal struct{ in *os.File }

func (t fdTerminal) ReadSecretLine(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(t.in.Fd()) //nolint:gosec // G115: a descriptor is a small non-negative number
	saved, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	restore := sync.OnceFunc(func() { _ = term.Restore(fd, saved) })
	defer restore()

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readRawLine(t.in)
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		// The read is left behind, blocked on the terminal; the command is ending. Whatever it
		// had collected is dropped.
		return "", ctx.Err()
	}
}

// readRawLine reads keys from a terminal in raw mode until Enter. Backspace (0x7f, 0x08) removes
// the last character, Ctrl-U the whole line, Ctrl-D on an empty line (or the end of input) ends
// with an empty line and is ignored inside a line, Ctrl-C is errInterrupted.
func readRawLine(r io.Reader) (string, error) {
	var line []byte
	var key [1]byte
	for {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			if errors.Is(err, io.EOF) { // the terminal went away: what was typed is not a secret
				return "", nil
			}
			return "", err
		}
		switch c := key[0]; c {
		case '\r', '\n':
			return string(line), nil
		case 0x03:
			return "", errInterrupted
		case 0x04:
			if len(line) == 0 {
				return "", nil
			}
		case 0x7f, 0x08:
			if len(line) > 0 {
				_, size := utf8.DecodeLastRune(line)
				line = line[:len(line)-size]
			}
		case 0x15:
			line = line[:0]
		default:
			line = append(line, c)
		}
	}
}

// terminalOf is the Terminal behind stdin, or nil when stdin is not a terminal (a pipe, a file,
// a test reader).
func terminalOf(stdin io.Reader) Terminal {
	f, ok := stdin.(*os.File)
	if !ok {
		return nil
	}
	if !term.IsTerminal(int(f.Fd())) { //nolint:gosec // G115: a descriptor is a small non-negative number
		return nil
	}
	return fdTerminal{in: f}
}
