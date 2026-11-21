package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// ToolAnnotations are the four boolean MCP hints. A nil field is absent; an explicit false is
// kept. Other annotation keys (title...) are dropped when a snapshot is taken.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// ToolSnapshot is what upstream tools/list says about a tool. InputSchema and OutputSchema are
// the upstream JSON as received (stored without loss); OutputSchema nil means absent.
type ToolSnapshot struct {
	Name         string
	Title        *string // nil: absent
	Description  string  // "" when absent
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage // nil: absent
	Annotations  ToolAnnotations
}

// SnapshotFromUpstream normalises an upstream tool: absent fields get their stored defaults, a
// JSON null output schema is absent, and only boolean hints are kept.
func SnapshotFromUpstream(u kernel.UpstreamTool) ToolSnapshot {
	snap := ToolSnapshot{Name: u.Name, Title: u.Title, InputSchema: u.InputSchema}
	if u.Description != nil {
		snap.Description = *u.Description
	}
	if !isJSONNull(u.OutputSchema) {
		snap.OutputSchema = u.OutputSchema
	}
	var raw map[string]any
	if json.Unmarshal(u.Annotations, &raw) == nil { // not an object (or absent): no hints
		hint := func(key string) *bool {
			if v, ok := raw[key].(bool); ok {
				return &v
			}
			return nil
		}
		snap.Annotations = ToolAnnotations{
			ReadOnlyHint: hint("readOnlyHint"), DestructiveHint: hint("destructiveHint"),
			IdempotentHint: hint("idempotentHint"), OpenWorldHint: hint("openWorldHint"),
		}
	}
	return snap
}

func isJSONNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// ToolState is a stored tool, for RehydrateTool.
type ToolState struct {
	ToolSnapshot
	Server    string
	Hidden    bool       // set by an admin, survives synchronisation
	RemovedAt *time.Time // nil: upstream still offers the tool
	SyncedAt  time.Time
}

// Tool is a tool snapshot plus the admin's hidden flag (which outlives synchronisation).
type Tool struct {
	s      ToolState
	events []kernel.Event
}

// RehydrateTool restores a stored tool: no validation, no events.
func RehydrateTool(s ToolState) *Tool { return &Tool{s: s} }

// Server returns the tool's Server.
func (t *Tool) Server() string { return t.s.Server }

// Name returns the tool's Name.
func (t *Tool) Name() string { return t.s.Name }

// Title returns the tool's Title.
func (t *Tool) Title() *string { return t.s.Title }

// Description returns the tool's Description.
func (t *Tool) Description() string { return t.s.Description }

// InputSchema returns the tool's InputSchema.
func (t *Tool) InputSchema() json.RawMessage { return t.s.InputSchema }

// OutputSchema returns the tool's OutputSchema.
func (t *Tool) OutputSchema() json.RawMessage { return t.s.OutputSchema }

// Annotations returns the tool's Annotations.
func (t *Tool) Annotations() ToolAnnotations { return t.s.Annotations }

// Hidden returns the tool's Hidden.
func (t *Tool) Hidden() bool { return t.s.Hidden }

// RemovedAt returns the tool's RemovedAt.
func (t *Tool) RemovedAt() *time.Time { return t.s.RemovedAt }

// SyncedAt returns the tool's SyncedAt.
func (t *Tool) SyncedAt() time.Time { return t.s.SyncedAt }

// Snapshot is the upstream-visible part of the tool.
func (t *Tool) Snapshot() ToolSnapshot { return t.s.ToolSnapshot }

// Ref is the agent-facing name "<server>.<tool>".
func (t *Tool) Ref() string { return FormatToolRef(t.s.Server, t.s.Name) }

// IsRemoved reports that upstream no longer offers the tool.
func (t *Tool) IsRemoved() bool { return t.s.RemovedAt != nil }

// Visible reports whether the tool is searchable and callable, i.e. not hidden by an admin and still offered by upstream.
func (t *Tool) Visible() bool { return !t.s.Hidden && !t.IsRemoved() }

// SetHidden reports false when nothing changed. A tool upstream no longer offers cannot be
// hidden or unhidden (not_found). A change records one tools_changed event.
func (t *Tool) SetHidden(hidden bool, _ time.Time) (bool, error) {
	if t.IsRemoved() {
		return false, newError(CodeNotFound, `tool "`+t.Ref()+`" not found`)
	}
	if t.s.Hidden == hidden {
		return false, nil
	}
	t.s.Hidden = hidden
	t.events = append(t.events, ToolsChangedEvent(t.s.Server, []string{t.Ref()}))
	return true, nil
}

