package kernel_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

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

const secretValue = "Bearer s3cr3t-token-value"

func secretTarget() kernel.UpstreamTarget {
	return kernel.UpstreamTarget{
		URL:              "https://up.example/mcp",
		Headers:          kernel.UpstreamHeaders{"Authorization": secretValue},
		Timeout:          time.Second,
		MaxResponseBytes: 10,
	}
}

func TestUpstreamHeadersNeverPrintTheirValues(t *testing.T) {
	tg := secretTarget()
	tg.Headers = map[string]string{"Authorization": secretValue} // assignable from a plain map
	var back map[string]string = tg.Headers                      // and back: range, index and len keep working
	if back["Authorization"] != secretValue || len(tg.Headers) != 1 {
		t.Fatal("the underlying map must stay usable")
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		for name, arg := range map[string]any{"target": tg, "pointer": &tg, "headers": tg.Headers} {
			got := fmt.Sprintf(verb, arg)
			if strings.Contains(got, secretValue) || strings.Contains(got, "s3cr3t") {
				t.Errorf("%s of %s leaked: %s", verb, name, got)
			}
			if verb != "%x" && verb != "%d" && !strings.Contains(got, "Authorization") {
				t.Errorf("%s of %s lacks the header name: %s", verb, name, got)
			}
		}
	}
	if got := fmt.Sprint(tg.Headers); got != "map[Authorization:[redacted]]" {
		t.Errorf("String = %q", got)
	}
}

func TestUpstreamTargetInJSONAndSlogRedactsHeaders(t *testing.T) {
	tg := secretTarget()
	b, err := json.Marshal(tg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), `"Authorization":"[redacted]"`) {
		t.Errorf("json.Marshal: %s", b)
	}
	if text, err := tg.Headers.MarshalText(); err != nil || strings.Contains(string(text), "s3cr3t") || !strings.Contains(string(text), "Authorization") {
		t.Errorf("MarshalText: %q %v", text, err)
	}
	for name, mk := range map[string]func(*bytes.Buffer) *slog.Logger{
		"json": func(b *bytes.Buffer) *slog.Logger { return slog.New(slog.NewJSONHandler(b, nil)) },
		"text": func(b *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(b, nil)) },
	} {
		var buf bytes.Buffer
		l := mk(&buf)
		l.Info("sync", "target", tg)
		l.Info("sync", "headers", tg.Headers)
		l.Info("sync", slog.Group("g", slog.Any("h", tg.Headers)))
		if strings.Contains(buf.String(), "s3cr3t") || !strings.Contains(buf.String(), "Authorization") {
			t.Errorf("slog %s: %s", name, buf.String())
		}
	}
}
