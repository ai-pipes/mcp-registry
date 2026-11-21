package domain

// NotFound is a domain error.
type NotFound struct{}

func (*NotFound) Error() string { return "not found" }
