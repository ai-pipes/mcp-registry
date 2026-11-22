package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxPassageChars is the longest text handed to the embedder or stored as the full-text body, in
// UTF-16 code units (JavaScript's string length).
const MaxPassageChars = 2000

// SplitPairStandIn is what stands in the embedder text for the lone high surrogate that
// JavaScript's slice leaves when a cut falls inside an astral character. Go strings cannot hold
// a lone surrogate. The tokenizer reads the lone surrogate as <unk>, and measured against
// transformers.js the code point U+F0000 (private use, not in the vocabulary) gives exactly the
// same token ids, whereas U+FFFD would become a space.
const SplitPairStandIn = "\U000F0000"

const replacement = "�"

// UTF16Len is the length of s in UTF-16 code units.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += unitsOf(r)
	}
	return n
}

func unitsOf(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// cutUnits is s.slice(0, n) over UTF-16 code units. split reports that the cut fell inside an
// astral character, where JavaScript keeps its high surrogate: kept is then the text before it.
func cutUnits(s string, n int) (kept string, split bool) {
	units := 0
	for i, r := range s {
		w := unitsOf(r)
		if units+w > n {
			return s[:i], w == 2 && units == n-1
		}
		units += w
	}
	return s, false
}

// PassageSource is what a tool's passage is built from.
type PassageSource struct {
	ServerName        string
	ServerDescription string
	ToolName          string
	// Title is empty when the tool has none (null and "" are the same).
	Title       string
	Description string
}

// BuildPassage is the text the embedder reads for a tool:
// "<server name>. <server description>\n<tool name> <title>\n<description>", cut to
// MaxPassageChars UTF-16 units, nothing trimmed. When the cut splits a surrogate pair the text
// ends with SplitPairStandIn (see there).
func BuildPassage(s PassageSource) string {
	head := s.ToolName
	if s.Title != "" {
		head = s.ToolName + " " + s.Title
	}
	text := s.ServerName + ". " + s.ServerDescription + "\n" + head + "\n" + s.Description
	kept, split := cutUnits(text, MaxPassageChars)
	if split {
		return kept + SplitPairStandIn
	}
	return kept
}

// SchemaText is the full-text body of a tool: the names of the parameters of an input schema and
// their descriptions, nested ones included, joined by single spaces and cut to MaxPassageChars
// units. raw is the schema's JSON as stored; the order of properties is that of
// Object.entries over the parsed document (integer-like keys first, ascending, then document
// order). Anything that is not a JSON object has no words. A cut inside a surrogate pair ends the
// text with U+FFFD, which is what Node stores for the lone surrogate.
func SchemaText(raw []byte) (string, error) {
	root, err := parseJSON(raw)
	if err != nil {
		return "", err
	}
	obj, ok := root.(*object)
	if !ok {
		return "", nil
	}
	// The schema's own description repeats the tool's: only the parameters count.
	var out []string
	collect(&object{keys: []string{"properties", "items"}, vals: map[string]any{
		"properties": obj.vals["properties"], "items": obj.vals["items"],
	}}, &out)
	kept, split := cutUnits(strings.Join(out, " "), MaxPassageChars)
	if split {
		return kept + replacement, nil
	}
	return kept, nil
}

func collect(schema any, out *[]string) {
	obj, ok := schema.(*object)
	if !ok {
		return
	}
	if d, ok := obj.vals["description"].(string); ok {
		*out = append(*out, d)
	}
	if props, ok := obj.vals["properties"].(*object); ok {
		for _, name := range props.entries() {
			*out = append(*out, name)
			collect(props.vals[name], out)
		}
	}
	collect(obj.vals["items"], out)
}

// object is a JSON object with its keys in document order; a repeated key keeps its first
// position and takes the last value, as JSON.parse does.
type object struct {
	keys []string
	vals map[string]any
}

// entries are the keys in Object.entries order: array indices (canonical non-negative integers
// below 2^32-1) ascending, then the others in document order.
func (o *object) entries() []string {
	var idx, rest []string
	for _, k := range o.keys {
		if isArrayIndex(k) {
			idx = append(idx, k)
		} else {
			rest = append(rest, k)
		}
	}
	// all canonical: shorter is smaller, equal lengths compare as text
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && indexLess(idx[j], idx[j-1]); j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
	return append(idx, rest...)
}

func indexLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func isArrayIndex(k string) bool {
	if k == "" || len(k) > 10 {
		return false
	}
	if k[0] == '0' {
		return k == "0"
	}
	var n uint64
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
		n = n*10 + uint64(k[i]-'0')
	}
	return n < 1<<32-1
}

var errMalformed = errors.New("malformed JSON")

// parseJSON decodes one JSON value keeping object key order: *object, []any, string, or another
// scalar as nil/float64/bool.
func parseJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errMalformed
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w", errMalformed)
	}
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil
	}
	switch d {
	case '{':
		o := &object{vals: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			key, ok := kt.(string)
			if err != nil || !ok {
				return nil, errMalformed
			}
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			if _, seen := o.vals[key]; !seen {
				o.keys = append(o.keys, key)
			}
			o.vals[key] = v
		}
		if _, err := dec.Token(); err != nil {
			return nil, errMalformed
		}
		return o, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, errMalformed
		}
		return arr, nil
	}
	return nil, errMalformed
}
