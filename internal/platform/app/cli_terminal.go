package app

import (
	"errors"
	"io"
	"os"

	"golang.org/x/term"
)

// fdTerminal reads from a terminal's file descriptor with echo off.
type fdTerminal struct{ fd int }

func (t fdTerminal) ReadSecretLine() (string, error) {
	line, err := term.ReadPassword(t.fd)
	if errors.Is(err, io.EOF) { // Ctrl-D: the reference's readline closes and answers ''
		return string(line), nil
	}
	return string(line), err
}

// terminalOf is the Terminal behind stdin, or nil when stdin is not a terminal (a pipe, a file,
// a test reader).
func terminalOf(stdin io.Reader) Terminal {
	f, ok := stdin.(*os.File)
	if !ok {
		return nil
	}
	fd := int(f.Fd()) //nolint:gosec // G115: a descriptor is a small non-negative number
	if !term.IsTerminal(fd) {
		return nil
	}
	return fdTerminal{fd: fd}
}
