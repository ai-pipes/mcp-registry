// Package logging writes pino-compatible JSON log lines on top of log/slog.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// Levels pino has and slog lacks.
const (
	LevelTrace slog.Level = -8
	LevelFatal slog.Level = 12
)

// levelSilent is above every level a record can have.
const levelSilent slog.Level = 1 << 20

// New returns a logger that writes one JSON object per line to w. level is a pino level name
// (trace, debug, info, warn, error, fatal) or "silent"; an unknown name means info.
func New(level string, w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       parseLevel(level),
		ReplaceAttr: replaceAttr,
	}))
}

// Fatal writes a record at level fatal (60). It never ends the process: that is the caller's job.
func Fatal(l *slog.Logger, msg string, args ...any) {
	l.Log(context.Background(), LevelFatal, msg, args...)
}

// ErrAttr describes an error without its message, which may quote values: the dynamic type name
// and kernel.ErrorLabel (type name plus a short code when the error carries one).
func ErrAttr(err error) slog.Attr {
	label := kernel.ErrorLabel(err)
	typ, _, _ := strings.Cut(label, ": ")
	return slog.Group("err", slog.String("type", typ), slog.String("label", label))
}

func parseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "fatal":
		return LevelFatal
	case "silent":
		return levelSilent
	default:
		return slog.LevelInfo
	}
}
