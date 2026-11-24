package archtest

import (
	"slices"
	"testing"
)

// plainSecretAllowlist are the fields (and named types) of internal/ whose name looks like a secret
// and whose type is a plain string or byte slice, each with the reason it may stay so. A new entry
// needs a reason a reviewer can check; anything that carries a credential is held as kernel.Secret
// or kernel.SecretBytes instead.
var plainSecretAllowlist = map[string]string{ //nolint:gosec // G101: the keys name struct fields and the values are reasons, no credential
	"kernel.DomainError.Code":                         "an error code (a fixed identifier), not an OAuth code",
	"modules/catalog/app.SyncOutcome.Code":            "an error code of a synchronisation",
	"modules/connections/app.TokenEndpointError.Code": "the OAuth error code the token endpoint answered (invalid_grant...), not a token",
	"modules/gateway/http.zodIssue.Code":              "a validation issue code of the wire format",
	"platform/app.failedSync.Code":                    "an error code in a log record",
	"modules/connections/http.patRequest.Token": "the wire DTO the PAT body is decoded into; wrapped into a kernel.Secret as soon as " +
		"it is validated, never kept or printed",
	"modules/identity/http.linkLoginRequest.Code": "the wire DTO the sign-in code body is decoded into; wrapped into a kernel.Secret " +
		"as soon as it is validated, never kept or printed",
	"modules/identity/http.createTokenResponse.Token": "the wire DTO of the one response that shows a created token, filled by an " +
		"explicit Reveal and written at once",
	"modules/identity/infra/oidctest.Provider.ClientSecret": "the fake identity provider of the tests and the browser flows (test support)",
	"modules/identity/infra/oidctest.Grant.Nonce":           "the fake identity provider of the tests (test support)",
	"modules/identity/infra/oidctest.Tamper.Nonce":          "the fake identity provider of the tests (test support)",
	"modules/catalog/domain.AuthorizeParam.Key":             "the NAME of an extra authorize query parameter, not a key",
	"modules/gateway/http.envelopeIssue.Key":                "the NAME of a JSON key of a request envelope, in an error answer",
	"modules/identity/infra.entry.key":                      "a rate limiter's key: the client address it counts",
	"archtest.FieldViolation.Key":                           "this check's own allowlist key (a field path)",
}

func TestPlainSecretFieldIsReported(t *testing.T) {
	v, err := PlainSecretFields("testdata/plain-secret-field", nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, x := range v {
		keys = append(keys, x.Key)
	}
	if want := []string{"modules/demo/app.Command.AccessToken", "modules/demo/app.Sealed"}; !slices.Equal(keys, want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
}

func TestSecretTypesAndAllowlistedFieldsAreNotReported(t *testing.T) {
	v, err := PlainSecretFields("testdata/plain-secret-field-allowed", map[string]string{"modules/demo/app.Problem.Code": "an error code"})
	if err != nil || len(v) != 0 {
		t.Fatalf("got %+v err=%v", v, err)
	}
	v, err = PlainSecretFields("testdata/plain-secret-field-allowed", nil)
	if err != nil || len(v) != 1 || v[0].Key != "modules/demo/app.Problem.Code" {
		t.Fatalf("without the allowlist the code field is reported: %+v err=%v", v, err)
	}
}

func TestSecretNameWords(t *testing.T) {
	for name, want := range map[string]bool{
		"AccessToken": true, "ClientSecret": true, "CodeVerifier": true, "SessionID": true, "APIKey": true,
		"apiKey": true, "access_token": true, "Nonce": true, "state": true, "SealedSecret": true,
		"EncryptionKey": true, "Key": true, "Authorization": true, "LoginLinkURL": true, "Link": true,
		"TokenURL": false, "TokenID": false, "CodeHash": false, "IDHash": false, "SecretSet": false, "Status": false,
		"Header": false, "URL": false, "PublicURL": false, "KeyID": false,
	} {
		if got := secretName(name); got != want {
			t.Errorf("secretName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestRepositoryHasNoPlainSecretFields: every field of internal/ that carries a secret is held as
// kernel.Secret / kernel.SecretBytes, or is allowlisted with a reason; an entry that no longer
// matches anything is removed (a stale entry would silently allow a new field of that name).
func TestRepositoryHasNoPlainSecretFields(t *testing.T) {
	v, err := PlainSecretFields("..", plainSecretAllowlist)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		t.Error(x)
	}
	all, err := PlainSecretFields("..", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, x := range all {
		seen[x.Key] = true
	}
	for key := range plainSecretAllowlist {
		if !seen[key] {
			t.Errorf("allowlist entry %q matches nothing: remove it", key)
		}
	}
}
