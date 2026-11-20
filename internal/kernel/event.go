package kernel

// Event is a domain event delivered through the outbox.
type Event struct {
	Type    string
	Payload map[string]any
}
