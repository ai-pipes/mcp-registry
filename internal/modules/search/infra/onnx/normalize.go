package onnx

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// nulReplacement stands for U+0000, which would end the C string the tokenizer receives. A
// plane-15 private-use code point is not in the vocabulary, so the tokenizer turns it into <unk>
// exactly where the JavaScript tokenizer turns the NUL into one (measured on every NUL text).
const nulReplacement = rune(0xF0000)

// fullwidthTilde is U+FF5E, around which NFKC is applied piecewise (as in the reference).
var fullwidthTilde = string(rune(0xFF5E))

// JSNormalize ports Precompiled.normalize of @huggingface/tokenizers 0.2.0, the normaliser
// transformers.js runs first (it does not use the model's precompiled character map):
//   - dropped: U+0001-U+0008, U+000B, U+000E-U+001F, U+007F, U+008F, U+009F;
//   - mapped to a space: U+0009, U+000A, U+000C, U+000D, U+00A0, U+1680, U+2000-U+200F, U+2028,
//     U+2029, U+202F, U+205F, U+2581, U+3000, U+FEFF, U+FFFD;
//   - then NFKC, each run between two U+FF5E separately.
//
// Invalid UTF-8 reads as U+FFFD and so becomes a space.
func JSNormalize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case dropped(r):
		case spaceLike(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	parts := strings.Split(b.String(), fullwidthTilde)
	for i, p := range parts {
		parts[i] = norm.NFKC.String(p)
	}
	return strings.Join(parts, fullwidthTilde)
}

func dropped(r rune) bool {
	return (r >= 0x01 && r <= 0x08) || r == 0x0B || (r >= 0x0E && r <= 0x1F) || r == 0x7F || r == 0x8F || r == 0x9F
}

func spaceLike(r rune) bool {
	switch r {
	case 0x09, 0x0A, 0x0C, 0x0D, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x2581, 0x3000, 0xFEFF, 0xFFFD:
		return true
	}
	return r >= 0x2000 && r <= 0x200F
}

// PrepareText is what the tokenizer receives: the text after JSNormalize, with U+0000 replaced.
func PrepareText(text string) string {
	return strings.ReplaceAll(JSNormalize(text), "\x00", string(nulReplacement))
}
