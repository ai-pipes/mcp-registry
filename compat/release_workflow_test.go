//go:build compat

package compat_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The release workflow is the only place in the repository with packages: write. These pins keep
// its shape (Docker-free, run by make go-verify):
//   - the workflow grants contents: read and nothing else; its triggers are exactly push (only
//     tags: ['v*']), workflow_dispatch and pull_request (no pull_request_target, workflow_call,
//     workflow_run, schedule or anything else that could run it with other privileges or inputs);
//   - only the publishing jobs (push-digest, publish) have a write permission, and that one is
//     packages: write; each of them runs only if the event is a push of a tag and needs the test
//     job; every other job has no permissions key or contents: read (scopes set to none allowed);
//   - nothing outside the publishing jobs (workflow-level env, defaults and the rest, every other
//     job with its env, defaults and steps) mentions a secret or the workflow token, or pushes an
//     image;
//   - no emulation: no QEMU or binfmt, no --platform in a command, and every image build names its
//     platform through the matrix of a job that pairs linux/amd64 with ubuntu-latest and linux/arm64
//     with ubuntu-24.04-arm (each architecture built and run on its own runner).

const releaseWorkflowPath = "../.github/workflows/release.yml"

// publishingJobs are the only jobs that may hold packages: write.
var publishingJobs = []string{"push-digest", "publish"}

// nativeRunners pairs each platform with the GitHub-hosted runner of that architecture.
var nativeRunners = map[string]string{"linux/amd64": "ubuntu-latest", "linux/arm64": "ubuntu-24.04-arm"}

// tagPushCondition is the exact `if:` of every job that holds packages: write: a push event that
// is a tag (the push trigger lists only v* tags; the second half keeps it so if a branch is added).
const tagPushCondition = "github.event_name == 'push' && github.ref_type == 'tag'"

// allowedTriggers are the workflow's triggers: a tag push releases, the other two rehearse.
var allowedTriggers = []string{"pull_request", "push", "workflow_dispatch"}

// secretMentions are the texts that read a secret or the workflow token.
var secretMentions = []string{"secrets.", "github.token", "GITHUB_TOKEN"}

