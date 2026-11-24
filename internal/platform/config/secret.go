package config

import (
	"fmt"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// Secret is a configuration value that must never reach a log line, an error or a dump by
// accident: kernel.Secret, which prints, logs and encodes as "[Redacted]" ("[unset]" for the zero
// value) however it is held. The value is available only through the explicit Reveal, whose call
// sites are the few places that hand it to a client or a cipher. In the configuration an empty
// value means "not set" (NewSecret), which kernel.NewSecret would keep as a set empty secret.
type Secret = kernel.Secret

// NewSecret wraps s. An empty string is "not set": the result is the zero Secret.
func NewSecret(s string) Secret {
	if s == "" {
		return Secret{}
	}
	return kernel.NewSecret(s)
}

// SecretBytes is Secret for binary values (keys): the zero value is "not set", a set one is never
// empty.
type SecretBytes = kernel.SecretBytes

// NewSecretBytes wraps a copy of b, so later changes to b do not reach the secret. Nil or empty is
// "not set": the result is the zero SecretBytes.
func NewSecretBytes(b []byte) SecretBytes {
	if len(b) == 0 {
		return SecretBytes{}
	}
	return kernel.NewSecretBytes(b)
}

// Format implements fmt.Formatter. fmt prints a struct reached through a pointer under a verb that
// does not suit pointers (%s on a Config, say) by reflection with all methods switched off, which
// would write ClientSecret raw; a Formatter on the client itself is still honoured there. The
// secret's own marker text (String) is what is written, so no secret value reaches fmt.
func (c OIDCClient) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "{%s %s %s}", c.Issuer, c.ClientID, c.ClientSecret.String())
}
