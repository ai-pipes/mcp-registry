package kernel_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

func TestUpstreamErrorKindIsVisibleThroughErrorsAs(t *testing.T) {
	for _, kind := range []kernel.UpstreamErrorKind{
		kernel.UpstreamUnauthorized, kernel.UpstreamTimeout, kernel.UpstreamTooLarge, kernel.UpstreamTransport,
	} {
		wrapped := fmt.Errorf("sync: %w", kernel.NewUpstreamError(kind))
		var ue *kernel.UpstreamError
		if !errors.As(wrapped, &ue) || ue.Kind != kind || ue.Rejection != nil {
			t.Errorf("%s: %+v", kind, ue)
		}
		got, ok := kernel.AsUpstreamError(wrapped)
		if !ok || got.Kind != kind {
			t.Errorf("%s: AsUpstreamError = %v, %v", kind, got, ok)
		}
	}
	if _, ok := kernel.AsUpstreamError(errors.New("plain")); ok {
		t.Error("a plain error is not an UpstreamError")
	}
	if _, ok := kernel.AsUpstreamError(nil); ok {
		t.Error("nil is not an UpstreamError")
	}
}

func TestUpstreamKindValues(t *testing.T) {
	for kind, want := range map[kernel.UpstreamErrorKind]string{
		kernel.UpstreamUnauthorized: "unauthorized",
		kernel.UpstreamTimeout:      "timeout",
		kernel.UpstreamTooLarge:     "too_large",
		kernel.UpstreamTransport:    "transport",
		kernel.UpstreamRejected:     "rejected",
	} {
		if string(kind) != want {
			t.Errorf("%q, want %q", kind, want)
		}
	}
}

func TestUpstreamErrorTextsAreFixedPerKind(t *testing.T) {
	for kind, want := range map[kernel.UpstreamErrorKind]string{
		kernel.UpstreamUnauthorized: "upstream answered 401",
		kernel.UpstreamTimeout:      "upstream did not answer in time",
		kernel.UpstreamTooLarge:     "upstream answer exceeds the size limit",
		kernel.UpstreamTransport:    "upstream transport error",
	} {
		if got := kernel.NewUpstreamError(kind).Error(); got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
}

func TestRejectedKeepsUpstreamTextApartFromTheError(t *testing.T) {
	e := kernel.RejectedUpstreamError(-32602, `owner "secret-xyz" not found`)
	if e.Kind != kernel.UpstreamRejected || e.Rejection == nil || e.Rejection.Code != -32602 ||
		e.Rejection.Message != `owner "secret-xyz" not found` {
		t.Fatalf("%+v", e)
	}
	if e.Error() != "upstream rejected the request (JSON-RPC -32602)" {
		t.Errorf("Error() = %q", e.Error())
	}
	wrapped := fmt.Errorf("call: %w", e)
	for _, s := range []string{e.Error(), wrapped.Error(), kernel.ErrorLabel(e), fmt.Sprintf("%v %+v %s", e, e, e)} {
		if strings.Contains(s, "secret-xyz") {
			t.Errorf("upstream text leaked into %q", s)
		}
	}
}

func TestErrorLabelOfAnUpstreamErrorIsItsKind(t *testing.T) {
	if l := kernel.ErrorLabel(kernel.NewUpstreamError(kernel.UpstreamTooLarge)); l != "kernel.UpstreamError: too_large" {
		t.Errorf("label %q", l)
	}
	if l := kernel.ErrorLabel(fmt.Errorf("w: %w", kernel.RejectedUpstreamError(1, "m"))); l != "kernel.UpstreamError: rejected" {
		t.Errorf("label %q", l)
	}
}