func releaseWorkflowProblems(raw []byte) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	var wf map[string]any
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return []string{"release.yml does not parse: " + err.Error()}
	}
	text := string(raw)
	for _, banned := range []string{"setup-qemu-action", "binfmt"} {
		if strings.Contains(text, banned) {
			add("release.yml mentions %s: build each architecture on its own runner, never emulated", banned)
		}
	}

	if perms, ok := wf["permissions"].(map[string]any); !ok || len(perms) != 1 || perms["contents"] != "read" {
		add("the workflow permissions are %v, want exactly contents: read", wf["permissions"])
	}
	on, _ := wf["on"].(map[string]any)
	if len(on) == 0 {
		add("on is %v, want the triggers %v as a map", wf["on"], allowedTriggers)
	}
	for _, trigger := range sortedKeys(on) {
		if !slices.Contains(allowedTriggers, trigger) {
			add("trigger %s is not allowed: the triggers are exactly %v (another trigger could run the workflow with other privileges, secrets or inputs)", trigger, allowedTriggers)
		}
	}
	for _, trigger := range allowedTriggers {
		if _, ok := on[trigger]; !ok {
			add("trigger %s is missing: the triggers are exactly %v", trigger, allowedTriggers)
		}
	}
	push, _ := on["push"].(map[string]any)
	tags, _ := push["tags"].([]any)
	if len(push) != 1 || len(tags) != 1 || tags[0] != "v*" {
		add("on.push is %v, want only tags: ['v*'] (a push event must mean a release tag)", on["push"])
	}

	// Everything at the workflow level (env, defaults, concurrency, run-name...) reaches every job.
	for _, key := range sortedKeys(wf) {
		if key == "jobs" {
			continue
		}
		section, _ := yaml.Marshal(wf[key])
		for _, token := range secretMentions {
			if strings.Contains(string(section), token) {
				add("the workflow's %s mentions %s: it reaches every job, and only %v may read a secret or the workflow token", key, token, publishingJobs)
			}
		}
	}

	jobs, _ := wf["jobs"].(map[string]any)
	if len(jobs) == 0 {
		add("release.yml has no jobs")
	}
	for _, name := range sortedKeys(jobs) {
		job, _ := jobs[name].(map[string]any)
		publishing := slices.Contains(publishingJobs, name)
		jobText, _ := yaml.Marshal(job)

		switch perms := job["permissions"].(type) {
		case nil:
		case map[string]any:
			for _, scope := range sortedKeys(perms) {
				value := perms[scope]
				switch {
				case value == "none", scope == "contents" && value == "read":
				case !publishing:
					add("job %s has permission %s: %v; a job other than %v gets at most contents: read", name, scope, value, publishingJobs)
				case value == "read":
				case scope != "packages" || value != "write":
					add("job %s has permission %s: %v; only %v may hold a write permission, and only packages: write", name, scope, value, publishingJobs)
				}
			}
		default:
			add("job %s sets permissions to %v; name each scope", name, perms)
		}

		if publishing {
			if job["if"] != tagPushCondition {
				add("job %s runs if %q, want exactly %q (a push of a tag)", name, job["if"], tagPushCondition)
			}
			if !slices.Contains(stringList(job["needs"]), "test") {
				add("job %s needs %v, want the test job among them", name, job["needs"])
			}
		} else {
			for _, token := range secretMentions {
				if strings.Contains(string(jobText), token) {
					add("job %s mentions %s: only %v may read a secret or the workflow token", name, token, publishingJobs)
				}
			}
		}

		steps, _ := job["steps"].([]any)
		for i, s := range steps {
			step, _ := s.(map[string]any)
			label := fmt.Sprintf("job %s step %d", name, i+1)
			if n, ok := step["name"].(string); ok {
				label = fmt.Sprintf("job %s step %q", name, n)
			}
			run, _ := step["run"].(string)
			if strings.Contains(run, "--platform") {
				add("%s runs a command with --platform: no emulated build or run", label)
			}
			if !publishing && (strings.Contains(run, "docker push") || (strings.Contains(run, "imagetools create") && !strings.Contains(run, "--dry-run"))) {
				add("%s pushes to a registry: only %v may", label, publishingJobs)
			}
			uses, _ := step["uses"].(string)
			if !strings.HasPrefix(uses, "docker/build-push-action@") {
				continue
			}
			with, _ := step["with"].(map[string]any)
			if with["platforms"] != "${{ matrix.platform }}" {
				add("%s builds for platforms %v, want ${{ matrix.platform }} (one native platform per runner)", label, with["platforms"])
			}
			outputs, _ := with["outputs"].(string)
			if !publishing && (with["push"] != false || strings.Contains(outputs, "push=true")) {
				add("%s may push (push: %v, outputs: %q): only %v may; set push: false", label, with["push"], outputs, publishingJobs)
			}
			for _, p := range nativeMatrixProblems(job) {
				add("%s: %s", label, p)
			}
		}
	}
	return slices.Compact(problems)
}

// nativeMatrixProblems checks that a job building ${{ matrix.platform }} runs on ${{ matrix.runner }}
// and that its matrix pairs exactly linux/amd64 with ubuntu-latest and linux/arm64 with ubuntu-24.04-arm.
func nativeMatrixProblems(job map[string]any) []string {
	var problems []string
	if job["runs-on"] != "${{ matrix.runner }}" {
		problems = append(problems, fmt.Sprintf("runs on %v, want ${{ matrix.runner }}", job["runs-on"]))
	}
	strategy, _ := job["strategy"].(map[string]any)
	matrix, _ := strategy["matrix"].(map[string]any)
	include, _ := matrix["include"].([]any)
	seen := map[string]bool{}
	for _, e := range include {
		entry, _ := e.(map[string]any)
		platform, _ := entry["platform"].(string)
		runner, _ := entry["runner"].(string)
		want, known := nativeRunners[platform]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("matrix platform %q is not one of %v", platform, sortedKeys(nativeRunners)))
		case runner != want:
			problems = append(problems, fmt.Sprintf("matrix builds %s on %q, want its native runner %q", platform, runner, want))
		case seen[platform]:
			problems = append(problems, fmt.Sprintf("matrix lists %s twice", platform))
		}
		seen[platform] = true
	}
	if len(include) != len(nativeRunners) || len(matrix) != 1 {
		problems = append(problems, fmt.Sprintf("matrix is %v, want only an include list of one entry per platform in %v", matrix, sortedKeys(nativeRunners)))
	}
	return problems
}

