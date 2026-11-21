GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GO_ARCH_LINT := github.com/fe3dback/go-arch-lint@v1.19.2
GOFUMPT := mvdan.cc/gofumpt@v0.12.0
VERSION := $(shell node -p "require('./apps/server/package.json').version")

.PHONY: compat compat-selfcheck compat-schemas compat-image-ts compat-image-go compat-go e2e-flows go-lint go-lint-new go-fmt-check go-arch-lint go-verify go-test-int go-build print-go-arch-lint print-golangci-lint

go-lint:
	go run $(GOLANGCI_LINT) run

go-lint-new:
	go run $(GOLANGCI_LINT) run --new-from-rev=HEAD

go-fmt-check:
	@out="$$(go run $(GOFUMPT) -l .)"; \
	if [ -n "$$out" ]; then echo "gofumpt: needs formatting:"; echo "$$out"; exit 1; fi

go-arch-lint:
	go run $(GO_ARCH_LINT) check

# The pinned tool references above are the only place the versions live; internal/archtest
# reads them through these targets.
print-go-arch-lint:
	@echo $(GO_ARCH_LINT)

print-golangci-lint:
	@echo $(GOLANGCI_LINT)

# The compat suite is built with -tags compat so that lint and vet see it; its tests need Docker
# and minutes, so go-verify does not run them (make compat does).
#
# Integration tests (build tag integration) start a real Postgres through testcontainers and need
# Docker, like `pnpm test:int`. They are vetted and linted with the tag (.golangci.yml build-tags)
# and run by go-test-int, which go-verify includes.
go-verify: go-lint go-arch-lint
	go vet ./...
	go vet -tags integration ./...
	go vet -tags compat ./...
	go vet -tags "compat selfcheck" ./compat/selfcheck/
	go vet -tags "compat e2e" ./compat/e2e/
	go test -tags "compat e2e" -count=1 -run '^(TestControl(Refuses|Echo)|TestPlaywrightArgs)' ./compat/e2e/
	# Docker-free tests of the suite itself (none boots a stack; they pass with the Docker socket
	# unreachable). A test that needs no Docker is added here by name.
	go test -tags compat -count=1 -run '^(TestAppArgsPublishTheContainerPortAndKeepFileContentOffTheCommandLine|TestAppEnvReadsTheEnvironment|TestCheckUpdate|TestCheckUpdateAllowedReadsTheEnvironment|TestCLIResultStringMasksTokensAndLinkCodes|TestDefaultImageAndCLIFollowTheEnvironment|TestDoChunkedExpectingEarlyRefusalSendsNoContentLength|TestDoExpectingEarlyRefusalReturnsTheAnswerWhateverHappenedToTheWrite|TestErrorNamesSchemaAndPath|TestEveryContractHasASchema|TestEveryDeviationHasADescriptionAndIsUsedByAScenario|TestFinishSavesTheLogOnlyForAFailedTest|TestFixedFollowsTheEnvironment|TestListSuffixAndUnknownName|TestLogFileName|TestLoopbackHostResolution|TestOneOffCLIContainerNeverPutsAValueOnTheCommandLine|TestOtherFormatsAreAsserted|TestParseAppEnv|TestParseFixed|TestPortTakenRecognisesDockersMessages|TestPublicHostAndExtraHostsReachTheContainerArguments|TestRedactTruncates|TestResponseDecode|TestSaveLogDoesNothingWithoutADirectory|TestSchemaRejectsExtraAndMissingFields|TestScrubRemovesCredentialShapes|TestScrubRemovesTheContentOfSecretFiles|TestSecretValuesListTheBareDatabasePasswordToo|TestServerViewWithSecretFieldIsRejected|TestUpdateRefusesNonReferenceImage|TestURLFormatsAreAsserted|TestWriteLogScrubsAndCreatesTheDirectory)$$' ./compat/harness/
	go test -tags compat -count=1 -run '^(TestAuditPagingProblem|TestCreateServerSyncFailureProblem|TestMCPReaderIsLenient|TestRetryAfterProblem|TestSearchReadyProblem)$$' ./compat/
	go test ./...
	$(MAKE) go-test-int

