//go:build compat

package compat_test

import (
	"testing"
)

// TestMCPReaderIsLenient pins the reader of the scenarios (mcpMessages): an event stream may open
// with a priming event (an id and empty data), carry comments, id: and retry: lines and unknown
// events, and hold several messages; the answer is the message whose id is the request's.
func TestMCPReaderIsLenient(t *testing.T) {
	const answer = `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`
	for _, c := range []struct {
		name, ct, body string
		ids            []any // ids of the messages the reader finds
	}{
		{"a JSON body", "application/json", answer, []any{float64(7)}},
		{"a JSON body with a charset", "application/json; charset=utf-8", answer, []any{float64(7)}},
		{"one event", "text/event-stream", "event: message\ndata: " + answer + "\n\n", []any{float64(7)}},
		{"with a charset", "text/event-stream; charset=utf-8", "data: " + answer + "\n\n", []any{float64(7)}},
		{"a priming event first", "text/event-stream", "id: 1\ndata:\n\nid: 2\nevent: message\ndata: " + answer + "\n\n", []any{float64(7)}},
		{"a priming event with retry", "text/event-stream", "id: 0\nretry: 1000\ndata: \n\ndata: " + answer + "\n\n", []any{float64(7)}},
		{"comments, id and retry lines", "text/event-stream", ": keep-alive\nid: 5\nretry: 3000\nevent: message\ndata: " + answer + "\n\n: bye\n\n", []any{float64(7)}},
		{"an event of another type", "text/event-stream", "event: ping\ndata: {}\n\nevent: message\ndata: " + answer + "\n\n", []any{float64(7)}},
		{"CRLF line ends", "text/event-stream", "event: message\r\ndata: " + answer + "\r\n\r\n", []any{float64(7)}},
		{"two messages", "text/event-stream", `data: {"jsonrpc":"2.0","id":6,"result":{}}` + "\n\ndata: " + answer + "\n\n", []any{float64(6), float64(7)}},
		{"data over several lines", "text/event-stream", "data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":7,\"result\":{}}\n\n", []any{float64(7)}},
	} {
		msgs, _ := mcpMessages(t, c.ct, []byte(c.body))
		var got []any
		for _, m := range msgs {
			got = append(got, m.ID)
		}
		if len(got) != len(c.ids) {
			t.Errorf("%s: ids %v, want %v", c.name, got, c.ids)
			continue
		}
		for i := range got {
			if got[i] != c.ids[i] {
				t.Errorf("%s: ids %v, want %v", c.name, got, c.ids)
			}
		}
	}
	if !mcpIsStream("text/event-stream; charset=utf-8") || !mcpIsJSON("application/json; charset=utf-8") || mcpIsJSON("text/html") {
		t.Error("content types are not compared by prefix")
	}

	// Selection: two message events with different ids; the one with the request's id is the answer.
	msgs, _ := mcpMessages(t, "text/event-stream", []byte("id: 1\ndata:\n\ndata: "+`{"jsonrpc":"2.0","id":6,"result":{"which":"other"}}`+"\n\ndata: "+answer+"\n\n"))
	if m, ok := mcpSelect(msgs, float64(7)); !ok || m.Result["ok"] != true {
		t.Errorf("selected %+v (found %v), want the message with id 7", m, ok)
	}
	if m, ok := mcpSelect(msgs, float64(6)); !ok || m.Result["which"] != "other" {
		t.Errorf("selected %+v (found %v), want the message with id 6", m, ok)
	}
	if _, ok := mcpSelect(msgs, float64(8)); ok {
		t.Error("a message was selected for an id nobody answered")
	}
}
