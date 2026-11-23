package http

import (
	"bytes"
	"encoding/json"
)

// marshalJS is JSON.stringify of v for the values this package writes: compact, `<`, `>` and `&`
// not escaped, U+2028 and U+2029 written raw (encoding/json escapes both even without HTML
// escaping). A json.RawMessage inside v is compacted and kept otherwise.
func marshalJS(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return rawLineSeparators(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// rawLineSeparators replaces the escapes   and   in a JSON text by the characters
// themselves, which is the same JSON value written as JavaScript writes it. Backslashes come in
// escape pairs inside strings (and nowhere else in JSON), so an escaped backslash followed by
// "u2028" is left alone.
func rawLineSeparators(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			out = append(out, b[i])
			continue
		}
		if b[i+1] == 'u' && i+5 < len(b) && b[i+2] == '2' && b[i+3] == '0' && b[i+4] == '2' && (b[i+5] == '8' || b[i+5] == '9') {
			if b[i+5] == '8' {
				out = append(out, " "...)
			} else {
				out = append(out, " "...)
			}
			i += 5
			continue
		}
		out = append(out, b[i], b[i+1])
		i++
	}
	return out
}
