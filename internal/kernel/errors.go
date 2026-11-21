package kernel

import "errors"

// DomainError is a business-rule violation with a stable machine code and a human message.
type DomainError struct{ Code, Message string }

// NewDomainError builds a DomainError.
func NewDomainError(code, message string) *DomainError {
	return &DomainError{Code: code, Message: message}
}

// Error returns the message.
func (e *DomainError) Error() string { return e.Message }

// AsDomainError finds a DomainError anywhere in err's wrap chain.
func AsDomainError(err error) (*DomainError, bool) {
	var de *DomainError
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}
