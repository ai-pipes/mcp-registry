//go:build compat

package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// COMPAT_APP_ENV adds variables to the app container's environment from the outside (the suite's
// self-check passes its distortion this way): comma-separated NAME=value pairs, non-secret values
// without commas. They sit on top of the harness defaults and under Options.Env.
const appEnvVar = "COMPAT_APP_ENV"

// COMPAT_LOG_DIR makes every stack that fails (or fails to boot) write its app container's log,
// secrets removed, to <dir>/<test name>-<stack id>.log. Unset = nothing is written.
const logDirVar = "COMPAT_LOG_DIR"

func appEnv() (map[string]string, error) { return parseAppEnv(os.Getenv(appEnvVar)) }

// harnessOwned are the variables the harness sets itself (they tie the app to its database, its
// published port and PUBLIC_URL, which is also Stack.BaseURL, or come from Options.OIDC); the
// outside may not override them.
var harnessOwned = map[string]bool{
	"DATABASE_URL": true, "PUBLIC_URL": true, "COOKIE_SECURE": true, "SECRETS_KEY": true,
	"LOG_LEVEL": true, "PORT": true,
	"OIDC_ISSUER": true, "OIDC_CLIENT_ID": true, "OIDC_CLIENT_SECRET": true,
}

// credentialShape matches the sign-in link codes (ll_ + 32 bytes base64url) and API tokens (sk_ +
// 32 bytes base64url) of the product; anything that long after the prefix is removed from logs.
var credentialShape = regexp.MustCompile(`\b(?:ll|sk)_[A-Za-z0-9_-]{20,}`)

// parseAppEnv reads the value of COMPAT_APP_ENV.
func parseAppEnv(raw string) (map[string]string, error) {
	var out map[string]string
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s: %q is not NAME=value", appEnvVar, pair)
		}
		if out == nil {
			out = map[string]string{}
		}
		name = strings.TrimSpace(name)
		if harnessOwned[name] {
			return nil, fmt.Errorf("%s: %s is set by the harness and cannot be overridden", appEnvVar, name)
		}
		out[name] = value
	}
	return out, nil
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// logFileName is <test name>-<stack id>.log with every character outside [A-Za-z0-9._-] (the
// slashes of subtests, spaces) replaced by an underscore.
func logFileName(testName, stackID string) string {
	name := strings.Trim(unsafeFileChars.ReplaceAllString(testName, "_"), "_")
	if name == "" {
		name = "stack"
	}
	return name + "-" + stackID + ".log"
}

// writeLog scrubs text and writes it into dir (created when absent); it returns the path.
func (s *Stack) writeLog(dir, testName, text string) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G703: dir is the operator's own COMPAT_LOG_DIR
		return "", err
	}
	path := filepath.Join(dir, logFileName(testName, s.id))
	return path, os.WriteFile(path, []byte(s.scrub(text)), 0o600) //nolint:gosec // G703: path is under the operator's own COMPAT_LOG_DIR
}

// saveLog writes the app container's whole log to COMPAT_LOG_DIR, when it is set. Failing to save
// is reported on stderr and never changes a result.
func (s *Stack) saveLog(testName string) {
	dir := os.Getenv(logDirVar)
	if dir == "" || s.app == "" {
		return
	}
	fetch := s.fetchLog
	if fetch == nil {
		fetch = func(container string) (string, error) { return dockerLogs(container, "") }
	}
	text, err := fetch(s.app)
	if err != nil { // no app container (the boot failed before it started): nothing to keep
		fmt.Fprintf(os.Stderr, "harness: no app log to save for %s: %v\n", s.app, err)
		return
	}
	if path, werr := s.writeLog(dir, testName, text); werr != nil {
		fmt.Fprintf(os.Stderr, "harness: saving the app log to %s: %v\n", dir, werr)
	} else {
		fmt.Fprintf(os.Stderr, "harness: app log saved to %s\n", path)
	}
}
