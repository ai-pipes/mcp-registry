//go:build compat

package harness

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// CLIResult is the outcome of one operator CLI invocation inside the app container.
type CLIResult struct {
	Stdout, Stderr string
	Code           int
}

// CLI runs the operator CLI (COMPAT_CLI plus args) in the app container through docker exec.
// stdin is passed when it is not empty.
func (s *Stack) CLI(t *testing.T, stdin string, args ...string) CLIResult {
	t.Helper()
	if up, err := s.running(); err != nil || !up {
		t.Fatalf("cli %v: the app container is not running (%v)", args, err)
	}
	argv := []string{"exec"}
	if stdin != "" {
		argv = append(argv, "-i")
	}
	argv = append(argv, s.app)
	argv = append(argv, strings.Fields(s.cli)...)
	argv = append(argv, args...)
	return s.runDocker(t, argv, stdin, nil, args)
}

// CLIWithEnv runs the CLI in a fresh one-off container of the stack's current image, on the stack's
// network and against the stack's database, with the stack's settings and env on top (an empty
// value removes the variable). It works when the app container is not running (the app refuses to
// start on a key mismatch), and it is how a CLI run sees a different SECRETS_KEY or an extra
// variable for --secret-env. Files placed with PutFile are copied into the container. Values never
// appear on a command line: every variable is passed as "-e NAME" with the value in the environment
// of the docker process.
func (s *Stack) CLIWithEnv(t *testing.T, env map[string]string, stdin string, args ...string) CLIResult {
	t.Helper()
	create, extraEnv := s.oneOffCreateArgs(env, stdin != "", args)
	id, err := dockerEnv(extraEnv, create...)
	if err != nil {
		t.Fatalf("cli %v: %v", args, errors.New(s.scrub(err.Error())))
	}
	defer func() { _, _ = dockerErr("rm", "-f", "-v", id) }()
	for path, content := range s.files {
		if err := copyInto(id, path, content); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
	}
	start := []string{"start", "-a"}
	if stdin != "" {
		start = append(start, "-i")
	}
	return s.runDocker(t, append(start, id), stdin, nil, args)
}

// oneOffCreateArgs builds the `docker create` arguments of a one-off CLI container and the
// "NAME=value" pairs the docker process needs in its environment. The arguments carry variable
// NAMES only ("-e NAME"); a value never appears on a command line, where a process list shows it.
func (s *Stack) oneOffCreateArgs(env map[string]string, withStdin bool, args []string) (create, extraEnv []string) {
	merged := make(map[string]string, len(s.env)+len(env))
	for k, v := range s.env {
		merged[k] = v
	}
	for k, v := range env {
		if v == "" {
			delete(merged, k)
		} else {
			merged[k] = v
		}
	}
	names := make([]string, 0, len(merged))
	for k := range merged {
		names = append(names, k)
	}
	sort.Strings(names)
	create = []string{"create", "--network", s.network, "--add-host", "host.docker.internal:host-gateway"}
	create = append(create, s.labels()...)
	if withStdin {
		create = append(create, "-i")
	}
	for _, k := range names {
		create = append(create, "-e", k)
		extraEnv = append(extraEnv, k+"="+merged[k])
	}
	create = append(create, s.image)
	create = append(create, strings.Fields(s.cli)...)
	create = append(create, args...)
	return create, extraEnv
}

// PutFile places a file (mode 0644) at an absolute path inside the app container (when one exists:
// the app may be down after a refused start) and remembers it, so that CLIWithEnv containers and
// every later app container (SwapImage) get it too.
func (s *Stack) PutFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if !strings.HasPrefix(path, "/") {
		t.Fatalf("PutFile: %q is not an absolute path", path)
	}
	if _, err := dockerErr("inspect", "-f", "{{.Id}}", s.app); err == nil {
		if err := copyInto(s.app, path, content); err != nil {
			t.Fatalf("PutFile: %v", err)
		}
	}
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = bytes.Clone(content)
}

// copyInto writes content to path in a container with docker cp (the target needs no shell).
func copyInto(container, path string, content []byte) error {
	dir, err := os.MkdirTemp("", "compat-put-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	local := filepath.Join(dir, filepath.Base(path))
	if err := os.WriteFile(local, content, 0o644); err != nil { //nolint:gosec // the container's user must be able to read it
		return err
	}
	_, err = dockerErr("cp", local, container+":"+path)
	return err
}

// dockerEnv is dockerErr with extra environment variables for the docker process.
func dockerEnv(extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", errors.New("docker " + args[0] + ": " + err.Error() + ": " + strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (s *Stack) runDocker(t *testing.T, argv []string, stdin string, extraEnv []string, args []string) CLIResult {
	t.Helper()
	cmd := exec.Command("docker", argv...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	res := CLIResult{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
		// 125-127 and a daemon error are docker's own failures, not the CLI's answer.
		if res.Code >= 125 && res.Code <= 127 || strings.HasPrefix(res.Stderr, "Error response from daemon") {
			t.Fatalf("docker failed running the cli %v (exit %d): %s", args, res.Code, strings.TrimSpace(s.scrub(res.Stderr)))
		}
	default:
		t.Fatalf("docker: %v", err)
	}
	return res
}
