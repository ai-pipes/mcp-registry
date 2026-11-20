GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GO_ARCH_LINT := github.com/fe3dback/go-arch-lint@v1.19.2
GOFUMPT := mvdan.cc/gofumpt@v0.12.0
VERSION := $(shell node -p "require('./apps/server/package.json').version")

.PHONY: compat compat-selfcheck compat-schemas compat-image-ts e2e-flows go-lint go-lint-new go-fmt-check go-arch-lint go-verify go-test-int go-build print-go-arch-lint print-golangci-lint

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
	go test -tags compat -count=1 -run '^(TestAppArgsPublishTheContainerPortAndKeepFileContentOffTheCommandLine|TestAppEnvReadsTheEnvironment|TestCheckUpdate|TestCheckUpdateAllowedReadsTheEnvironment|TestCLIResultStringMasksTokensAndLinkCodes|TestDefaultImageAndCLIFollowTheEnvironment|TestDoExpectingEarlyRefusalReturnsTheAnswerWhateverHappenedToTheWrite|TestErrorNamesSchemaAndPath|TestEveryContractHasASchema|TestEveryDeviationHasADescriptionAndIsUsedByAScenario|TestFinishSavesTheLogOnlyForAFailedTest|TestFixedFollowsTheEnvironment|TestListSuffixAndUnknownName|TestLogFileName|TestLoopbackHostResolution|TestOneOffCLIContainerNeverPutsAValueOnTheCommandLine|TestOtherFormatsAreAsserted|TestParseAppEnv|TestParseFixed|TestPortTakenRecognisesDockersMessages|TestPublicHostAndExtraHostsReachTheContainerArguments|TestRedactTruncates|TestResponseDecode|TestSaveLogDoesNothingWithoutADirectory|TestSchemaRejectsExtraAndMissingFields|TestScrubRemovesCredentialShapes|TestScrubRemovesTheContentOfSecretFiles|TestSecretValuesListTheBareDatabasePasswordToo|TestServerViewWithSecretFieldIsRejected|TestUpdateRefusesNonReferenceImage|TestURLFormatsAreAsserted|TestWriteLogScrubsAndCreatesTheDirectory)$$' ./compat/harness/
	go test -tags compat -count=1 -run '^(TestAuditPagingProblem|TestCreateServerSyncFailureProblem|TestMCPReaderIsLenient|TestRetryAfterProblem|TestSearchReadyProblem)$$' ./compat/
	go test ./...
	$(MAKE) go-test-int

# -p 1: every test binary starts its own Postgres container and the testcontainers reaper they
# share fails when several binaries create it at once.
go-test-int:
	go test -tags integration -count=1 -p 1 ./internal/...

# Regenerates compat/schemas from the zod contracts; CI fails when the committed files are stale.
compat-schemas:
	node --experimental-strip-types packages/contracts/scripts/export-json-schema.mjs compat/schemas

compat-image-ts:
	docker build -t mcp-registry:compat-ts .

# COMPAT_ARGS adds go test arguments, e.g. make compat COMPAT_ARGS='-run ^TestPlatform' (the -run
# patterns per area are in CONTRIBUTING.md). Nothing here sets COMPAT_REFERENCE: it matters only
# for -update, which the harness allows only for the reference image. -p 1 runs the test packages
# (compat, compat/harness, compat/fakes) one after another: concurrent stacks on one machine stall
# the app's event loop (it loads its embedding model right after it first answers healthy), and a
# stalled app turns into flaky timeouts.
compat:
	go test -tags compat -count=1 -p 1 -timeout 30m $(COMPAT_ARGS) ./compat/...

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
