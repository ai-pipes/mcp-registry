package infra

import (
	"fmt"
	"slices"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// checkEventTypes refuses an event whose type is not in domain.EventTypes: an internal error (a
// programming mistake), returned inside the transaction so the change rolls back. The outbox
// handler table is checked against that list at assembly, so an unlisted type would be lost.
func checkEventTypes(events []kernel.Event) error {
	listed := domain.EventTypes()
	for _, e := range events {
		if !slices.Contains(listed, e.Type) {
			return fmt.Errorf("catalog event type %q is not in domain.EventTypes", e.Type)
		}
	}
	return nil
}
