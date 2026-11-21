package domain_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func snap(name string, mut ...func(*domain.ToolSnapshot)) domain.ToolSnapshot {
	s := domain.ToolSnapshot{
		Name: name, Description: "does " + name, InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	for _, m := range mut {
		m(&s)
	}
	return s
}

func stored(name string, mut ...func(*domain.ToolState)) *domain.Tool {
	st := domain.ToolState{Server: "gitea", ToolSnapshot: snap(name), SyncedAt: t0}
	for _, m := range mut {
		m(&st)
	}
	return domain.RehydrateTool(st)
}

func hiddenState(h bool) func(*domain.ToolState) { return func(s *domain.ToolState) { s.Hidden = h } }

func toolsEvent(kind, server string, refs ...string) kernel.Event {
	return kernel.Event{Type: "catalog." + kind, Payload: map[string]any{"server": server, "refs": refs}}
}

func TestToolRefAndVisibility(t *testing.T) {
	tl := stored("issues.create")
	if tl.Ref() != "gitea.issues.create" || tl.Server() != "gitea" || tl.Name() != "issues.create" || !tl.Visible() || tl.Hidden() || tl.IsRemoved() {
		t.Fatal("tool")
	}
	if stored("a", hiddenState(true)).Visible() {
		t.Fatal("hidden is not visible")
	}
	if stored("a", func(s *domain.ToolState) { s.RemovedAt = &t0 }).Visible() {
		t.Fatal("removed is not visible")
	}
	if tl.Title() != nil || tl.Description() != "does issues.create" || !tl.SyncedAt().Equal(t0) || tl.RemovedAt() != nil {
		t.Fatal("getters")
	}
	if string(tl.InputSchema()) != `{"type":"object"}` || tl.OutputSchema() != nil {
		t.Fatal("schemas")
	}
	if !reflect.DeepEqual(tl.Snapshot(), snap("issues.create")) {
		t.Fatal("snapshot")
	}
}

func TestSetHiddenEmitsOnlyOnChange(t *testing.T) {
	tl := stored("a")
	changed, err := tl.SetHidden(false, t1)
	if err != nil || changed || len(tl.PullEvents()) != 0 {
		t.Fatal(changed, err)
	}
	changed, err = tl.SetHidden(true, t1)
	if err != nil || !changed || !tl.Hidden() {
		t.Fatal(changed, err)
	}
	if got := tl.PullEvents(); !reflect.DeepEqual(got, []kernel.Event{toolsEvent("tools_changed", "gitea", "gitea.a")}) {
		t.Fatalf("%#v", got)
	}
	if len(tl.PullEvents()) != 0 {
		t.Fatal("drained")
	}
}

func TestRemovedToolRefusesHiding(t *testing.T) {
	tl := stored("a", func(s *domain.ToolState) { s.RemovedAt = &t0 })
	if !tl.IsRemoved() {
		t.Fatal("removed")
	}
	for _, h := range []bool{true, false} {
		changed, err := tl.SetHidden(h, t1)
		wantMsg(t, err, "not_found", `tool "gitea.a" not found`)
		if changed {
			t.Fatal("changed")
		}
	}
	if len(tl.PullEvents()) != 0 {
		t.Fatal("events")
	}
}

func refs(rs ...string) []string { return append([]string{}, rs...) }

func TestReconcileAddsNewTools(t *testing.T) {
	r := domain.ReconcileTools("gitea", nil, []domain.ToolSnapshot{snap("a"), snap("b")}, t1)
	if names(r.Upserts) != "a,b" || !reflect.DeepEqual(r.ChangedRefs, refs("gitea.a", "gitea.b")) ||
		!reflect.DeepEqual(r.AddedRefs, refs("gitea.a", "gitea.b")) || len(r.RemovedRefs) != 0 {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(r.Events, []kernel.Event{toolsEvent("tools_changed", "gitea", "gitea.a", "gitea.b")}) {
		t.Fatalf("%#v", r.Events)
	}
	if r.Upserts[0].Hidden() || !r.Upserts[0].SyncedAt().Equal(t1) || r.Upserts[0].IsRemoved() || r.Upserts[0].Server() != "gitea" {
		t.Fatal("new tool state")
	}
}

func names(ts []*domain.Tool) string {
	out := ""
	for i, t := range ts {
		if i > 0 {
			out += ","
		}
		out += t.Name()
	}
	return out
}

func TestReconcileLeavesUnchangedAlone(t *testing.T) {
	r := domain.ReconcileTools("gitea", []*domain.Tool{stored("a")}, []domain.ToolSnapshot{snap("a")}, t1)
	if len(r.Upserts) != 0 || len(r.Events) != 0 || len(r.ChangedRefs) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestReconcileChangedKeepsHidden(t *testing.T) {
	r := domain.ReconcileTools("gitea", []*domain.Tool{stored("a", hiddenState(true))},
		[]domain.ToolSnapshot{snap("a", func(s *domain.ToolSnapshot) { s.Description = "new" })}, t1)
	if len(r.Upserts) != 1 {
		t.Fatalf("%+v", r)
	}
	u := r.Upserts[0]
	if u.Description() != "new" || !u.Hidden() || !u.SyncedAt().Equal(t1) {
		t.Fatal("upsert")
	}
	if !reflect.DeepEqual(r.ChangedRefs, refs("gitea.a")) || len(r.AddedRefs) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestReconcileRemovesVanishedOnce(t *testing.T) {
	r := domain.ReconcileTools("gitea", []*domain.Tool{stored("a"), stored("b", hiddenState(true))}, []domain.ToolSnapshot{snap("a")}, t1)
	if len(r.Upserts) != 1 || r.Upserts[0].Name() != "b" || r.Upserts[0].RemovedAt() == nil || !r.Upserts[0].RemovedAt().Equal(t1) {
		t.Fatalf("%+v", r)
	}
	if !r.Upserts[0].SyncedAt().Equal(t0) || !r.Upserts[0].Hidden() {
		t.Fatal("syncedAt and hidden are kept")
	}
	if !reflect.DeepEqual(r.RemovedRefs, refs("gitea.b")) {
		t.Fatal(r.RemovedRefs)
	}
	if !reflect.DeepEqual(r.Events, []kernel.Event{toolsEvent("tools_removed", "gitea", "gitea.b")}) {
		t.Fatalf("%#v", r.Events)
	}
	removed := func(s *domain.ToolState) { s.RemovedAt = &t0 }
	again := domain.ReconcileTools("gitea", []*domain.Tool{stored("b", removed)}, nil, t1)
	if len(again.Upserts) != 0 || len(again.Events) != 0 {
		t.Fatal("already removed")
	}
}

func TestReconcileRevivesKeepingHidden(t *testing.T) {
	removed := func(s *domain.ToolState) { s.RemovedAt = &t0; s.Hidden = true }
	r := domain.ReconcileTools("gitea", []*domain.Tool{stored("a", removed)}, []domain.ToolSnapshot{snap("a")}, t1)
	if len(r.Upserts) != 1 || r.Upserts[0].RemovedAt() != nil || !r.Upserts[0].Hidden() {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(r.ChangedRefs, refs("gitea.a")) || !reflect.DeepEqual(r.AddedRefs, refs("gitea.a")) {
		t.Fatalf("%+v", r)
	}
}

func TestReconcileDuplicateNamesFirstWins(t *testing.T) {
	r := domain.ReconcileTools("gitea", nil, []domain.ToolSnapshot{snap("a"), snap("a", func(s *domain.ToolSnapshot) { s.Description = "dup" })}, t1)
	if len(r.Upserts) != 1 || r.Upserts[0].Description() != "does a" {
		t.Fatalf("%+v", r)
	}
}

func TestReconcileEventOrderAndResultOrder(t *testing.T) {
	r := domain.ReconcileTools("gitea",
		[]*domain.Tool{stored("old1"), stored("keep"), stored("old2")},
		[]domain.ToolSnapshot{snap("z"), snap("keep"), snap("a")}, t1)
	if names(r.Upserts) != "z,a,old1,old2" {
		t.Fatalf("incoming order first (changed), then removed in stored order: %s", names(r.Upserts))
	}
	if len(r.Events) != 2 || r.Events[0].Type != "catalog.tools_changed" || r.Events[1].Type != "catalog.tools_removed" {
		t.Fatalf("%#v", r.Events)
	}
	if !reflect.DeepEqual(r.Events[0].Payload["refs"], []string{"gitea.z", "gitea.a"}) || !reflect.DeepEqual(r.Events[1].Payload["refs"], []string{"gitea.old1", "gitea.old2"}) {
		t.Fatalf("%#v", r.Events)
	}
}

func TestReconcileDuplicateStoredNames(t *testing.T) {
	// a Map built from entries: the later stored row wins, as in the TypeScript code
	a1 := stored("a", func(s *domain.ToolState) { s.Description = "first" })
	a2 := stored("a")
	r := domain.ReconcileTools("gitea", []*domain.Tool{a1, a2}, []domain.ToolSnapshot{snap("a")}, t1)
	if len(r.Upserts) != 0 {
		t.Fatalf("compared against the last stored row: %+v", r)
	}
}

func TestReconcileSameContentIgnoresJSONFormattingAndKeyOrder(t *testing.T) {
	old := stored("a", func(s *domain.ToolState) {
		s.InputSchema = json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer","minimum":1},"y":{"type":"string"}},"required":["x"]}`)
		s.OutputSchema = json.RawMessage(`{"b":1,"a":[1,2,{"d":1,"c":2}]}`)
	})
	same := snap("a", func(s *domain.ToolSnapshot) {
		s.InputSchema = json.RawMessage("{\n  \"required\": [\"x\"],\n  \"properties\": {\"y\": {\"type\": \"string\"}, \"x\": {\"minimum\": 1.0, \"type\": \"integer\"}},\n  \"type\": \"object\"\n}")
		s.OutputSchema = json.RawMessage(`{"a":[1,2,{"c":2,"d":1}],"b":1e0}`)
	})
	if r := domain.ReconcileTools("gitea", []*domain.Tool{old}, []domain.ToolSnapshot{same}, t1); len(r.Upserts) != 0 {
		t.Fatal("key order, whitespace and 1 vs 1.0 are not changes (deep-equal on parsed JSON)")
	}
}

func TestReconcileDetectsEveryChangedField(t *testing.T) {
	base := func(s *domain.ToolState) {
		s.Title = ptr("T")
		s.OutputSchema = json.RawMessage(`{"type":"object"}`)
		s.Annotations = domain.ToolAnnotations{ReadOnlyHint: ptr(true)}
	}
	muts := map[string]func(*domain.ToolSnapshot){
		"title":              func(s *domain.ToolSnapshot) { s.Title = ptr("U") },
		"title removed":      func(s *domain.ToolSnapshot) { s.Title = nil },
		"empty vs no title":  func(s *domain.ToolSnapshot) { s.Title = ptr("") },
		"description":        func(s *domain.ToolSnapshot) { s.Description = "x" },
		"input schema":       func(s *domain.ToolSnapshot) { s.InputSchema = json.RawMessage(`{"type":"object","x":1}`) },
		"input array order":  func(s *domain.ToolSnapshot) { s.InputSchema = json.RawMessage(`{"type":["object"]}`) },
		"output removed":     func(s *domain.ToolSnapshot) { s.OutputSchema = nil },
		"output changed":     func(s *domain.ToolSnapshot) { s.OutputSchema = json.RawMessage(`{"type":"array"}`) },
		"annotation flipped": func(s *domain.ToolSnapshot) { s.Annotations = domain.ToolAnnotations{ReadOnlyHint: ptr(false)} },
		"annotation removed": func(s *domain.ToolSnapshot) { s.Annotations = domain.ToolAnnotations{} },
		"annotation added": func(s *domain.ToolSnapshot) {
			s.Annotations = domain.ToolAnnotations{ReadOnlyHint: ptr(true), OpenWorldHint: ptr(false)}
		},
	}
	for name, mut := range muts {
		old := stored("a", base)
		next := domain.ToolSnapshot{
			Name: "a", Title: ptr("T"), Description: "does a", InputSchema: json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`), Annotations: domain.ToolAnnotations{ReadOnlyHint: ptr(true)},
		}
		mut(&next)
		if r := domain.ReconcileTools("gitea", []*domain.Tool{old}, []domain.ToolSnapshot{next}, t1); len(r.Upserts) != 1 {
			t.Errorf("%s: not detected", name)
		}
	}
	old := stored("a", base)
	same := domain.ToolSnapshot{
		Name: "a", Title: ptr("T"), Description: "does a", InputSchema: json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`), Annotations: domain.ToolAnnotations{ReadOnlyHint: ptr(true)},
	}
	if r := domain.ReconcileTools("gitea", []*domain.Tool{old}, []domain.ToolSnapshot{same}, t1); len(r.Upserts) != 0 {
		t.Fatal("identical")
	}
}

func TestReconcileNumbersCompareAsFloat64(t *testing.T) {
	// the TypeScript code compares JS-parsed values: these two collapse to one double
	a := stored("a", func(s *domain.ToolState) { s.InputSchema = json.RawMessage(`{"n":9007199254740993}`) })
	b := snap("a", func(s *domain.ToolSnapshot) { s.InputSchema = json.RawMessage(`{"n":9007199254740992}`) })
	if r := domain.ReconcileTools("gitea", []*domain.Tool{a}, []domain.ToolSnapshot{b}, t1); len(r.Upserts) != 0 {
		t.Fatal("float64 semantics, as in the TypeScript canonical()")
	}
}

func TestReconcileNumbersOutOfRangeAreJavaScriptNumbers(t *testing.T) {
	// JSON.parse("1e999") is Infinity and JSON.stringify turns it into null; a jsonb round trip
	// stores the number with its digits expanded, which must not look like a change.
	huge := "1" + strings.Repeat("0", 999)
	cases := []struct {
		a, b string
		same bool
	}{
		{`{"a":1e999}`, `{"a":null}`, true},
		{`{"a":1e999}`, `{"a":` + huge + `}`, true},
		{`{"a":-1e999}`, `{"a":null}`, true},
		{`{"a":1e999}`, `{"a":2e999}`, true},
		{`{"a":1e999}`, `{"a":1}`, false},
		{`{"a":1}`, `{"a":1.0}`, true},
		{`{"a":1}`, `{"a":1.5}`, false},
		{`{"a":-0}`, `{"a":0}`, true},
		{`{"a":1e-999}`, `{"a":0}`, true},
		{`{"a":12345678901234567890}`, `{"a":12345678901234567891}`, true},
		{`{"a":12345678901234567890}`, `{"a":12345678901234577890}`, false},
		{`[1e999,2]`, `[null,2]`, true},
		{`{"a":"1e999"}`, `{"a":null}`, false},
	}
	for _, c := range cases {
		old := stored("a", func(s *domain.ToolState) { s.InputSchema = json.RawMessage(c.a) })
		next := snap("a", func(s *domain.ToolSnapshot) { s.InputSchema = json.RawMessage(c.b) })
		r := domain.ReconcileTools("gitea", []*domain.Tool{old}, []domain.ToolSnapshot{next}, t1)
		if (len(r.Upserts) == 0) != c.same {
			t.Errorf("%s vs %s: same=%v want %v", c.a, c.b, len(r.Upserts) == 0, c.same)
		}
	}
}

func TestReconcileSyntacticallyInvalidJSONComparesAsText(t *testing.T) {
	a := stored("a", func(s *domain.ToolState) { s.InputSchema = json.RawMessage(`{"n":`) })
	same := snap("a", func(s *domain.ToolSnapshot) { s.InputSchema = json.RawMessage(`{"n":`) })
	other := snap("a", func(s *domain.ToolSnapshot) { s.InputSchema = json.RawMessage(`{"m":`) })
	if r := domain.ReconcileTools("gitea", []*domain.Tool{a}, []domain.ToolSnapshot{same}, t1); len(r.Upserts) != 0 {
		t.Fatal("identical bytes")
	}
	if r := domain.ReconcileTools("gitea", []*domain.Tool{a}, []domain.ToolSnapshot{other}, t1); len(r.Upserts) != 1 {
		t.Fatal("different bytes")
	}
}

func TestReconcileTitleNilAndEmptyDiffer(t *testing.T) {
	// TypeScript: `a.title === b.title` over string | null, so null and "" are different.
	for _, c := range []struct{ old, next *string }{{nil, ptr("")}, {ptr(""), nil}, {ptr(""), ptr("x")}} {
		old := stored("a", func(s *domain.ToolState) { s.Title = c.old })
		next := snap("a", func(s *domain.ToolSnapshot) { s.Title = c.next })
		if r := domain.ReconcileTools("gitea", []*domain.Tool{old}, []domain.ToolSnapshot{next}, t1); len(r.Upserts) != 1 {
			t.Errorf("%v vs %v not detected", c.old, c.next)
		}
	}
	old := stored("a", func(s *domain.ToolState) { s.Title = ptr("") })
	next := snap("a", func(s *domain.ToolSnapshot) { s.Title = ptr("") })
	if r := domain.ReconcileTools("gitea", []*domain.Tool{old}, []domain.ToolSnapshot{next}, t1); len(r.Upserts) != 0 {
		t.Fatal("equal empty titles")
	}
}

func TestToolGettersReturnCopies(t *testing.T) {
	in := json.RawMessage(`{"type":"object"}`)
	out := json.RawMessage(`{"o":1}`)
	title := "T"
	ro := true
	tl := domain.RehydrateTool(domain.ToolState{
		ToolSnapshot: domain.ToolSnapshot{Name: "a", Title: &title, InputSchema: in, OutputSchema: out, Annotations: domain.ToolAnnotations{ReadOnlyHint: &ro}},
		Server:       "gitea", SyncedAt: t0,
	})
	// the input of RehydrateTool is copied
	in[2] = 'X'
	out[2] = 'X'
	title = "changed"
	ro = false
	if string(tl.InputSchema()) != `{"type":"object"}` || string(tl.OutputSchema()) != `{"o":1}` || *tl.Title() != "T" || !*tl.Annotations().ReadOnlyHint {
		t.Fatal("RehydrateTool shares memory with its input")
	}
	// getters
	tl.InputSchema()[2] = 'X'
	tl.OutputSchema()[2] = 'X'
	*tl.Title() = "changed"
	*tl.Annotations().ReadOnlyHint = false
	snapshot := tl.Snapshot()
	snapshot.InputSchema[2] = 'X'
	snapshot.OutputSchema[2] = 'X'
	*snapshot.Title = "changed"
	*snapshot.Annotations.ReadOnlyHint = false
	removed := t1
	tl2 := domain.RehydrateTool(domain.ToolState{Server: "s", RemovedAt: &removed, SyncedAt: t0})
	*tl2.RemovedAt() = t2
	if !tl2.RemovedAt().Equal(t1) {
		t.Fatal("RemovedAt shares memory")
	}
	if string(tl.InputSchema()) != `{"type":"object"}` || string(tl.OutputSchema()) != `{"o":1}` || *tl.Title() != "T" || !*tl.Annotations().ReadOnlyHint {
		t.Fatal("a getter returned the aggregate's own memory")
	}
}

func TestReconcileResultDoesNotShareMemory(t *testing.T) {
	in := snap("a")
	r := domain.ReconcileTools("gitea", nil, []domain.ToolSnapshot{in}, t1)
	in.InputSchema[2] = 'X'
	if string(r.Upserts[0].InputSchema()) != `{"type":"object"}` {
		t.Fatal("upsert shares the incoming bytes")
	}
	r.Events[0].Payload["refs"].([]string)[0] = "changed"
	r.ChangedRefs[0] = "changed"
	if r.Upserts[0].Ref() != "gitea.a" {
		t.Fatal("ref")
	}
	r2 := domain.ReconcileTools("gitea", nil, []domain.ToolSnapshot{snap("a")}, t1)
	if r2.Events[0].Payload["refs"].([]string)[0] != "gitea.a" {
		t.Fatal("setup")
	}
	r2.ChangedRefs[0] = "changed"
	if r2.Events[0].Payload["refs"].([]string)[0] != "gitea.a" {
		t.Fatal("the event payload shares its slice with ChangedRefs")
	}
	removed := domain.ReconcileTools("gitea", []*domain.Tool{stored("b")}, nil, t1)
	removed.RemovedRefs[0] = "changed"
	if removed.Events[0].Payload["refs"].([]string)[0] != "gitea.b" {
		t.Fatal("the event payload shares its slice with RemovedRefs")
	}
}

func TestReconcileStoredSchemasKeepRawBytes(t *testing.T) {
	raw := json.RawMessage(`{"b":1.0,  "a":99999999999999999999}`)
	r := domain.ReconcileTools("gitea", nil, []domain.ToolSnapshot{snap("a", func(s *domain.ToolSnapshot) { s.InputSchema = raw })}, t1)
	if string(r.Upserts[0].InputSchema()) != string(raw) {
		t.Fatalf("the upstream bytes must be kept without loss: %s", r.Upserts[0].InputSchema())
	}
}

func TestReconcileResultIsNeverNil(t *testing.T) {
	r := domain.ReconcileTools("gitea", nil, nil, t1)
	if r.Upserts == nil || r.ChangedRefs == nil || r.AddedRefs == nil || r.RemovedRefs == nil || r.Events == nil {
		t.Fatal("empty slices, not nil, so JSON and counts are stable")
	}
}

func TestSnapshotFromUpstreamDefaults(t *testing.T) {
	got := domain.SnapshotFromUpstream(kernel.UpstreamTool{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)})
	want := domain.ToolSnapshot{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}

func TestSnapshotFromUpstreamKeepsFieldsAndOnlyBooleanHints(t *testing.T) {
	got := domain.SnapshotFromUpstream(kernel.UpstreamTool{
		Name: "a", Title: ptr("A"), Description: ptr("does a"),
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations:  json.RawMessage(`{"title":"ignored","readOnlyHint":true,"destructiveHint":false,"idempotentHint":"yes","openWorldHint":true}`),
	})
	want := domain.ToolSnapshot{
		Name: "a", Title: ptr("A"), Description: "does a",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: domain.ToolAnnotations{ReadOnlyHint: ptr(true), DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}

func TestSnapshotFromUpstreamNullAndOddValues(t *testing.T) {
	for _, ann := range []string{`null`, `[]`, `"x"`, `7`, `{`, ``} {
		got := domain.SnapshotFromUpstream(kernel.UpstreamTool{Name: "a", Annotations: json.RawMessage(ann)})
		if !reflect.DeepEqual(got.Annotations, domain.ToolAnnotations{}) {
			t.Errorf("%q: %+v", ann, got.Annotations)
		}
	}
	got := domain.SnapshotFromUpstream(kernel.UpstreamTool{Name: "a", OutputSchema: json.RawMessage(` null `)})
	if got.OutputSchema != nil {
		t.Fatal("a JSON null output schema is absent, as `?? null` in TypeScript")
	}
	got = domain.SnapshotFromUpstream(kernel.UpstreamTool{Name: "a", Title: ptr(""), Description: ptr("")})
	if got.Title == nil || *got.Title != "" {
		t.Fatal("an empty title stays an empty title")
	}
}

func TestAnnotationsJSON(t *testing.T) {
	b, err := json.Marshal(domain.ToolAnnotations{})
	if err != nil || string(b) != `{}` {
		t.Fatalf("%s %v", b, err)
	}
	b, _ = json.Marshal(domain.ToolAnnotations{ReadOnlyHint: ptr(true), DestructiveHint: ptr(false), IdempotentHint: ptr(true), OpenWorldHint: ptr(false)})
	if string(b) != `{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}` {
		t.Fatalf("an explicit false is kept: %s", b)
	}
	var a domain.ToolAnnotations
	if err := json.Unmarshal([]byte(`{"readOnlyHint":false}`), &a); err != nil || a.ReadOnlyHint == nil || *a.ReadOnlyHint {
		t.Fatal("round trip", a, err)
	}
}

func TestEventConstructors(t *testing.T) {
	if got := domain.ToolsChangedEvent("s", []string{"s.a"}); got.Type != "catalog.tools_changed" || !reflect.DeepEqual(got.Payload, map[string]any{"server": "s", "refs": []string{"s.a"}}) {
		t.Fatalf("%#v", got)
	}
	if got := domain.ToolsRemovedEvent("s", []string{"s.a"}); got.Type != "catalog.tools_removed" || !reflect.DeepEqual(got.Payload, map[string]any{"server": "s", "refs": []string{"s.a"}}) {
		t.Fatalf("%#v", got)
	}
	if got := domain.ServerChangedEvent("s"); got.Type != "catalog.server_changed" || !reflect.DeepEqual(got.Payload, map[string]any{"server": "s"}) {
		t.Fatalf("%#v", got)
	}
	b, err := json.Marshal(domain.ToolsChangedEvent("s", []string{"s.a", "s.b"}).Payload)
	if err != nil || string(b) != `{"refs":["s.a","s.b"],"server":"s"}` {
		t.Fatalf("%s %v", b, err)
	}
}
