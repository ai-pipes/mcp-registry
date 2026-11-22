//go:build darwin

package app

import "golang.org/x/sys/unix"

// freadFlag is FREAD of <sys/fcntl.h> (absent from x/sys): TIOCFLUSH then drops the input queue.
const freadFlag = 0x1

// discardPendingInput drops the input the terminal has received and nobody has read.
func discardPendingInput(fd int) { _ = unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, freadFlag) }
