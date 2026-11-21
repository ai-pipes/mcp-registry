package config

import (
	"fmt"
	"log/slog"
)

const (
	redactedText = "[Redacted]"
	unsetText    = "[unset]"
)

// Secret is a string value that must never reach a log line, an error or a dump by accident: every
// way of printing or encoding it yields "[Redacted]" ("[unset]" for the zero value, which means
// "not set"). The value is available only through the explicit Reveal, whose call sites are the few
// places that hand it to a client or a cipher.
//
// It holds a pointer, not the string. fmt reaches a field it may not call methods on (an
// unexported field of a struct, a struct behind a pointer under a non-pointer verb) by reflection
// with every method switched off; a string-based type would be written raw there, a pointer is
// written as an address at most. Secrets are not comparable by value on purpose: the API has no
// equality, and == on two Secrets would compare addresses.
type Secret struct{ v *string }

// NewSecret wraps s. An empty string is "not set": the result is the zero Secret.
func NewSecret(s string) Secret {
	if s == "" {
		return Secret{}
	}
	return Secret{v: &s}
}

// IsSet reports whether the secret has a value.
func (s Secret) IsSet() bool { return s.v != nil }

// Reveal returns the value, "" when unset.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

func (s Secret) text() string {
	if s.v == nil {
		return unsetText
	}
	return redactedText
}

// Format implements fmt.Formatter for every verb. Without it a verb that does not suit the type
// (for example %d) makes fmt print the raw value as part of its "%!d(...)" error text.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.text())) }

// String implements fmt.Stringer.
func (s Secret) String() string { return s.text() }

// GoString implements fmt.GoStringer (the %#v verb).
func (s Secret) GoString() string { return s.text() }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.text()) }

// MarshalJSON implements encoding/json.Marshaler (also honoured by encoding/json/v2).
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.text() + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.text()), nil }

// SecretBytes is Secret for binary values (keys): the zero value is "not set", a set one is never
// empty.
type SecretBytes struct{ v *[]byte }

// NewSecretBytes wraps a copy of b, so later changes to b do not reach the secret. Nil or empty is
// "not set": the result is the zero SecretBytes.
func NewSecretBytes(b []byte) SecretBytes {
	if len(b) == 0 {
		return SecretBytes{}
	}
	c := append([]byte(nil), b...)
	return SecretBytes{v: &c}
}

// IsSet reports whether the secret has a value.
func (s SecretBytes) IsSet() bool { return s.v != nil }

// Reveal returns the value (not a copy: callers must not modify it), nil when unset.
func (s SecretBytes) Reveal() []byte {
	if s.v == nil {
		return nil
	}
	return *s.v
}

func (s SecretBytes) text() string {
	if s.v == nil {
		return unsetText
	}
	return redactedText
}

// Format implements fmt.Formatter for every verb (see Secret.Format).
func (s SecretBytes) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.text())) }

// String implements fmt.Stringer.
func (s SecretBytes) String() string { return s.text() }

// GoString implements fmt.GoStringer (the %#v verb).
func (s SecretBytes) GoString() string { return s.text() }

// LogValue implements slog.LogValuer.
func (s SecretBytes) LogValue() slog.Value { return slog.StringValue(s.text()) }

// MarshalJSON implements encoding/json.Marshaler (also honoured by encoding/json/v2).
func (s SecretBytes) MarshalJSON() ([]byte, error) { return []byte(`"` + s.text() + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (s SecretBytes) MarshalText() ([]byte, error) { return []byte(s.text()), nil }

// Format implements fmt.Formatter. fmt prints a struct reached through a pointer under a verb that
// does not suit pointers (%s on a Config, say) by reflection with all methods switched off, which
// would write ClientSecret raw; a Formatter on the client itself is still honoured there.
func (c OIDCClient) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "{%s %s %s}", c.Issuer, c.ClientID, c.ClientSecret)
}
