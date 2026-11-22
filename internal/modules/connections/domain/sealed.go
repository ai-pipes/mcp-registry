package domain

import (
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[redacted]"

// Sealed is token material already sealed by kernel.SecretBox outside the domain. The domain never
// opens it; it only carries the bytes. Every printing path (fmt with any verb, slog, JSON, text)
// shows "[redacted]"; read the bytes by converting to []byte.
type Sealed []byte

// String is "[redacted]".
func (Sealed) String() string { return redacted }

// GoString is "[redacted]" too (the %#v form).
func (Sealed) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (Sealed) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue is the slog form.
func (Sealed) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON writes "[redacted]".
func (Sealed) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText writes "[redacted]".
func (Sealed) MarshalText() ([]byte, error) { return []byte(redacted), nil }

func cloneSealed(s Sealed) Sealed {
	if len(s) == 0 {
		return nil
	}
	return append(Sealed(nil), s...)
}
