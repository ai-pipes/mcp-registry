package http

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The input checks of the four meta-tools, with the reference's wording (zod 4 behind the
// TypeScript SDK): each problem is "<dotted path>: <message>", several are joined with ", " in the
// order of the schema's keys, unknown keys are ignored. The answer to a refused input is an isError
// result "Input validation error: Invalid arguments for tool <name>: <problems>".

// issues collects the problems of one input.
type issues []string

func (is *issues) add(path, msg string) { *is = append(*is, path+": "+msg) }

// kindOf is the type zod names for a JSON value ("undefined" for an absent one).
func kindOf(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "undefined"
	}
	switch raw[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

func invalidType(expected, received string) string {
	return "Invalid input: expected " + expected + ", received " + received
}

// str is z.string().min(lo).max(hi); ok is false when the value is not a string. Lengths are code
// points, as zod 4.6.7 measures them ($ZodCheckMaxLength: util.codePointLength, not String.length):
// an astral character is one. A lone surrogate escape is decoded as U+FFFD, one code point, as
// the lone surrogate JSON.parse keeps is one for zod.
func (is *issues) str(path string, raw json.RawMessage, lo, hi int) (string, bool) {
	if k := kindOf(raw); k != "string" {
		is.add(path, invalidType("string", k))
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		is.add(path, invalidType("string", "string"))
		return "", false
	}
	n := utf8.RuneCountInString(s)
	if n < lo {
		is.add(path, "Too small: expected string to have >="+strconv.Itoa(lo)+" characters")
	}
	if n > hi {
		is.add(path, "Too big: expected string to have <="+strconv.Itoa(hi)+" characters")
	}
	return s, true
}

// integer is z.number().int().min(lo).max(hi).
func (is *issues) integer(path string, raw json.RawMessage, lo, hi int) int {
	if k := kindOf(raw); k != "number" {
		is.add(path, invalidType("number", k))
		return 0
	}
	f, err := strconv.ParseFloat(string(bytes.TrimSpace(raw)), 64)
	if err != nil || f != math.Trunc(f) || math.IsInf(f, 0) {
		is.add(path, invalidType("int", "number"))
	}
	if f < float64(lo) {
		is.add(path, "Too small: expected number to be >="+strconv.Itoa(lo))
	}
	if f > float64(hi) {
		is.add(path, "Too big: expected number to be <="+strconv.Itoa(hi))
	}
	return int(f)
}

// stringList is z.array(z.string().min(itemLo).max(itemHi)).min(lo).max(hi).
func (is *issues) stringList(path string, raw json.RawMessage, lo, hi, itemLo, itemHi int) []string {
	if k := kindOf(raw); k != "array" {
		is.add(path, invalidType("array", k))
		return nil
	}
	var items []json.RawMessage
	_ = json.Unmarshal(raw, &items) // valid JSON array
	out := make([]string, 0, len(items))
	for i, it := range items {
		if s, ok := is.str(path+"."+strconv.Itoa(i), it, itemLo, itemHi); ok {
			out = append(out, s)
		}
	}
	if len(items) < lo {
		is.add(path, "Too small: expected array to have >="+strconv.Itoa(lo)+" items")
	}
	if len(items) > hi {
		is.add(path, "Too big: expected array to have <="+strconv.Itoa(hi)+" items")
	}
	return out
}

// record is z.record(z.string(), z.unknown()): the raw object as given.
func (is *issues) record(path string, raw json.RawMessage) json.RawMessage {
	if k := kindOf(raw); k != "object" {
		is.add(path, invalidType("record", k))
		return nil
	}
	return bytes.TrimSpace(raw)
}

func (is issues) text(tool string) string {
	return "Input validation error: Invalid arguments for tool " + tool + ": " + strings.Join(is, ", ")
}

// searchInput is search_tools' input: query 1..500, limit integer 1..25 (default 10), servers
// optional, 1..20 slugs of 1..64.
type searchInput struct {
	query      string
	limit      int
	servers    []string
	hasServers bool
}

func parseSearch(args map[string]json.RawMessage) (searchInput, issues) {
	var is issues
	in := searchInput{limit: 10}
	in.query, _ = is.str("query", args["query"], 1, 500)
	if raw, ok := args["limit"]; ok {
		in.limit = is.integer("limit", raw, 1, 25)
	}
	if raw, ok := args["servers"]; ok {
		in.servers, in.hasServers = is.stringList("servers", raw, 1, 20, 1, 64), true
	}
	return in, is
}

// parseTool is the input of describe_tool: tool 1..300.
func parseTool(args map[string]json.RawMessage) (string, issues) {
	var is issues
	ref, _ := is.str("tool", args["tool"], 1, 300)
	return ref, is
}

// parseCall is the input of call_tool: tool 1..300, arguments an object (omitted: none, nil).
func parseCall(args map[string]json.RawMessage) (string, json.RawMessage, issues) {
	var is issues
	ref, _ := is.str("tool", args["tool"], 1, 300)
	var arguments json.RawMessage
	if raw, ok := args["arguments"]; ok {
		arguments = is.record("arguments", raw)
	}
	return ref, arguments, is
}
