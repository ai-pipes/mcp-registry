package infra

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// parseVector reads the text form of a pgvector value ("[1,2,3]") back into float32s. Production code
// never reads vectors (the port has no getter): this is the tests' way to see what was stored.
func parseVector(text string) ([]float32, error) {
	body := strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
	if body == "" {
		return []float32{}, nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, err
		}
		out[i] = float32(v)
	}
	return out, nil
}

func TestVectorLiteralIsBracketedCommaSeparatedShortestFloat32(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []float32
		want string
	}{
		{"empty", []float32{}, "[]"},
		{"plain values", []float32{1, -1, 0.25}, "[1,-1,0.25]"},
		{"shortest float32 digits, not the float64 expansion of 0.1", []float32{0.1, 1.0 / 3}, "[0.1,0.33333334]"},
		{"negative zero is written as 0, as JavaScript's join does", []float32{float32(math.Copysign(0, -1)), 0}, "[0,0]"},
		{"tiny and huge magnitudes keep an exponent form pgvector reads", []float32{1e-7, -1e-7, math.MaxFloat32, math.SmallestNonzeroFloat32}, "[1e-07,-1e-07,3.4028235e+38,1e-45]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vectorLiteral(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVectorLiteralRoundTripsEveryFloat32(t *testing.T) {
	in := []float32{0.1, -3.5e-8, 123456789, 1.17549435e-38, 7.0e-45, 0.99999994}
	lit, err := vectorLiteral(in)
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseVector(lit)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if back[i] != in[i] {
			t.Errorf("component %d: %v became %v through %q", i, in[i], back[i], lit)
		}
	}
}

func TestVectorLiteralRefusesNonFiniteBeforeSQL(t *testing.T) {
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		_, err := vectorLiteral([]float32{1, bad, 2})
		if err == nil {
			t.Fatalf("%v accepted", bad)
		}
		if !strings.Contains(err.Error(), "component 1") || strings.Contains(err.Error(), "NaN") || strings.Contains(err.Error(), "Inf") {
			t.Errorf("error %q must name the position and nothing else", err)
		}
	}
}
