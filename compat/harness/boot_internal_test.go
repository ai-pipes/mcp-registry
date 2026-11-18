//go:build compat

package harness

import (
	"strings"
	"testing"
	"time"
)

func sessionObjects(t *testing.T) string {
	t.Helper()
	c, err := dockerErr("ps", "-aq", "--filter", "label=compat.session="+session)
	if err != nil {
		t.Fatal(err)
	}
	n, err := dockerErr("network", "ls", "-q", "--filter", "label=compat.session="+session)
	if err != nil {
		t.Fatal(err)
	}
	return c + "|" + n
}

func TestFailedBootCleansUpAndExplains(t *testing.T) {
	const badKey = "bad-key-that-must-not-leak"
	before := sessionObjects(t)
	start := time.Now()
	s, err := boot(Options{Env: map[string]string{"SECRETS_KEY": badKey}}, false)
	if err == nil {
		s.teardown()
		t.Fatal("boot with an invalid SECRETS_KEY succeeded")
	}
	if time.Since(start) > 60*time.Second {
		t.Fatalf("the boot failure was not detected fast: %s", time.Since(start))
	}
	if !strings.Contains(err.Error(), "the app container is not running") || !strings.Contains(err.Error(), "SECRETS_KEY: must be base64") {
		t.Fatalf("error lacks the reason and the app log: %v", err)
	}
	if strings.Contains(err.Error(), badKey) {
		t.Fatalf("error quotes the key: %v", err)
	}
	if after := sessionObjects(t); after != before {
		t.Fatalf("leftovers after a failed boot: before %q after %q", before, after)
	}
}

func TestBootWithAMissingImageCleansUp(t *testing.T) {
	t.Setenv("COMPAT_IMAGE", "mcp-registry:does-not-exist")
	before := sessionObjects(t)
	if _, err := boot(Options{}, false); err == nil {
		t.Fatal("boot with a missing image succeeded")
	}
	if after := sessionObjects(t); after != before {
		t.Fatalf("leftovers: before %q after %q", before, after)
	}
}
