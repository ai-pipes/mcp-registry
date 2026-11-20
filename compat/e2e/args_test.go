//go:build compat && e2e

package e2e_test

import "testing"

func TestPlaywrightArgsRefusesWhatCouldPassWithoutTesting(t *testing.T) {
	for _, a := range []string{
		"--list", "--pass-with-no-tests", "--retries", "--retries=2", "--reporter=dot", "--reporter",
		"-c", "-cother.config.ts", "-c=other.config.ts", "--config", "--config=other.config.ts",
		"--grep-invert", "--grep-invert=tokens", "--shard", "--shard=1/2", "--last-failed", "--only-changed", "--only-changed=main",
	} {
		if !refusedPlaywrightArg(a) {
			t.Errorf("%q is accepted, want refused", a)
		}
	}
	// Negative control: what selecting flows needs passes.
	for _, a := range []string{"03-tokens.spec.ts", "--repeat-each=30", "-x", "--grep", "tokens"} {
		if refusedPlaywrightArg(a) {
			t.Errorf("%q is refused, want accepted", a)
		}
	}
}
