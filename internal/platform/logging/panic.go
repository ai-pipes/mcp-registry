package logging

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// MaxPanicFrames bounds the number of frames PanicFrames returns.
const MaxPanicFrames = 16

// PanicFrames describes the call stack of a panic as "function file:line" strings, innermost
// first, at most MaxPanicFrames of them: no arguments, no panic value, none of the runtime's own
// frames, and file paths cut to start at internal/, apps/ or cmd/ (else the last two elements).
//
// Call it directly inside the deferred function that calls recover (not from a helper it calls):
// it skips exactly its own frame and that function's. Anywhere else the first frames are lost.
func PanicFrames() []string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(3, pcs)
	it := runtime.CallersFrames(pcs[:n])
	var out []string
	for len(out) < MaxPanicFrames {
		f, more := it.Next()
		if f.Function != "" && !strings.HasPrefix(f.Function, "runtime.") {
			out = append(out, f.Function+" "+shortPath(f.File)+":"+strconv.Itoa(f.Line))
		}
		if !more {
			break
		}
	}
	return out
}

// shortPath drops the build directory.
func shortPath(file string) string {
	file = filepath.ToSlash(file)
	for _, marker := range []string{"/internal/", "/apps/", "/cmd/"} {
		if i := strings.LastIndex(file, marker); i >= 0 {
			return file[i+1:]
		}
	}
	parts := strings.Split(file, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// PanicAttr describes a recovered panic value as the attribute "err" with the same {type, label}
// shape ErrAttr gives an error, never with the value's text (it may quote values): for an error
// value it is ErrAttr; for any other value type and label are its Go type.
func PanicAttr(v any) slog.Attr {
	if err, ok := v.(error); ok {
		return ErrAttr(err)
	}
	typ := fmt.Sprintf("%T", v)
	return slog.Group("err", slog.String("type", typ), slog.String("label", typ))
}
