package infra

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// vectorLiteral is the text pgvector reads a vector from: "[" + the components joined by commas + "]".
// It is sent as a TEXT parameter and cast in SQL (`$n::text::vector`); no pgvector codec is
// registered in pgx here, so the database does the parsing (strtof into float4).
//
// Every component is written with the shortest decimal that round-trips float32
// (strconv 'g', -1, 32), so the stored float4 is exactly the float32 handed in. The TypeScript
// server writes the shortest float64 decimal of the same value: different digits, the same float4.
// Go may use an exponent form ("1e-07", "3.4028235e+38"), which pgvector accepts. Negative zero is
// written as "0", as JavaScript's number-to-string does (it would be stored as -0 otherwise).
// NaN and infinities are refused here, before any SQL: pgvector rejects them anyway, but with a
// message that quotes the whole literal.
func vectorLiteral(v []float32) (string, error) {
	var b strings.Builder
	b.Grow(len(v)*12 + 2)
	b.WriteByte('[')
	for i, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", fmt.Errorf("vector component %d is not a finite number", i)
		}
		if i > 0 {
			b.WriteByte(',')
		}
		if x == 0 {
			b.WriteByte('0')
			continue
		}
		b.WriteString(strconv.FormatFloat(f, 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}
