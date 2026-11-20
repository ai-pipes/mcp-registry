package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one JSON object per line: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestNumericLevels(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New("trace", &buf)
	l.Log(t.Context(), logging.LevelTrace, "t")
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	logging.Fatal(l, "f")
	want := []float64{10, 20, 30, 40, 50, 60}
	got := records(t, &buf)
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i, r := range got {
		if r["level"] != want[i] {
			t.Errorf("record %d: level %v, want %v", i, r["level"], want[i])
		}
	}
}

func TestRecordShape(t *testing.T) {
	var buf bytes.Buffer
	before := time.Now().UnixMilli()
	logging.New("info", &buf).Info("hello", "port", 3000)
	r := records(t, &buf)[0]
	if r["msg"] != "hello" || r["port"] != float64(3000) {
		t.Fatalf("msg/fields at top level: %v", r)
	}
	ts, ok := r["time"].(float64)
	if !ok || int64(ts) < before || int64(ts) > time.Now().UnixMilli() {
		t.Fatalf("time must be epoch milliseconds: %v", r["time"])
	}
	for _, k := range []string{"pid", "hostname"} {
		if _, has := r[k]; has {
			t.Errorf("unexpected %s", k)
		}
	}
}

func TestLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New("warn", &buf)
	l.Info("no")
	l.Warn("yes")
	got := records(t, &buf)
	if len(got) != 1 || got[0]["msg"] != "yes" {
		t.Fatalf("got %v", got)
	}
}

func TestSilent(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New("silent", &buf)
	l.Error("e")
	logging.Fatal(l, "f")
	if buf.Len() != 0 {
		t.Fatalf("silent wrote %q", buf.String())
	}
}

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New("info", &buf)
	l.Info("x",
		"password", "p1",
		"Token", "t1",
		"req", map[string]any{"headers": map[string]any{"Authorization": "a1", "cookie": "c1", "ok": "fine"}},
		"list", []any{map[string]any{"token": "t2"}},
		"g", nestedGroup(),
	)
	raw := buf.String()
	for _, s := range []string{"p1", "t1", "a1", "c1", "t2", "g1"} {
		if strings.Contains(raw, s) {
			t.Errorf("%q leaked: %s", s, raw)
		}
	}
	if !strings.Contains(raw, "fine") || strings.Count(raw, "[Redacted]") != 6 {
		t.Errorf("unexpected redaction result: %s", raw)
	}
}

func nestedGroup() any { return map[string]any{"a": map[string]any{"password": "g1"}} }

func TestErrAttrHasNoMessage(t *testing.T) {
	var buf bytes.Buffer
	err := errors.New("password=hunter2")
	logging.New("info", &buf).Error("failed", logging.ErrAttr(err))
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("message leaked: %s", buf.String())
	}
	e := records(t, &buf)[0]["err"].(map[string]any)
	if e["type"] != "errors.errorString" || e["label"] != kernel.ErrorLabel(err) {
		t.Fatalf("err = %v", e)
	}
}
