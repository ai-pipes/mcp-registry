package logging

import (
	"log/slog"
	"strings"
)

const redacted = "[Redacted]"

// redactedKeys are compared in lower case.
var redactedKeys = map[string]bool{
	"password":      true,
	"token":         true,
	"authorization": true,
	"cookie":        true,
}

// pinoLevel maps a slog level to pino's number.
func pinoLevel(l slog.Level) int64 {
	switch {
	case l <= LevelTrace:
		return 10
	case l <= slog.LevelDebug:
		return 20
	case l <= slog.LevelInfo:
		return 30
	case l <= slog.LevelWarn:
		return 40
	case l <= slog.LevelError:
		return 50
	default:
		return 60
	}
}

// replaceAttr turns slog's built-in time and level into pino's, and redacts secret keys.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey:
			if a.Value.Kind() == slog.KindTime {
				return slog.Int64("time", a.Value.Time().UnixMilli())
			}
			return a
		case slog.LevelKey:
			if l, ok := a.Value.Any().(slog.Level); ok {
				return slog.Int64("level", pinoLevel(l))
			}
			return a
		}
	}
	if redactedKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, redacted)
	}
	if a.Value.Kind() == slog.KindAny {
		a.Value = slog.AnyValue(scrub(a.Value.Any()))
	}
	return a
}

// scrub redacts secret keys inside maps and slices that slog marshals without calling
// replaceAttr. It returns a copy; the caller's values are untouched.
func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if redactedKeys[strings.ToLower(k)] {
				out[k] = redacted
			} else {
				out[k] = scrub(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = scrub(val)
		}
		return out
	default:
		return v
	}
}
