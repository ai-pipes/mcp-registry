package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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

type tagged struct {
	Password string `json:"password"`
	Name     string `json:"name"`
}

type untagged struct {
	Token string
	Name  string
}

type tokenValuer struct{}

func (tokenValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("value", "VALUER-SECRET"))
}

type unmarshalable struct{ C chan int }

func TestRedactionReachesEveryValueType(t *testing.T) {
	hdr := http.Header{"Authorization": {"HDR-SECRET"}, "Cookie": {"COOKIE-SECRET"}, "Accept": {"text/plain"}}
	m := map[string]any{"password": "PTR-SECRET"}
	cases := map[string]struct {
		secret string
		log    func(l *slog.Logger)
	}{
		"http.Header":       {"HDR-SECRET", func(l *slog.Logger) { l.Info("x", "req", map[string]any{"headers": hdr}) }},
		"cookie":            {"COOKIE-SECRET", func(l *slog.Logger) { l.Info("x", "headers", hdr) }},
		"map[string]string": {"MSS-SECRET", func(l *slog.Logger) { l.Info("x", "m", map[string]string{"token": "MSS-SECRET"}) }},
		"[]map":             {"SLM-SECRET", func(l *slog.Logger) { l.Info("x", "m", []map[string]any{{"password": "SLM-SECRET"}}) }},
		"pointer to map":    {"PTR-SECRET", func(l *slog.Logger) { l.Info("x", "m", &m) }},
		"tagged struct":     {"TAG-SECRET", func(l *slog.Logger) { l.Info("x", "s", tagged{Password: "TAG-SECRET", Name: "n"}) }},
		"untagged struct":   {"UNT-SECRET", func(l *slog.Logger) { l.Info("x", "s", &untagged{Token: "UNT-SECRET"}) }},
		"group under token": {"GRP-SECRET", func(l *slog.Logger) { l.Info("x", slog.Group("token", slog.String("value", "GRP-SECRET"))) }},
		"WithGroup":         {"WG-SECRET", func(l *slog.Logger) { l.WithGroup("password").Info("x", "v", "WG-SECRET") }},
		"WithGroup case":    {"WGC-SECRET", func(l *slog.Logger) { l.WithGroup("Authorization").Info("x", "v", "WGC-SECRET") }},
		"LogValuer group":   {"VALUER-SECRET", func(l *slog.Logger) { l.Info("x", "token", tokenValuer{}) }},
		"unmarshalable":     {"UNM-SECRET", func(l *slog.Logger) { l.Info("x", "u", unmarshalable{}, "password", "UNM-SECRET") }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			c.log(logging.New("info", &buf))
			raw := buf.String()
			if strings.Contains(raw, c.secret) {
				t.Errorf("secret leaked: %s", raw)
			}
			if name != "unmarshalable" && !strings.Contains(raw, "[Redacted]") {
				t.Errorf("no [Redacted]: %s", raw)
			}
		})
	}
}

func TestNonRedactedStructFieldsSurvive(t *testing.T) {
	var buf bytes.Buffer
	logging.New("info", &buf).Info("x", "s", tagged{Password: "P", Name: "visible"})
	if !strings.Contains(buf.String(), "visible") {
		t.Fatalf("%s", buf.String())
	}
}

func TestUnmarshalableValueIsMarkedNotWritten(t *testing.T) {
	var buf bytes.Buffer
	logging.New("info", &buf).Info("x", "u", unmarshalable{C: make(chan int)})
	r := records(t, &buf)[0]
	if r["u"] != "[Unloggable]" {
		t.Fatalf("got %v", r["u"])
	}
}

func TestRawErrorAttributeHasNoMessage(t *testing.T) {
	var buf bytes.Buffer
	err := errors.New("MSG-SECRET")
	logging.New("info", &buf).Error("failed", "err", err, "cause", err)
	if strings.Contains(buf.String(), "MSG-SECRET") {
		t.Fatalf("message leaked: %s", buf.String())
	}
	r := records(t, &buf)[0]
	for _, k := range []string{"err", "cause"} {
		e, ok := r[k].(map[string]any)
		if !ok || e["type"] != "errors.errorString" || e["label"] != kernel.ErrorLabel(err) {
			t.Errorf("%s = %v", k, r[k])
		}
	}
}

func TestCallerLevelAndTimeAreRenamed(t *testing.T) {
	var buf bytes.Buffer
	logging.New("info", &buf).Info("x", "level", "custom", "time", "yesterday")
	if strings.Count(buf.String(), `"level"`) != 1 || strings.Count(buf.String(), `"time"`) != 1 {
		t.Fatalf("duplicate built-in keys: %s", buf.String())
	}
	r := records(t, &buf)[0]
	if r["fields.level"] != "custom" || r["fields.time"] != "yesterday" || r["level"] != float64(30) {
		t.Fatalf("%v", r)
	}
}

func TestRedactionCoversNaturalSecretNames(t *testing.T) {
	secrets := []string{
		"password", "Password", "token", "Token", "authorization", "Authorization", "cookie", "Cookie",
		"set-cookie", "Set-Cookie", "secret", "clientSecret", "client_secret", "client-secret",
		"accessToken", "access_token", "refreshToken", "refresh_token", "X-Api-Key", "apiKey", "api_key",
		"secretsKey", "SECRETS_KEY", "dbPassword", "bearerToken", "PGPASSWORD",
	}
	for _, name := range secrets {
		var buf bytes.Buffer
		l := logging.New("info", &buf)
		l.Info("x", name, "ZZ-top-level")
		l.Info("x", "req", map[string]any{name: "ZZ-nested"})
		l.Info("x", slog.Group("g", slog.String(name, "ZZ-grouped")))
		l.Info("x", slog.Group(name, slog.String("v", "ZZ-in-group")))
		if strings.Contains(buf.String(), "ZZ-") {
			t.Errorf("%q leaked: %s", name, buf.String())
		}
	}
}

// Names the server logs today (apps/server/src and the Go platform) and natural ones for ids and
// counters must stay readable.
func TestRedactionLeavesOrdinaryNamesAlone(t *testing.T) {
	ordinary := []string{
		"reqId", "outboxId", "tokenId", "keyCheck", "type", "label", "err", "version", "port", "signal",
		"applied", "job", "stack", "method", "path", "status", "ip", "ms", "tool", "servers", "failed",
		"sessions", "key", "tokens", "secrets", "keyId", "passwordSet", "authorized", "cookies", "userId",
		"handler", "attempts", "ref", "server",
	}
	var buf bytes.Buffer
	l := logging.New("info", &buf)
	for _, name := range ordinary {
		l.Info("x", name, "ZZ-"+name)
	}
	for _, name := range ordinary {
		if !strings.Contains(buf.String(), "ZZ-"+name+`"`) {
			t.Errorf("%q was redacted: %s", name, buf.String())
		}
	}
}
