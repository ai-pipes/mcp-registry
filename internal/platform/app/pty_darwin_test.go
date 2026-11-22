//go:build darwin

package app

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const getTermios = unix.TIOCGETA

// openPTY is a pseudo-terminal pair: what is written to master is typed at slave. The sequence is
// posix_openpt, grantpt, unlockpt, ptsname of libc, as ioctls.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	fd := m.Fd()
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCPTYGRANT, 0); e != 0 { //nolint:staticcheck // SA1019: x/sys has no other ioctl on darwin that takes a buffer or no argument
		t.Fatalf("grantpt: %v", e)
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCPTYUNLK, 0); e != 0 { //nolint:staticcheck // SA1019: x/sys has no other ioctl on darwin that takes a buffer or no argument
		t.Fatalf("unlockpt: %v", e)
	}
	var name [128]byte
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 { //nolint:staticcheck,gosec // SA1019, G103: the buffer is the ioctl argument; x/sys has no other ioctl on darwin that takes a buffer or no argument
		t.Fatalf("ptsname: %v", e)
	}
	s, err := os.OpenFile(string(name[:bytes.IndexByte(name[:], 0)]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}