// PullEvents drains the events recorded since the last call.
func (t *Tool) PullEvents() []kernel.Event {
	out := t.events
	t.events = nil
	return out
}

// jsonValue decodes JSON the way JavaScript's JSON.parse sees it: numbers are float64, object
// key order is irrelevant. ok is false when the text is not decodable (e.g. 1e999).
func jsonValue(raw json.RawMessage) (v any, ok bool) {
	if isJSONNull(raw) {
		return nil, true
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// sameJSON mirrors the TypeScript canonical() comparison: equality of the parsed values
// (sorted-key JSON of JS-parsed numbers), so key order, whitespace and 1 vs 1.0 do not count.
// Text that cannot be decoded is compared byte for byte.
func sameJSON(a, b json.RawMessage) bool {
	va, okA := jsonValue(a)
	vb, okB := jsonValue(b)
	if okA && okB {
		return reflect.DeepEqual(va, vb)
	}
	return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
}

func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameContent(a, b ToolSnapshot) bool {
	return sameString(a.Title, b.Title) && a.Description == b.Description &&
		sameJSON(a.InputSchema, b.InputSchema) && sameJSON(a.OutputSchema, b.OutputSchema) &&
		annotationsEqual(a.Annotations, b.Annotations)
}

func annotationsEqual(a, b ToolAnnotations) bool {
	eq := func(x, y *bool) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.ReadOnlyHint, b.ReadOnlyHint) && eq(a.DestructiveHint, b.DestructiveHint) &&
		eq(a.IdempotentHint, b.IdempotentHint) && eq(a.OpenWorldHint, b.OpenWorldHint)
}

// Reconciliation is the result of merging an upstream tools/list into the stored tools.
type Reconciliation struct {
	// Upserts are the tools to write: new, changed, revived (in upstream order), then newly
	// removed (in stored order). Unchanged ones are absent.
	Upserts []*Tool
	// ChangedRefs: new, changed and revived tools (one tools_changed event).
	ChangedRefs []string
	// AddedRefs: the part of ChangedRefs that was absent or removed before.
	AddedRefs   []string
	RemovedRefs []string
	// Events: tools_changed (if any ChangedRefs), then tools_removed (if any RemovedRefs).
	Events []kernel.Event
}

// ReconcileTools merges an upstream tools/list snapshot into the stored tools of one server: new
// and changed tools are upserted, vanished ones are marked removed, returned ones revived;
// hidden is kept. Duplicate names in incoming: the first wins.
func ReconcileTools(server string, existing []*Tool, incoming []ToolSnapshot, now time.Time) Reconciliation {
	byName := make(map[string]*Tool, len(existing))
	for _, t := range existing {
		byName[t.Name()] = t // a later stored row wins, as with a Map built from entries
	}
	seen := make(map[string]bool, len(incoming))
	r := Reconciliation{Upserts: []*Tool{}, ChangedRefs: []string{}, AddedRefs: []string{}, RemovedRefs: []string{}, Events: []kernel.Event{}}
	for _, snap := range incoming {
		if seen[snap.Name] {
			continue
		}
		seen[snap.Name] = true
		old := byName[snap.Name]
		if old != nil && !old.IsRemoved() && sameContent(old.Snapshot(), snap) {
			continue
		}
		next := RehydrateTool(ToolState{ToolSnapshot: snap, Server: server, Hidden: old != nil && old.Hidden(), SyncedAt: now})
		r.Upserts = append(r.Upserts, next)
		r.ChangedRefs = append(r.ChangedRefs, next.Ref())
		if old == nil || old.IsRemoved() {
			r.AddedRefs = append(r.AddedRefs, next.Ref())
		}
	}
	for _, old := range existing {
		if seen[old.Name()] || old.IsRemoved() {
			continue
		}
		removedAt := now
		gone := RehydrateTool(ToolState{
			ToolSnapshot: old.Snapshot(), Server: server, Hidden: old.Hidden(), RemovedAt: &removedAt, SyncedAt: old.SyncedAt(),
		})
		r.Upserts = append(r.Upserts, gone)
		r.RemovedRefs = append(r.RemovedRefs, gone.Ref())
	}
	if len(r.ChangedRefs) > 0 {
		r.Events = append(r.Events, ToolsChangedEvent(server, r.ChangedRefs))
	}
	if len(r.RemovedRefs) > 0 {
		r.Events = append(r.Events, ToolsRemovedEvent(server, r.RemovedRefs))
	}
	return r
}
