package onnx

import "testing"

// parityRequiredEnv is set by make go-test-onnx-parity: there the parity tests are the point of the
// run, so a missing input fails them instead of skipping them (a skipped test reports ok).
const parityRequiredEnv = "ONNX_TEST_PARITY_REQUIRED"

// skipper is the part of testing.TB that missingInput uses (so the helper can be tested).
type skipper interface {
	Helper()
	Fatal(args ...any)
	Skip(args ...any)
}

// missingInput skips the test, or fails it when ONNX_TEST_PARITY_REQUIRED=1.
func missingInput(t skipper, msg string, getenv func(string) string) {
	t.Helper()
	if getenv(parityRequiredEnv) == "1" {
		t.Fatal(msg + " (" + parityRequiredEnv + "=1: the parity run must not skip)")
		return
	}
	t.Skip(msg)
}

type recordedSkipper struct{ fatal, skip bool }

func (*recordedSkipper) Helper()        {}
func (r *recordedSkipper) Fatal(...any) { r.fatal = true }
func (r *recordedSkipper) Skip(...any)  { r.skip = true }

func TestMissingInputSkipsUnlessParityIsRequired(t *testing.T) {
	env := func(v string) func(string) string {
		return func(string) string { return v }
	}
	var r recordedSkipper
	missingInput(&r, "x", env(""))
	if !r.skip || r.fatal {
		t.Fatalf("without the variable the test must skip: %+v", r)
	}
	r = recordedSkipper{}
	missingInput(&r, "x", env("1"))
	if r.skip || !r.fatal {
		t.Fatalf("with the variable the test must fail: %+v", r)
	}
}
