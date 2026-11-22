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
	// Input typed after the line (the rest of a multi-line paste) is thrown away before the
	// terminal is given back, or the shell would run it.
	restore := sync.OnceFunc(func() { discardPendingInput(fd); _ = term.Restore(fd, saved) })
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

// keyState is where the key reader is inside an escape sequence.
type keyState int

const (
	keyPlain keyState = iota // ordinary input
	keyEsc                   // after ESC: the next byte says what follows
	keyCSI                   // after ESC [ : parameters, then a final byte 0x40-0x7E
	keySS3                   // after ESC O : one more byte
)

// readRawLine reads keys from a terminal in raw mode until Enter. Backspace (0x7f, 0x08) removes
// the last character, Ctrl-U the whole line, Ctrl-D on an empty line (or the end of input) ends
// with an empty line and is ignored inside a line, Ctrl-C is errInterrupted. Every other control
// byte is dropped, and escape sequences (arrows, Home/End, function keys, the bracketed-paste
// markers ESC [ 200 ~ and ESC [ 201 ~) are swallowed whole, wherever the reads split them, so a
// key that is not a character never becomes part of the secret; the text between the paste markers
// is kept. A lone ESC takes the byte after it with it. A control byte inside a sequence ends the
// sequence and is then handled as typed (so Ctrl-C always interrupts).
func readRawLine(r io.Reader) (string, error) {
	var line []byte
	var key [1]byte
	state := keyPlain
	for {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			if errors.Is(err, io.EOF) { // the terminal went away: what was typed is not a secret
				return "", nil
			}
			return "", err
		}
		c := key[0]
		switch state {
		case keyEsc:
			switch {
			case c == '[':
				state = keyCSI
				continue
			case c == 'O':
				state = keySS3
				continue
			case c == 0x1b:
				continue // ESC ESC: still waiting for what follows
			case c >= 0x20 && c != 0x7f:
				state = keyPlain
				continue // the byte of a two-byte sequence
			}
			state = keyPlain // a control byte: typed as it is
		case keyCSI:
			if c >= 0x20 && c < 0x40 { // parameter and intermediate bytes
				continue
			}
			state = keyPlain
			if c >= 0x40 && c <= 0x7e { // the final byte
				continue
			}
		case keySS3:
			state = keyPlain
			if c >= 0x20 && c != 0x7f {
				continue
			}
		}
		switch {
		case c == 0x1b:
			state = keyEsc
		case c == '\r' || c == '\n':
			return string(line), nil
		case c == 0x03:
			return "", errInterrupted
		case c == 0x04:
			if len(line) == 0 {
				return "", nil
			}
		case c == 0x7f || c == 0x08:
			if len(line) > 0 {
				_, size := utf8.DecodeLastRune(line)
				line = line[:len(line)-size]
			}
		case c == 0x15:
			line = line[:0]
		case c < 0x20:
			// Tab, Ctrl-W, Ctrl-A...: not characters.
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
