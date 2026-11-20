package kernel

import "context"

// AuditEntry is one audit record.
type AuditEntry struct {
	ActorID *string
	Action  string
	Target  *string
	Meta    map[string]any
}

// AuditLog records audit entries.
type AuditLog interface {
	Record(ctx context.Context, e AuditEntry) error
}
