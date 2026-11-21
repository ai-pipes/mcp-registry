package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const (
	redacted   = "[Redacted]"
	unloggable = "[Unloggable]"
)

// exactSecretKeys are secret field names on their own (compared after normalisation).
var exactSecretKeys = map[string]bool{
	"password":      true,
	"token":         true,
	"authorization": true,
	"cookie":        true,
	"setcookie":     true,
	"secret":        true,
	// Names for credentials that are not worded as a password, token, secret or key. The bare
	// words "code" and "state" are deliberately absent: "code" is an ordinary field of a sync
	// record, and neither is secret by itself.
	"credentials":      true,
	"bearer":           true,
	"dsn":              true,
	"databaseurl":      true,
	"connectionstring": true,
	"codeverifier":     true,
	"nonce":            true,
	"sessionid":        true,
	"sid":              true,
}

// secretSuffixes mark a compound name as secret: clientSecret, client_secret, accessToken,
// refresh_token, X-Api-Key, secretsKey, dbPassword. The bare word "key" is deliberately not
// secret (a map or idempotency key), and neither are names that merely contain a word:
// tokenId, outboxId, keyCheck, tokens.
var secretSuffixes = []string{"password", "token", "secret", "key"}

// isSecretKey reports whether a log field (or a key inside a logged value) names a secret. The
// name is compared in lower case with "_" and "-" removed.
func isSecretKey(name string) bool {
	k := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	if exactSecretKeys[k] {
		return true
	}
	for _, suf := range secretSuffixes {
		if strings.HasSuffix(k, suf) && len(k) > len(suf) {
			return true
		}
	}
	return false
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

// replaceAttr turns slog's built-in time and level into pino's, redacts secret keys (also when a
// secret key is an enclosing group, which slog does not report as an attribute of its own) and
// keeps error messages out of the log.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey:
			if a.Value.Kind() == slog.KindTime {
				return slog.Int64("time", a.Value.Time().UnixMilli())
			}
			a.Key = "fields.time"
		case slog.LevelKey:
			if l, ok := a.Value.Any().(slog.Level); ok && a.Value.Kind() == slog.KindAny {
				return slog.Int64("level", pinoLevel(l))
			}
			a.Key = "fields.level"
		}
	}
	for _, g := range groups {
		if isSecretKey(g) {
			return slog.String(a.Key, redacted)
		}
	}
	if isSecretKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			return errAttr(a.Key, err)
		}
		a.Value = slog.AnyValue(normalize(a.Value.Any()))
	}
	return a
}

// normalize turns any structured value into plain maps, slices and primitives through its JSON
// form (so struct fields, json tags, http.Header and pointers are all seen by key) and scrubs it.
// A value that cannot be marshalled is replaced by a fixed marker, never written raw.
func normalize(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return unloggable
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return unloggable
	}
	return scrub(out)
}

func errAttr(key string, err error) slog.Attr {
	label := kernel.ErrorLabel(err)
	typ, _, _ := strings.Cut(label, ": ")
	return slog.Group(key, slog.String("type", typ), slog.String("label", label))
}

// scrub redacts secret keys inside maps and slices that slog marshals without calling
// replaceAttr. It returns a copy; the caller's values are untouched.
func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if isSecretKey(k) {
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