func stringList(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestReleaseWorkflowKeepsThePublishingPrivilegeAndNativeBuilds(t *testing.T) {
	raw, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatal(err)
	}
	if problems := releaseWorkflowProblems(raw); len(problems) > 0 {
		t.Errorf("%s:\n  %s", releaseWorkflowPath, strings.Join(problems, "\n  "))
	}
}

// Each mutation of the real file must be refused, with a reason that names what changed. A
// mutation whose text is not found exactly once fails the test, so none of them can silently
// stop mutating anything when the workflow is edited.
func TestReleaseWorkflowGuardRefusesBrokenCopies(t *testing.T) {
	raw, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	cases := []struct {
		name, old, new, reason string
	}{
		{
			name:   "the test job gets packages: write",
			old:    "  test:\n    name: test (${{ matrix.arch }})\n",
			new:    "  test:\n    name: test (${{ matrix.arch }})\n    permissions:\n      contents: read\n      packages: write\n",
			reason: "job test has permission packages: write",
		},
		{
			name:   "the workflow grants packages: write",
			old:    "permissions:\n  contents: read\n",
			new:    "permissions:\n  contents: read\n  packages: write\n",
			reason: "the workflow permissions are",
		},
		{
			name:   "a publishing job gets another write permission",
			old:    "    timeout-minutes: 30\n    permissions:\n      contents: read\n      packages: write\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n        with:\n          persist-credentials: false\n      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1\n      - name: The image name in lowercase",
			new:    "    timeout-minutes: 30\n    permissions:\n      contents: read\n      packages: write\n      id-token: write\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n        with:\n          persist-credentials: false\n      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1\n      - name: The image name in lowercase",
			reason: "job publish has permission id-token: write",
		},
		{
			name:   "publish loses its tag-push condition",
			old:    "  publish:\n    needs: [test, push-digest]\n    if: github.event_name == 'push' && github.ref_type == 'tag'\n",
			new:    "  publish:\n    needs: [test, push-digest]\n",
			reason: "job publish runs if",
		},
		{
			name:   "publish runs on any push, not only a tag",
			old:    "  publish:\n    needs: [test, push-digest]\n    if: github.event_name == 'push' && github.ref_type == 'tag'\n",
			new:    "  publish:\n    needs: [test, push-digest]\n    if: github.event_name == 'push'\n",
			reason: "job publish runs if \"github.event_name == 'push'\"",
		},
		{
			name:   "push-digest runs on every event",
			old:    "  push-digest:\n    needs: test\n    if: github.event_name == 'push' && github.ref_type == 'tag'\n",
			new:    "  push-digest:\n    needs: test\n    if: always()\n",
			reason: "job push-digest runs if \"always()\"",
		},
		{
			name:   "publish no longer needs the test job",
			old:    "  publish:\n    needs: [test, push-digest]\n",
			new:    "  publish:\n    needs: [push-digest]\n",
			reason: "job publish needs [push-digest], want the test job",
		},
		{
			name:   "a push of a branch counts as a release",
			old:    "  push:\n    tags: ['v*']\n",
			new:    "  push:\n    branches: [main]\n    tags: ['v*']\n",
			reason: "on.push is",
		},
		{
			name:   "QEMU comes back",
			old:    "      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1\n      # The candidate",
			new:    "      - uses: docker/setup-qemu-action@99012661954931238ded8c8b007157a8430204e1 # v4.4.0\n      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1\n      # The candidate",
			reason: "mentions setup-qemu-action",
		},
		{
			name:   "the candidate is built for both platforms in one build",
			old:    "          platforms: ${{ matrix.platform }}\n          load: true\n",
			new:    "          platforms: linux/amd64,linux/arm64\n          load: true\n",
			reason: "builds for platforms linux/amd64,linux/arm64",
		},
		{
			name:   "arm64 is tested on an amd64 runner",
			old:    "          - arch: arm64\n            platform: linux/arm64\n            runner: ubuntu-24.04-arm\n    # One output per architecture",
			new:    "          - arch: arm64\n            platform: linux/arm64\n            runner: ubuntu-latest\n    # One output per architecture",
			reason: "matrix builds linux/arm64 on \"ubuntu-latest\", want its native runner \"ubuntu-24.04-arm\"",
		},
		{
			name:   "an image is run under emulation",
			old:    "docker run --rm mcp-registry:rc",
			new:    "docker run --rm --platform linux/arm64 mcp-registry:rc",
			reason: "runs a command with --platform",
		},
		{
			name:   "the test job reads a secret",
			old:    "      - name: The image prints the version from the VERSION file\n        env:\n",
			new:    "      - name: The image prints the version from the VERSION file\n        env:\n          LEAK: ${{ secrets.GITHUB_TOKEN }}\n",
			reason: "job test mentions secrets.",
		},
		{
			name:   "the test job pushes its candidate",
			old:    "          load: true\n          push: false\n",
			new:    "          load: true\n          push: true\n",
			reason: "may push (push: true",
		},
		{
			name:   "a pull_request_target trigger is added",
			old:    "  workflow_dispatch:\n",
			new:    "  workflow_dispatch:\n  pull_request_target:\n",
			reason: "trigger pull_request_target",
		},
		{
			name:   "a workflow_call trigger is added",
			old:    "  workflow_dispatch:\n",
			new:    "  workflow_dispatch:\n  workflow_call:\n",
			reason: "trigger workflow_call",
		},
		{
			name:   "a workflow_run trigger is added",
			old:    "  workflow_dispatch:\n",
			new:    "  workflow_dispatch:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\n",
			reason: "trigger workflow_run",
		},
		{
			name:   "a schedule trigger is added",
			old:    "  workflow_dispatch:\n",
			new:    "  workflow_dispatch:\n  schedule:\n    - cron: '0 3 * * *'\n",
			reason: "trigger schedule",
		},
		{
			name:   "the workflow env reads a secret",
			old:    "\nenv:\n",
			new:    "\nenv:\n  T: ${{ secrets.X }}\n",
			reason: "the workflow's env mentions secrets.",
		},
		{
			name:   "the workflow defaults read the workflow token",
			old:    "defaults:\n  run:\n    shell: bash\n",
			new:    "defaults:\n  run:\n    shell: bash\n    working-directory: ${{ github.token }}\n",
			reason: "the workflow's defaults mentions github.token",
		},
		{
			name:   "the rehearsal's assembly hands a step the workflow token",
			old:    "      - name: Dry assembly of a manifest list from the reference image\n",
			new:    "      - name: Dry assembly of a manifest list from the reference image\n        env:\n          GH: ${{ secrets.GITHUB_TOKEN }}\n",
			reason: "job assemble mentions secrets.",
		},
		{
			name:   "the rehearsal's assembly gets write-all",
			old:    "  assemble:\n    needs: test\n",
			new:    "  assemble:\n    needs: test\n    permissions: write-all\n",
			reason: "job assemble sets permissions to write-all",
		},
		{
			name:   "the test job gets read-all",
			old:    "  test:\n    name: test (${{ matrix.arch }})\n",
			new:    "  test:\n    name: test (${{ matrix.arch }})\n    permissions: read-all\n",
			reason: "job test sets permissions to read-all",
		},
		{
			name:   "the test job may read actions",
			old:    "  test:\n    name: test (${{ matrix.arch }})\n",
			new:    "  test:\n    name: test (${{ matrix.arch }})\n    permissions:\n      contents: read\n      actions: read\n",
			reason: "job test has permission actions: read",
		},
		{
			name:   "the rehearsal's assembly pushes",
			old:    "docker buildx imagetools create --dry-run",
			new:    "docker buildx imagetools create",
			reason: "job assemble step \"Dry assembly of a manifest list from the reference image\" pushes to a registry",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if n := strings.Count(src, c.old); n != 1 {
				t.Fatalf("the text to mutate occurs %d times in %s, want once: %q", n, releaseWorkflowPath, c.old)
			}
			problems := releaseWorkflowProblems([]byte(strings.Replace(src, c.old, c.new, 1)))
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.reason) }) {
				t.Errorf("the broken copy was not refused for %q; problems: %q", c.reason, problems)
			}
		})
	}
}
