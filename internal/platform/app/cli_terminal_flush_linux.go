//go:build linux

package app

import "golang.org/x/sys/unix"

// discardPendingInput drops the input the terminal has received and nobody has read (TCIFLUSH).
func discardPendingInput(fd int) { _ = unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }
