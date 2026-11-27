package onnx

import (
	"io/fs"
	"os"
	"regexp"
	"strconv"
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

// The embedding model is pinned in the same file: a revision and, for each of its files, a name and
// a SHA-256 (MODEL_FILE_<n>, MODEL_SHA256_<n>). fetch-model.sh reads them; the Dockerfile stage
// `model` and `make native-model` run that script, so no Node is needed to fetch the model.
func TestEveryModelFileHasAPin(t *testing.T) {
	pins := readPins(t)
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if pins["MODEL_NAME"] != "Xenova/multilingual-e5-small" {
		t.Errorf("MODEL_NAME is %q", pins["MODEL_NAME"])
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(pins["MODEL_REVISION"]) {
		t.Errorf("MODEL_REVISION is not a commit: %q", pins["MODEL_REVISION"])
	}
	want := []string{
		"config.json", "special_tokens_map.json", "tokenizer_config.json", "tokenizer.json",
		"onnx/model_quantized.onnx",
	}
	for i, file := range want {
		n := strconv.Itoa(i + 1)
		if pins["MODEL_FILE_"+n] != file {
			t.Errorf("MODEL_FILE_%s is %q, want %q", n, pins["MODEL_FILE_"+n], file)
		}
		if !hex64.MatchString(pins["MODEL_SHA256_"+n]) {
			t.Errorf("MODEL_SHA256_%s is not a SHA-256: %q", n, pins["MODEL_SHA256_"+n])
		}
	}
	if pins["MODEL_FILE_"+strconv.Itoa(len(want)+1)] != "" {
		t.Error("pins.env lists more model files than the test knows: update the test with the layout the embedder reads")
	}
}

func TestNoPinIsRepeatedOutsidePinsEnv(t *testing.T) {
	pins := readPins(t)
	// Every file that could carry a second copy: a value set later in fetch.sh would override the
	// pin unnoticed.
	files := []string{
		"Dockerfile", "Makefile",
		"internal/modules/search/infra/onnx/native/fetch.sh",
		"internal/modules/search/infra/onnx/native/fetch-model.sh",
	}
	workflows, err := fs.Glob(os.DirFS("../../../../.."), ".github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, workflows...)
	for _, name := range files {
		text := repoFile(t, name)
		for k, v := range pins {
			if strings.Contains(v, "/") || len(v) == 64 || len(v) == 40 { // URLs, members, hashes and the model revision
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
	if !strings.Contains(repoFile(t, "Dockerfile"), "native/fetch-model.sh") {
		t.Error("the Dockerfile does not fetch the model with fetch-model.sh")
	}
	if !strings.Contains(repoFile(t, "Makefile"), "native/fetch-model.sh") {
		t.Error("the Makefile does not fetch the model with fetch-model.sh")
	}
}

// The parity target runs the Go tests in the same golang image the Dockerfile builds with: the
// Makefile names it by digest, and the digest must be the Dockerfile's.
func TestParityTargetUsesTheDockerfilesGolangDigest(t *testing.T) {
	re := regexp.MustCompile(`golang:[0-9.]+-bookworm@sha256:[0-9a-f]{64}`)
	docker := re.FindAllString(repoFile(t, "Dockerfile"), -1)
	if len(docker) == 0 {
		t.Fatal("the Dockerfile names no pinned golang image")
	}
	for _, d := range docker {
		if d != docker[0] {
			t.Fatalf("the Dockerfile pins two golang images: %s and %s", docker[0], d)
		}
	}
	mk := re.FindAllString(repoFile(t, "Makefile"), -1)
	if len(mk) == 0 {
		t.Fatal("the Makefile does not pin the golang image of the parity target by digest")
	}
	for _, m := range mk {
		if m != docker[0] {
			t.Errorf("the Makefile pins %s, the Dockerfile %s", m, docker[0])
		}
	}
}
