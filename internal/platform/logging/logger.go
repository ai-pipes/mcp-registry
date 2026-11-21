// Package logging writes pino-compatible JSON log lines on top of log/slog.
//
// Attribute keys: a top-level "level" or "time" given by the caller is renamed to "fields.level"
// or "fields.time" so the built-in keys stay unique. "msg" cannot be told apart from the message
// in ReplaceAttr, so it must not be used as an attribute key.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

// ErrAttr describes an error without its message, which may quote values: "type" is the part of
// kernel.ErrorLabel before ": " (for a wrapped error that carries a code, the coded error's type,
// not the outer one) and "label" is the whole kernel.ErrorLabel. Any other attribute whose value
// is an error is written the same way.
func ErrAttr(err error) slog.Attr { return errAttr("err", err) }

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
