package kernel_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

func TestAsDomainErrorThroughWrap(t *testing.T) {
	err := fmt.Errorf("ctx: %w", kernel.NewDomainError("not_found", "no such server"))
	de, ok := kernel.AsDomainError(err)
	if !ok || de.Code != "not_found" || de.Error() != "no such server" {
		t.Fatalf("got %v %v", de, ok)
	}
	if _, ok := kernel.AsDomainError(errors.New("x")); ok {
		t.Fatal("plain error is not a DomainError")
	}
}

func TestFixedClock(t *testing.T) {
	var c kernel.Clock = kernel.FixedClock{}
	if !c.Now().IsZero() {
		t.Fatal("fixed zero")
	}
	if (kernel.SystemClock{}).Now().IsZero() {
		t.Fatal("system clock zero")
	}
}
