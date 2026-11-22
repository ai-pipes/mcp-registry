package onnx

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// pins.env is the only list of the native libraries' URLs and SHA-256 values: `make native-libs`
// (Makefile include plus fetch.sh) and the Dockerfile stage go-native (the same fetch.sh) read it.
// These tests fail when a second copy appears, or when a platform loses a pin.

func readPins(t *testing.T) map[string]string {
	t.Helper()
	data, err := fs.ReadFile(os.DirFS("native"), "pins.env")
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("pins.env: not NAME=value: %q", line)
		}
		pins[k] = v
	}
	return pins
}

func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("../../../../.."), name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEveryPlatformHasEveryPin(t *testing.T) {
	pins := readPins(t)
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, key := range []string{"ORT_NPM_URL", "ORT_NPM_SHA256", "TOK_BASE_URL"} {
		if pins[key] == "" {
			t.Errorf("pins.env has no %s", key)
		}
	}
	if !hex64.MatchString(pins["ORT_NPM_SHA256"]) {
		t.Errorf("ORT_NPM_SHA256 is not a SHA-256: %q", pins["ORT_NPM_SHA256"])
	}
	for _, p := range []string{"darwin_arm64", "linux_arm64", "linux_amd64"} {
		for _, k := range []string{"ORT_MEMBER", "ORT_FILE"} {
			if pins[k+"_"+p] == "" {
				t.Errorf("pins.env has no %s_%s", k, p)
			}
		}
		for _, k := range []string{"ORT_SHA", "TOK_SHA_TGZ", "TOK_SHA"} {
			if !hex64.MatchString(pins[k+"_"+p]) {
				t.Errorf("%s_%s is not a SHA-256: %q", k, p, pins[k+"_"+p])
			}
		}
	}
}

func TestNoPinIsRepeatedOutsidePinsEnv(t *testing.T) {
	pins := readPins(t)
	// Every file that could carry a second copy: a value set later in fetch.sh would override the
	// pin unnoticed.
	files := []string{"Dockerfile", "Makefile", "internal/modules/search/infra/onnx/native/fetch.sh"}
	workflows, err := fs.Glob(os.DirFS("../../../../.."), ".github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, workflows...)
	for _, name := range files {
		text := repoFile(t, name)
		for k, v := range pins {
			if strings.Contains(v, "/") || len(v) == 64 { // URLs, members and hashes
				if strings.Contains(text, v) {
					t.Errorf("%s repeats the pin %s (%s): it belongs in pins.env only", name, k, v)
				}
			}
		}
	}
	if !strings.Contains(repoFile(t, "Makefile"), "include internal/modules/search/infra/onnx/native/pins.env") {
		t.Error("the Makefile does not include pins.env")
	}
	if !strings.Contains(repoFile(t, "Dockerfile"), "native/fetch.sh") {
		t.Error("the Dockerfile does not use fetch.sh")
	}
}