# Only internal/platform has integration tests. -p 1: testcontainers derives its session from the
# parent test process, so parallel test binaries race to create the shared reaper and some fail.
# The reaper removes a session's containers 10 s after its last client left; compiling the next
# package's test binary on a cold cache can take longer, and that binary then meets containers
# being removed. A minute covers it.
go-test-int:
	TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT=60s go test -tags integration -count=1 -p 1 ./internal/platform/...

# Regenerates compat/schemas from the zod contracts; CI fails when the committed files are stale.
compat-schemas:
	node --experimental-strip-types packages/contracts/scripts/export-json-schema.mjs compat/schemas

compat-image-ts:
	docker build -t mcp-registry:compat-ts .

# The Go image (Dockerfile target go), tagged mcp-registry:compat-go; the version comes from the same
# place as for go-build. The default target (runtime, the TypeScript image) is untouched.
compat-image-go:
	@test -n "$(VERSION)" || { echo "compat-image-go: the version read from apps/server/package.json is empty (is node on PATH?)"; exit 1; }
	docker build --target go --build-arg VERSION=$(VERSION) -t mcp-registry:compat-go .
	@got="$$(docker run --rm mcp-registry:compat-go mcp-registry --version)"; \
	if [ "$$got" != "$(VERSION)" ]; then echo "compat-image-go: the image reports version '$$got', the package says '$(VERSION)'"; exit 1; fi

# The scenarios the Go server passes so far (the milestone list); it grows with every milestone.
# Both variables are set with := here, so an environment variable of the same name does NOT
# override them; override on the make command line instead:
#   make compat-go COMPAT_GO_TESTS='TestConfigRefused TestPlatformWithoutPeople'
#   make compat-go COMPAT_GO_SKIP=
# In a value given that way (or edited here), `$$` is a literal `$` for make; a value is passed to
# go test inside single quotes, so it must not contain one.
COMPAT_GO_TESTS := TestConfigRefused TestPlatformStartsOnReferenceDatabase TestPlatformReverseHandOver \
	TestPlatformWithoutPeople TestPlatformMigrationsAndKeyCheck TestPlatformStaticFilesAndSPA

# Subtests of the scenarios above that need a later milestone, as a go test -skip expression
# (slash-separated regular expressions per level; spaces of a subtest name are underscores). Each
# entry names the milestone whose work removes it:
#   - TestPlatformMigrationsAndKeyCheck, "the old key alone: healthy, one warning;
#     rotate-secrets-key re-seals the check": runs the CLI command rotate-secrets-key: milestone 7
#     (the operator CLI).
#   - TestPlatformStaticFilesAndSPA, "/api, /mcp and /healthz are never the shell": asks for
#     /api/unknown (a JSON not_found) and /mcp (401 and a bearer-token error): milestone 2 (the
#     REST error shape of the identity routes) and milestone 6 (the /mcp route). The subtest's name
#     contains slashes, which go test reads as level separators: its levels are the empty string
#     after the first slash, "api,_", "mcp_and_" and "healthz_are_never_the_shell".
# Parent tests are never skipped by an entry with more levels than they have.
COMPAT_GO_SKIP := ^TestPlatformMigrationsAndKeyCheck$$/^the_old_key_alone:_healthy,_one_warning;_rotate-secrets-key_re-seals_the_check$$|^TestPlatformStaticFilesAndSPA$$/^$$/^api,_$$/^mcp_and_$$/^healthz_are_never_the_shell$$

space := $(subst ,, )
COMPAT_GO_RUN := ^($(subst $(space),|,$(strip $(COMPAT_GO_TESTS))))$$

