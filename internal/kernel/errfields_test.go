package kernel_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

type dbError struct{ code, msg string }

func (e *dbError) Error() string { return e.msg }
func (e *dbError) Code() string  { return e.code }

func TestErrorLabelNeverHasMessage(t *testing.T) {
	const secret = `invalid input syntax: "hunter2"`
	cases := map[string]error{
		"plain":   errors.New(secret),
		"domain":  kernel.NewDomainError("x", secret),
		"coded":   &dbError{"22P02", secret},
		"wrapped": fmt.Errorf("%s: %w", secret, &dbError{"22P02", secret}),
		"badcode": &dbError{`bad code "hunter2"`, secret},
	}
	for name, err := range cases {
		if l := kernel.ErrorLabel(err); strings.Contains(l, "hunter2") {
			t.Errorf("%s: label leaks: %q", name, l)
		}
	}
}

func TestErrorLabelShape(t *testing.T) {
	if l := kernel.ErrorLabel(&dbError{"22P02", "m"}); l != "kernel_test.dbError: 22P02" {
		t.Fatalf("got %q", l)
	}
	if l := kernel.ErrorLabel(fmt.Errorf("w: %w", &dbError{"X1", "m"})); l != "kernel_test.dbError: X1" {
		t.Fatalf("wrapped: %q", l)
	}
	if l := kernel.ErrorLabel(&dbError{"has space", "m"}); l != "kernel_test.dbError" {
		t.Fatalf("badcode: %q", l)
	}
	if l := kernel.ErrorLabel(kernel.NewDomainError("c", "m")); l != "kernel.DomainError" {
		t.Fatalf("domain: %q", l)
	}
	if kernel.ErrorLabel(nil) != "nil" {
		t.Fatal("nil")
	}
}

type pgLike struct{ state, msg string }

func (e *pgLike) Error() string    { return e.msg }
func (e *pgLike) SQLState() string { return e.state }

func TestErrorLabelSQLState(t *testing.T) {
	if l := kernel.ErrorLabel(&pgLike{"22P02", "m"}); l != "kernel_test.pgLike: 22P02" {
		t.Fatalf("got %q", l)
	}
	joined := errors.Join(errors.New("a"), fmt.Errorf("x: %w", &pgLike{"23505", `dup "hunter2"`}))
	if l := kernel.ErrorLabel(joined); l != "kernel_test.pgLike: 23505" {
		t.Fatalf("join: %q", l)
	}
	multi := fmt.Errorf("%w; %w", errors.New("a"), &pgLike{"40001", "m"})
	if l := kernel.ErrorLabel(multi); l != "kernel_test.pgLike: 40001" {
		t.Fatalf("multi: %q", l)
	}
}
