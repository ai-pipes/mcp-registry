package config

import (
	"fmt"
	"log/slog"
)

const redactedText = "[Redacted]"

// Secret is a string value that must never reach a log line, an error or a dump by accident: every
// way of printing or encoding it yields "[Redacted]". The value is available only through the
// explicit Reveal, whose call sites are the few places that hand it to a client or a cipher.
type Secret string

// Format implements fmt.Formatter for every verb. Without it a verb that does not suit a string
// (for example %d) makes fmt print the raw value as part of its "%!d(...)" error text.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedText)) }

// Reveal returns the value.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return redactedText }

// GoString implements fmt.GoStringer (the %#v verb).
func (Secret) GoString() string { return redactedText }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redactedText) }

// MarshalJSON implements encoding/json.Marshaler (also honoured by encoding/json/v2).
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedText + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redactedText), nil }

// SecretBytes is Secret for binary values (keys). A nil SecretBytes means "not set"; a set one is
// never empty.
type SecretBytes []byte

// Format implements fmt.Formatter for every verb (see Secret.Format).
func (SecretBytes) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedText)) }

// Reveal returns the value (not a copy: callers must not modify it).
func (s SecretBytes) Reveal() []byte { return []byte(s) }

// String implements fmt.Stringer.
func (SecretBytes) String() string { return redactedText }

// GoString implements fmt.GoStringer (the %#v verb).
func (SecretBytes) GoString() string { return redactedText }

// LogValue implements slog.LogValuer.
func (SecretBytes) LogValue() slog.Value { return slog.StringValue(redactedText) }

// MarshalJSON implements encoding/json.Marshaler (also honoured by encoding/json/v2).
func (SecretBytes) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedText + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (SecretBytes) MarshalText() ([]byte, error) { return []byte(redactedText), nil }

// Format implements fmt.Formatter. fmt prints a struct reached through a pointer under a verb that
// does not suit pointers (%s on a Config, say) by reflection with all methods switched off, which
// would write ClientSecret raw; a Formatter on the client itself is still honoured there.
func (c OIDCClient) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "{%s %s %s}", c.Issuer, c.ClientID, c.ClientSecret)
}
