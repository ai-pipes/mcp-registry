package kernel

import (
	"fmt"
	"log/slog"
)

// The markers every secret prints as.
const (
	SecretRedacted = "[Redacted]"
	SecretUnset    = "[unset]"
)

// Secret holds a string that must never reach a log line, an audit record, an error text or a
// dump: a person's token or PAT, a server or client secret, a session id, an sk_ token, a sign-in
// code, an OAuth state, code or verifier. Every way of printing or encoding it (fmt with any verb,
// slog, encoding/json, encoding.TextMarshaler) yields "[Redacted]", or "[unset]" for the zero
// value, which means "not set". The value is read only through the explicit Reveal, at the place
// that hands it to a client, a cipher or a hash.
//
// It holds the value two unexported pointers deep, however the Secret itself is held. fmt does
// not call methods on a value it reaches through an unexported field of another struct: it writes
// that value by reflection, so a string-based type would be written raw there, while a pointer is
// written as an address. A verb that does not suit a pointer (%s, %q, %d) makes fmt write the
// pointee of the outermost pointer it meets when that pointee is a struct, slice, array or map, so
// that pointee (secretBox) holds nothing but another pointer.
//
// A Secret is not comparable on purpose (== would compare addresses and answer "different" for
// equal values); compare through a constant-time port where a caller needs it. Copies of a Secret
// share the same immutable value.
type Secret struct {
	_ [0]func() // makes the type not comparable
	v *secretBox
}

type secretBox struct{ p *string }

// NewSecret wraps s. The result is set even when s is empty: an empty secret that is set (an
// admin's empty server secret, say) is not the zero value.
func NewSecret(s string) Secret { return Secret{v: &secretBox{p: &s}} }

// IsSet reports whether the secret has a value (the zero Secret has none).
func (s Secret) IsSet() bool { return s.v != nil }

// Reveal returns the value, "" when unset.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v.p
}

func (s Secret) text() string {
	if s.v == nil {
		return SecretUnset
	}
	return SecretRedacted
}

// Format implements fmt.Formatter for every verb. Without it a verb that does not suit the type
// (for example %d) would make fmt write the fields as part of its "%!d(...)" error text.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.text())) }

// String implements fmt.Stringer.
func (s Secret) String() string { return s.text() }

// GoString implements fmt.GoStringer (the %#v verb).
func (s Secret) GoString() string { return s.text() }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.text()) }

// MarshalJSON implements encoding/json.Marshaler (also honoured by encoding/json/v2). A type that
// must put the value into JSON does so through a named DTO with a plain field, filled by Reveal.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.text() + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.text()), nil }

// SecretBytes is Secret for binary values: keys and sealed (encrypted) secrets, which must not be
// printed either. The zero value is "not set".
type SecretBytes struct {
	_ [0]func() // makes the type not comparable (see Secret)
	v *secretBytesBox
}

// secretBytesBox holds only a pointer to the bytes (see Secret: a pointer straight to a []byte
// would make fmt write the slice under %s, %q or %d).
type secretBytesBox struct{ p *[]byte }

// NewSecretBytes wraps a copy of b, so later changes to b do not reach the secret. The result is
// set even when b is empty (nil included); the zero SecretBytes is the only unset one.
func NewSecretBytes(b []byte) SecretBytes {
	c := append([]byte(nil), b...)
	return SecretBytes{v: &secretBytesBox{p: &c}}
}

// IsSet reports whether the secret has a value.
func (s SecretBytes) IsSet() bool { return s.v != nil }

// Len is the length of the value, 0 when unset.
func (s SecretBytes) Len() int {
	if s.v == nil {
		return 0
	}
	return len(*s.v.p)
}

// Reveal returns a copy of the value, nil when unset or empty: changing the result does not
// change the secret.
func (s SecretBytes) Reveal() []byte {
	if s.v == nil || len(*s.v.p) == 0 {
		return nil
	}
	return append([]byte(nil), *s.v.p...)
}

func (s SecretBytes) text() string {
	if s.v == nil {
		return SecretUnset
	}
	return SecretRedacted
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
