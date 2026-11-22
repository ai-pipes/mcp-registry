package infra

import (
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// Every event the catalog writes must be listed in domain.EventTypes, which is what the
// composition checks the outbox handler table against.
func TestOnlyListedEventTypesPassTheCheck(t *testing.T) {
	for _, typ := range domain.EventTypes() {
		if err := checkEventTypes([]kernel.Event{{Type: typ}}); err != nil {
			t.Errorf("%s refused: %v", typ, err)
		}
	}
	if err := checkEventTypes(nil); err != nil {
		t.Errorf("no events: %v", err)
	}
	err := checkEventTypes([]kernel.Event{{Type: domain.EventServerChanged}, {Type: "catalog.unlisted"}})
	if err == nil || !strings.Contains(err.Error(), "catalog.unlisted") {
		t.Fatalf("an unlisted type was accepted: %v", err)
	}
}