# The suite against the Go image. The reference image (mcp-registry:compat-ts) must exist as well:
# some scenarios compare with it on the same machine, and the new-database scenario starts from it.
# COMPAT_FIXED=config-errors: the Go server has the owner's decided behaviour for the two
# configuration-error deviations (see compat/harness/deviations.go). Before it runs, the pattern is
# listed (nothing is started): it must match exactly the scenarios named in COMPAT_GO_TESTS, so a
# renamed or mistyped scenario fails here instead of passing by running nothing.
compat-go:
	@docker image inspect mcp-registry:compat-ts >/dev/null 2>&1 || { echo "compat-go: the reference image mcp-registry:compat-ts is missing: run make compat-image-ts"; exit 1; }
	@docker image inspect mcp-registry:compat-go >/dev/null 2>&1 || { echo "compat-go: the image mcp-registry:compat-go is missing: run make compat-image-go"; exit 1; }
	@listed="$$(go test -tags compat -count=1 -list '$(COMPAT_GO_RUN)' ./compat/ | grep -c '^Test')"; \
	if [ "$$listed" != "$(words $(COMPAT_GO_TESTS))" ]; then \
		echo "compat-go: the pattern '$(COMPAT_GO_RUN)' matches $$listed scenario(s), COMPAT_GO_TESTS names $(words $(COMPAT_GO_TESTS)): $(COMPAT_GO_TESTS)"; exit 1; fi
	COMPAT_IMAGE=mcp-registry:compat-go COMPAT_CLI=mcp-registry COMPAT_FIXED=config-errors \
		go test -tags compat -count=1 -p 1 -timeout 30m -run '$(COMPAT_GO_RUN)' $(if $(COMPAT_GO_SKIP),-skip '$(COMPAT_GO_SKIP)') $(COMPAT_ARGS) ./compat/

# COMPAT_ARGS adds go test arguments, e.g. make compat COMPAT_ARGS='-run ^TestPlatform' (the -run
# patterns per area are in CONTRIBUTING.md). Nothing here sets COMPAT_REFERENCE: it matters only
# for -update, which the harness allows only for the reference image. -p 1 runs the test packages
# (compat, compat/harness, compat/fakes) one after another: concurrent stacks on one machine stall
# the app's event loop (it loads its embedding model right after it first answers healthy), and a
# stalled app turns into flaky timeouts.
compat:
	go test -tags compat -count=1 -p 1 -timeout 45m $(COMPAT_ARGS) ./compat/...

# The browser flows (e2e/flows, Playwright with Chromium) against the image COMPAT_IMAGE (default
# mcp-registry:compat-ts, see compat-image-ts), orchestrated from Go so that the same flows judge the
# Go server later. Needs `pnpm install` and `pnpm exec playwright install chromium`. Not part of
# `make compat` (tag e2e). It also runs the control API's own tests (TestControl*); the Docker-free
# ones (TestControlRefuses*, TestControlEcho*: the guarantee behind the API's token and fixed
# operations; TestPlaywrightArgs*: the arguments refused) run in go-verify too. Traces of a failed run: node_modules/.cache/playwright/flows-results
# (or E2E_OUTPUT_DIR). E2E_PLAYWRIGHT_ARGS='03-tokens.spec.ts' runs one spec file (space-separated
# arguments only: a test title with spaces cannot be passed, pass its file; --list and
# --pass-with-no-tests are refused, as are --retries, --grep-invert, --shard, --last-failed,
# --only-changed, -c and --config in any form).
e2e-flows:
	go test -tags "compat e2e" -count=1 -timeout 30m -v -run '^(TestBrowserFlows|TestControl)' ./compat/e2e/

# Proves the suite can fail: runs named scenarios against the reference image behind a distorting
# proxy (compat/selfcheck). Slow; CI runs it daily (compat-selfcheck.yml), not on pull requests.
compat-selfcheck: compat-image-ts
	go test -tags "compat selfcheck" -count=1 -timeout 90m -v ./compat/selfcheck/

go-build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mcp-registry ./cmd/mcp-registry
