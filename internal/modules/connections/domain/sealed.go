package domain

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const redacted = "[redacted]"

// Sealed is token material already sealed by kernel.SecretBox outside the domain. The domain never
// opens it; it only carries the bytes. Every printing path (fmt with any verb, slog, JSON, text)
// shows "[redacted]", however a Sealed is held: the bytes are a kernel.SecretBytes. Read them with
// Bytes. The zero value (and a Sealed of no bytes) is "none".
type Sealed struct{ b kernel.SecretBytes }

// NewSealed wraps a copy of b; nil or empty is the zero Sealed ("none").
func NewSealed(b []byte) Sealed {
	if len(b) == 0 {
		return Sealed{}
	}
	return Sealed{b: kernel.NewSecretBytes(b)}
}

// Bytes is a copy of the sealed bytes, nil for none.
func (s Sealed) Bytes() []byte { return s.b.Reveal() }

// IsEmpty reports whether there are no bytes ("none").
func (s Sealed) IsEmpty() bool { return s.b.Len() == 0 }

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

// cloneSealed is s; a Sealed is immutable, so copies share its bytes.
func cloneSealed(s Sealed) Sealed { return s }
