//go:build compat

package compat_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// scripts/check-release-manifest.sh judges the manifest list every release tag names (and the
// rehearsal's dry assembly). Docker-free (bash and jq only), run by make go-verify: each input must
// get the stated exit code, so an input the script cannot judge (empty, not JSON, several documents)
// is refused instead of passing for want of problems.

const releaseManifestScript = "../scripts/check-release-manifest.sh"

func fakeDigest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func TestReleaseManifestCheckExitCodes(t *testing.T) {
	amd64, arm64, other := fakeDigest('a'), fakeDigest('b'), fakeDigest('c')
	image := func(digest, arch string) string {
		return `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","size":1,"platform":{"os":"linux","architecture":"` + arch + `"}}`
	}
	attestation := func(digest, describes string) string {
		return `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","size":1,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest","vnd.docker.reference.digest":"` + describes + `"}}`
	}
	index := func(manifests ...string) string {
		return `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` + strings.Join(manifests, ",") + `]}`
	}
	right := index(image(amd64, "amd64"), image(arm64, "arm64"))

	cases := []struct {
		name  string
		args  []string
		input string
		want  int
	}{
		{"empty input", []string{amd64, arm64}, "", 1},
		{"only whitespace", []string{amd64, arm64}, " \n", 1},
		{"not JSON", []string{amd64, arm64}, "not json", 1},
		{"a JSON string", []string{amd64, arm64}, `"index"`, 1},
		{"null", []string{amd64, arm64}, "null", 1},
		{"an array of indexes", []string{amd64, arm64}, "[" + right + "]", 1},
		{"two objects", []string{amd64, arm64}, right + "\n" + right, 1},
		{"an index with one platform", []string{amd64, arm64}, index(image(amd64, "amd64")), 1},
		{"an index with three platforms", []string{amd64, arm64}, index(image(amd64, "amd64"), image(arm64, "arm64"), image(other, "s390x")), 1},
		{"a single image manifest", []string{amd64, arm64}, image(amd64, "amd64"), 1},
		{"the right two", []string{amd64, arm64}, right, 0},
		{"the right two, a Docker manifest list", []string{amd64, arm64}, strings.Replace(right, "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json", 1), 0},
		{"the right two with their attestations", []string{amd64, arm64}, index(image(amd64, "amd64"), image(arm64, "arm64"), attestation(fakeDigest('d'), amd64), attestation(fakeDigest('e'), arm64)), 0},
		{"an attestation of another image", []string{amd64, arm64}, index(image(amd64, "amd64"), image(arm64, "arm64"), attestation(fakeDigest('d'), other)), 1},
		{"the digests swapped", []string{arm64, amd64}, right, 1},
		{"another amd64 image", []string{other, arm64}, right, 1},
		{"one argument", []string{amd64}, right, 2},
		{"a malformed digest", []string{"sha256:abc", arm64}, right, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{releaseManifestScript}, c.args...)...)
			cmd.Stdin = strings.NewReader(c.input)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("running the script: %v", err)
				}
				code = exit.ExitCode()
			}
			if code != c.want {
				t.Errorf("exit code %d, want %d; output:\n%s", code, c.want, out)
			}
		})
	}
}
