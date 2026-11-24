package archtest

import (
	"strings"
	"testing"
)

// secretTagSets are the build tag sets the check type-checks the repository under. onnx is not
// one: its cgo files need the native libraries (make native-libs) to type-check, and they hold no
// secret; compat code lives outside internal/ and cmd/.
var secretTagSets = [][]string{nil, {"integration"}}

func TestSecretIntoAnyIsReported(t *testing.T) {
	v, err := SecretsIntoEmptyInterfacesInFiles(repoRoot, "testdata/secret-into-any")
	if err != nil {
		t.Fatal(err)
	}
	var whats []string
	for _, l := range v {
		whats = append(whats, l.What)
	}
	want := "argument of fmt.Sprintf,argument of exec,variable declaration,element,map value,return"
	if got := strings.Join(whats, ","); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRevealedSecretsAndPortsAreNotReported(t *testing.T) {
	v, err := SecretsIntoEmptyInterfacesInFiles(repoRoot, "testdata/secret-into-any-allowed")
	if err != nil || len(v) != 0 {
		t.Fatalf("got %v err=%v", v, err)
	}
}

// TestRepositoryPassesNoSecretToAnEmptyInterface: no non-test code of internal/ or cmd/ hands a
// kernel.Secret / kernel.SecretBytes (or a value holding one) to fmt, slog, pgx, encoding/json or
// any other ...any / any parameter: pgx would store the redaction marker and fmt print it, with no
// error. Reveal() at the boundary instead.
func TestRepositoryPassesNoSecretToAnEmptyInterface(t *testing.T) {
	v, err := SecretsIntoEmptyInterfaces(repoRoot, secretTagSets, "./internal/...", "./cmd/...")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range v {
		t.Error(l)
	}
}
