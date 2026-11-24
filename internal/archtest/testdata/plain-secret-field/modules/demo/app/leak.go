package app

// Command carries a token as a plain string: reported. TokenURL, TokenID and CodeHash are not
// secrets (only the last word of a name counts).
type Command struct {
	AccessToken string
	TokenURL    string
	TokenID     string
	CodeHash    string
}

// Sealed is a named byte slice with a secret name: reported.
type Sealed []byte
