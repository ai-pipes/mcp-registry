package kernel

// SecretBox is symmetric authenticated encryption for secrets stored in the database.
type SecretBox interface {
	Seal(plain string) ([]byte, error)
	// Open fails when the bytes were altered or sealed with another key.
	Open(sealed []byte) (string, error)
}

// Outcomes of Reseal.
const (
	ResealCurrent    = "current"
	ResealResealed   = "resealed"
	ResealUnreadable = "unreadable"
)

// ResealOutcome is what re-sealing one stored secret came to.
type ResealOutcome struct {
	Kind   string // "current" | "resealed" | "unreadable"
	Sealed []byte // only for "resealed"
}

// Reseal moves one stored secret from the old key to the new one. The new key is tried first,
// so a repeated run is harmless. The plain text never leaves this function.
func Reseal(sealed []byte, from, to SecretBox) ResealOutcome {
	if _, err := to.Open(sealed); err == nil {
		return ResealOutcome{Kind: ResealCurrent}
	}
	plain, err := from.Open(sealed)
	if err != nil {
		return ResealOutcome{Kind: ResealUnreadable}
	}
	out, err := to.Seal(plain)
	if err != nil {
		return ResealOutcome{Kind: ResealUnreadable}
	}
	return ResealOutcome{Kind: ResealResealed, Sealed: out}
}
