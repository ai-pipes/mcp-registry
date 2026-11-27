GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GO_ARCH_LINT := github.com/fe3dback/go-arch-lint@v1.19.2
GOFUMPT := mvdan.cc/gofumpt@v0.12.0
# The version of the gateway: the root VERSION file, the only source (no Node needed).
VERSION := $(strip $(shell cat VERSION 2>/dev/null))

# One guard for every target that stamps the version into a binary or an image.
.PHONY: check-version
check-version:
	@echo '$(VERSION)' | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "check-version: VERSION must hold MAJOR.MINOR.PATCH, got '$(VERSION)'"; exit 1; }

.PHONY: native-libs native-model go-test-onnx go-test-onnx-parity go-vet-onnx go-lint-onnx go-verify-onnx compat compat-selfcheck compat-schemas compat-reference print-reference-image compat-upgrade-from print-upgrade-from-image compat-image-ts compat-image-go compat-go load-compare e2e-flows e2e-flows-go go-lint go-lint-new go-fmt-check go-arch-lint go-cover go-verify go-test-int go-build print-go-arch-lint print-golangci-lint

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
# The coverage gate: domain and app of every module cover at least 90 % of their statements, per
# package, measured by the unit tests alone (no build tags, no Docker; integration tests do not
# count). internal/archtest/cmd/covergate lists the expected packages itself and fails for a
# missing or empty one. The unit tests run with coverage here, which go test ./... cannot reuse
# (different flags, different cache entry): about 7 s.
go-cover:
	@profile="$$(mktemp)"; trap 'rm -f "$$profile"' EXIT; \
	go test -count=1 -coverprofile="$$profile" ./internal/modules/*/domain ./internal/modules/*/app >/dev/null || { echo "go-cover: unit tests failed (run go test ./internal/modules/...)"; exit 1; }; \
	go run ./internal/archtest/cmd/covergate -profile "$$profile" -root .

go-verify: go-lint go-arch-lint go-cover
	go vet ./...
	go vet -tags integration ./...
	go vet -tags compat ./...
	go vet -tags "compat selfcheck" ./compat/selfcheck/
	go vet -tags "compat load" ./compat/
	go vet -tags "compat e2e" ./compat/e2e/
	go test -tags "compat e2e" -count=1 -run '^(TestControl(Refuses|Echo)|TestPlaywrightArgs)' ./compat/e2e/
	# Docker-free tests of the suite itself (none boots a stack; they pass with the Docker socket
	# unreachable). A test that needs no Docker is added here by name.
	go test -tags compat -count=1 -run '^(TestAppArgsPublishTheContainerPortAndKeepFileContentOffTheCommandLine|TestAppEnvReadsTheEnvironment|TestCheckUpdate|TestCheckUpdateAllowedReadsTheEnvironment|TestCLIResultStringMasksTokensAndLinkCodes|TestDefaultImageAndCLIFollowTheEnvironment|TestDoChunkedExpectingEarlyRefusalSendsNoContentLength|TestDoExpectingEarlyRefusalReturnsTheAnswerWhateverHappenedToTheWrite|TestErrorNamesSchemaAndPath|TestEveryContractHasASchema|TestEveryDeviationHasADescriptionAndIsUsedByAScenario|TestFinishSavesTheLogOnlyForAFailedTest|TestFixedFollowsTheEnvironment|TestListSuffixAndUnknownName|TestLogFileName|TestLoopbackHostResolution|TestOneOffCLIContainerNeverPutsAValueOnTheCommandLine|TestOtherFormatsAreAsserted|TestParseAppEnv|TestParseFixed|TestPortTakenRecognisesDockersMessages|TestPublicHostAndExtraHostsReachTheContainerArguments|TestRedactTruncates|TestReferenceImageFollowsItsVariable|TestResponseDecode|TestSaveLogDoesNothingWithoutADirectory|TestSchemaRejectsExtraAndMissingFields|TestScrubRemovesCredentialShapes|TestScrubRemovesTheContentOfSecretFiles|TestSecretValuesListTheBareDatabasePasswordToo|TestServerViewWithSecretFieldIsRejected|TestTheReferenceIsPinnedByDigestTheSameEverywhere|TestUpdateRefusesNonReferenceImage|TestURLFormatsAreAsserted|TestWriteLogScrubsAndCreatesTheDirectory)$$' ./compat/harness/
	# The smoke tools of scripts/smoke.sh (compat/cmd): every test there is Docker-free.
	go test -tags compat -count=1 ./compat/cmd/...
	go test -tags compat -count=1 -run '^(TestAuditPagingProblem|TestCreateServerSyncFailureProblem|TestMCPReaderIsLenient|TestReleaseWorkflowGuardRefusesBrokenCopies|TestReleaseWorkflowKeepsThePublishingPrivilegeAndNativeBuilds|TestRetryAfterProblem|TestSearchReadyProblem|TestToolFixtureCopiesEqualTheOriginals|TestUpgradeSourceIsThePublished100PinnedByDigest|TestVersionFileEqualsTheServerPackage)$$' ./compat/
	go test ./...
	$(MAKE) go-test-int

# Integration tests live in internal/platform and internal/modules. -p 1: testcontainers derives its session from the
# parent test process, so parallel test binaries race to create the shared reaper and some fail.
# The reaper removes a session's containers 10 s after its last client left; compiling the next
# package's test binary on a cold cache can take longer, and that binary then meets containers
# being removed. A minute covers it.
go-test-int:
	TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT=60s go test -tags integration -count=1 -p 1 ./internal/platform/... ./internal/modules/...

# Regenerates compat/schemas from the zod contracts; CI fails when the committed files are stale.
compat-schemas:
	node --experimental-strip-types packages/contracts/scripts/export-json-schema.mjs compat/schemas

# The reference implementation the suite compares with (vectors, rankings, the database hand-over,
# the goldens, the embedder parity run, the self-check base): the published TypeScript image 1.0.1,
# pinned by the digest of its multi-arch index (linux/amd64, linux/arm64). The harness's
# defaultReferenceImage (compat/harness/stack.go) and compat/selfcheck/Dockerfile hold the same
# value; a Docker-free test keeps the three equal. compat-reference pulls it anonymously and checks
# that the local image carries that digest. The suite with the reference as the image under test:
#   make compat COMPAT_IMAGE="$$(make -s print-reference-image)"
REFERENCE_IMAGE := ghcr.io/ai-pipes/mcp-registry:1.0.1@sha256:42726e227e5c2522b35c28c41e1feb6ceb8471ba3447ec5399a6c0559a72baf9
REFERENCE_REPO := $(firstword $(subst :, ,$(REFERENCE_IMAGE)))
REFERENCE_DIGEST := $(lastword $(subst @, ,$(REFERENCE_IMAGE)))

compat-reference:
	docker pull -q $(REFERENCE_IMAGE)
	@docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' $(REFERENCE_IMAGE) | grep -qx '$(REFERENCE_REPO)@$(REFERENCE_DIGEST)' || \
		{ echo "compat-reference: the local image does not carry $(REFERENCE_REPO)@$(REFERENCE_DIGEST)"; exit 1; }

print-reference-image:
	@echo $(REFERENCE_IMAGE)

# The image the upgrade scenario (TestUpgradeKeepsEverything) starts from in compat-go and in CI's
# reference leg: the published TypeScript 1.0.0 (the release operators run), pinned by the digest of
# its multi-arch index (linux/amd64 and linux/arm64; resolved with `docker buildx imagetools inspect
# ghcr.io/ai-pipes/mcp-registry:1.0.0`). compat-upgrade-from pulls it anonymously and checks that the
# local image carries that digest. Dependabot does not watch this line.
UPGRADE_FROM_IMAGE := ghcr.io/ai-pipes/mcp-registry:1.0.0@sha256:9966fa11e597b45fe27ad916b4121a312c624232a1855d922c1f1946f7c09d72
UPGRADE_FROM_REPO := $(firstword $(subst :, ,$(UPGRADE_FROM_IMAGE)))
UPGRADE_FROM_DIGEST := $(lastword $(subst @, ,$(UPGRADE_FROM_IMAGE)))
UPGRADE_FROM_CLI := node dist/cli.js

compat-upgrade-from:
	docker pull -q $(UPGRADE_FROM_IMAGE)
	@docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' $(UPGRADE_FROM_IMAGE) | grep -qx '$(UPGRADE_FROM_REPO)@$(UPGRADE_FROM_DIGEST)' || \
		{ echo "compat-upgrade-from: the local image does not carry $(UPGRADE_FROM_REPO)@$(UPGRADE_FROM_DIGEST)"; exit 1; }

print-upgrade-from-image:
	@echo $(UPGRADE_FROM_IMAGE)

# The TypeScript image built from this tree (the default image under test until the switchover).
compat-image-ts:
	docker build -t mcp-registry:compat-ts .

# The Go image (Dockerfile target go), tagged mcp-registry:compat-go: cgo, build tag onnx, ONNX
# Runtime and the model inside. It is built natively for the host architecture; PLATFORM builds
# another one (emulated where needed): make compat-image-go PLATFORM=linux/amd64, tagged
# mcp-registry:compat-go-linux-amd64 so that the host image is not overwritten. The Dockerfile
# reads the version from the VERSION file itself (no build argument), as go-build does. The image
# carries its HEALTHCHECK (`mcp-registry healthcheck`). The default target (runtime, the TypeScript
# image) is untouched.
PLATFORM :=
GO_IMAGE := mcp-registry:compat-go$(if $(PLATFORM),-$(subst /,-,$(PLATFORM)))
compat-image-go: check-version
	docker build $(if $(PLATFORM),--platform $(PLATFORM)) --target go -t $(GO_IMAGE) .
	@got="$$(docker run --rm $(if $(PLATFORM),--platform $(PLATFORM)) $(GO_IMAGE) mcp-registry --version)"; \
	if [ "$$got" != "$(VERSION)" ]; then echo "compat-image-go: the image reports version '$$got', VERSION says '$(VERSION)'"; exit 1; fi

# The scenarios the Go server passes so far (the milestone list); it grows with every milestone.
# Both variables are set with := here, so an environment variable of the same name does NOT
# override them; override on the make command line instead:
#   make compat-go COMPAT_GO_TESTS='TestConfigRefused TestPlatformWithoutPeople'
#   make compat-go COMPAT_GO_SKIP=TestCLIKeys   # nothing is skipped by default
# In a value given that way (or edited here), `$$` is a literal `$` for make; a value is passed to
# go test inside single quotes, so it must not contain one.
#
# Milestone 2 (identity) added the scenarios that need people, sign-in, sessions and tokens but no
# /mcp and no catalog. Milestone 3 (catalog) added the scenarios of the catalog REST and of the
# commands create-server and sync, and TestPlatformBearerNeverOpensTheWebAPI; the three steps of
# TestCLICreateUser that listed /api/servers run again.
# Milestone 4 (search) added the scenarios that judge the index, the stored vectors and the reindex
# command: TestSearchStoredVectors (the milestone's judge: every stored vector equals the reference
# image's on the same machine), TestCLIReindexEmpty, TestConfigOpenAICompatibleEmbedder,
# TestConfigModelDirMissing, TestSearchFailedReembedding, TestSearchStaleRemoval and TestCatalogSearch.
# Milestone 5 (connections) added the connections scenarios, the personal-auth sync step of
# TestCLIServers and the hand-over of connections between the images (TestPlatformStartsOnReferenceDatabase
# and TestPlatformReverseHandOver).
# Milestone 6 (the gateway) added the scenarios that judge /mcp, call_tool and the call log:
# TestMCPProtocolClassic, TestMCPSDKClient, TestMCPSurface, TestMCPGuards, TestMCPMetaTools,
# TestGatewayCall, TestGatewayFaults, TestGatewayCallsAPI and TestGatewayAuditAPI; the credentials
# through call_tool (TestConnectionsRefresh, TestConnectionsRefreshFailures, TestConnectionsSingleFlight,
# TestConnectionsUpstreamRejectsTheToken); TestSearchReadyBeforeServing (the decided ready-before-serving
# branch: a search made at once after a restart equals the settled one); TestConfigNetworkAndRetention
# and TestConfigSecretsFromFiles (their cleanup demands that the scenario's MCP ledger saw an answer);
# and TestUpgradeKeepsEverything (since milestone 8 compat-go runs it as the real upgrade: the published
# 1.0.0 creates the state, the Go image takes over, see COMPAT_GO_UPGRADE_FROM).
# Milestone 7 (the operator CLI) added TestCLIKeys (rotate-secrets-key and reset-key-check) and the
# key-rotation subtest of TestPlatformMigrationsAndKeyCheck: nothing is left out for the Go image.
COMPAT_GO_TESTS := TestConfigRefused TestPlatformStartsOnReferenceDatabase TestPlatformReverseHandOver \
	TestPlatformWithoutPeople TestPlatformMigrationsAndKeyCheck TestPlatformStaticFilesAndSPA \
	TestIdentityOIDC TestIdentityOIDCFailedCallbackAudit TestIdentityOIDCProviderDownAtStart \
	TestIdentityNoOIDC TestIdentityDefaultRateLimits TestIdentityDisablingAPerson \
	TestCLIVersion TestCLICreateUser TestCLICreateUserRoleFlags TestCLICreateToken TestCLILoginLink TestCLILoginLinkWithOIDC \
	TestFirstStartAnnouncement TestFirstStartAnnouncementNeedsAnEmptyDatabaseAndNoOIDC \
	TestConfigEmptyMeansUnset TestPlatformMethodsWithoutOIDC TestPlatformCSRF TestPlatformHealthz \
	TestPlatformAccessLog TestPlatformBodyLimit TestPlatformSecurityHeaders TestIdentityOIDCBindingKept \
	TestCatalogOptimisticConcurrency TestCatalogValidation TestCatalogServers TestCLIServers \
	TestPlatformBearerNeverOpensTheWebAPI \
	TestCatalogSearch TestSearchStoredVectors TestCLIReindexEmpty TestConfigOpenAICompatibleEmbedder \
	TestConfigModelDirMissing TestSearchFailedReembedding TestSearchStaleRemoval \
	TestConnectionsConnectFlow TestConnectionsConnectCallbackRefusals TestConnectionsStartRefusals \
	TestConnectionsPAT TestConnectionsArePersonal TestConnectionsWebAPIRules TestConnectionsConfigChange \
	TestConnectionsDisconnectIgnoresRevokeFailures TestConnectionsAdminSync \
	TestConnectionsConfigChangeBySync \
	TestMCPProtocolClassic TestMCPSDKClient TestMCPSurface TestMCPGuards TestMCPMetaTools \
	TestGatewayCall TestGatewayFaults TestGatewayCallsAPI TestGatewayAuditAPI \
	TestConnectionsRefresh TestConnectionsRefreshFailures TestConnectionsSingleFlight \
	TestConnectionsUpstreamRejectsTheToken TestSearchReadyBeforeServing \
	TestConfigNetworkAndRetention TestConfigSecretsFromFiles TestUpgradeKeepsEverything \
	TestCLIKeys

# Subtests of the scenarios above that need a later milestone, as a go test -skip expression: one
# entry per line below, joined with `|` (slash-separated regular expressions per level; spaces of a
# subtest name are underscores; an entry names the milestone whose work removes it). Since milestone 7
# the list is empty: nothing is skipped for the Go image. With no entry, COMPAT_GO_SKIP is empty and
# -skip is not passed at all (see the compat-go recipe).
COMPAT_GO_SKIP_ENTRIES :=

space := $(subst ,, )
COMPAT_GO_SKIP := $(subst $(space),|,$(strip $(COMPAT_GO_SKIP_ENTRIES)))
COMPAT_GO_RUN := ^($(subst $(space),|,$(strip $(COMPAT_GO_TESTS))))$$

# The deviations (compat/harness/deviations.go) whose decided behaviour the Go server has, for
# compat-go and e2e-flows-go: config-errors (milestone 1), audit-login-via, create-user-keeps-role
# and oidc-binding-kept (milestone 2), create-server-sync-exit (milestone 3), reindex-wording
# and vector-kept-on-failed-reembedding and stale-removal-rechecks-catalog (milestone 4),
# oauth-state-kept (milestone 5), and request-id-on-refusals, token-429-retry-after and
# ready-before-serving (milestone 6: the /mcp route, its token check and the search made at once
# after a start). All twelve.
COMPAT_GO_FIXED := config-errors,audit-login-via,create-user-keeps-role,oidc-binding-kept,create-server-sync-exit,reindex-wording,vector-kept-on-failed-reembedding,stale-removal-rechecks-catalog,oauth-state-kept,request-id-on-refusals,token-429-retry-after,ready-before-serving

# The image TestUpgradeKeepsEverything starts from under compat-go: the published 1.0.0 (UPGRADE_FROM_IMAGE,
# make compat-upgrade-from), so the Go image takes over a database the TypeScript release created, and
# rotates its key. Give it on the make command line to start from another image; empty
# (make compat-go COMPAT_GO_UPGRADE_FROM=) makes the scenario a restart test of the Go image.
COMPAT_GO_UPGRADE_FROM := $(UPGRADE_FROM_IMAGE)

# The suite against the Go image. The reference image (REFERENCE_IMAGE, make compat-reference) must
# exist as well: some scenarios compare with it on the same machine, and the new-database scenario
# starts from it; so must the upgrade source (COMPAT_GO_UPGRADE_FROM, make compat-upgrade-from).
# Before it runs, the pattern is
# listed (nothing is started): it must match exactly the scenarios named in COMPAT_GO_TESTS, so a
# renamed or mistyped scenario fails here instead of passing by running nothing.
compat-go:
	@docker image inspect $(REFERENCE_IMAGE) >/dev/null 2>&1 || { echo "compat-go: the reference image $(REFERENCE_IMAGE) is missing: run make compat-reference"; exit 1; }
	@docker image inspect mcp-registry:compat-go >/dev/null 2>&1 || { echo "compat-go: the image mcp-registry:compat-go is missing: run make compat-image-go"; exit 1; }
	@$(if $(COMPAT_GO_UPGRADE_FROM),docker image inspect $(COMPAT_GO_UPGRADE_FROM) >/dev/null 2>&1 || { echo "compat-go: the upgrade source $(COMPAT_GO_UPGRADE_FROM) is missing: run make compat-upgrade-from"; exit 1; },true)
	@listed="$$(go test -tags compat -count=1 -list '$(COMPAT_GO_RUN)' ./compat/ | grep -c '^Test')"; \
	if [ "$$listed" != "$(words $(COMPAT_GO_TESTS))" ]; then \
		echo "compat-go: the pattern '$(COMPAT_GO_RUN)' matches $$listed scenario(s), COMPAT_GO_TESTS names $(words $(COMPAT_GO_TESTS)): $(COMPAT_GO_TESTS)"; exit 1; fi
	COMPAT_IMAGE=mcp-registry:compat-go COMPAT_CLI=mcp-registry COMPAT_FIXED=$(COMPAT_GO_FIXED) \
		$(if $(COMPAT_GO_UPGRADE_FROM),COMPAT_UPGRADE_FROM='$(COMPAT_GO_UPGRADE_FROM)' COMPAT_UPGRADE_FROM_CLI='$(UPGRADE_FROM_CLI)') \
		go test -tags compat -count=1 -p 1 -timeout 45m -run '$(COMPAT_GO_RUN)' $(if $(COMPAT_GO_SKIP),-skip '$(COMPAT_GO_SKIP)') $(COMPAT_ARGS) ./compat/

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

# All twelve browser flows against the Go image (build it with compat-image-go): identity, the catalog, connections, and since milestone 6 the
# token on /mcp (03-tokens) and a call through /mcp with the call log (09-permissions).
# TestControlOperationsAgainstARealStack is not run: the control API's tests are image independent and
# run in e2e-flows. Same guard, same specs: the arguments go through E2E_PLAYWRIGHT_ARGS, which refuses
# the ways to run fewer tests silently. An E2E_PLAYWRIGHT_ARGS given on the make command line replaces
# the list.
E2E_GO_SPECS := 00-guard-selftest.spec.ts 01-first-start-link.spec.ts 02-oidc-login.spec.ts \
	03-tokens.spec.ts 04-create-server.spec.ts 05-form-errors.spec.ts 06-hide-tool.spec.ts 07-edit-conflict.spec.ts \
	08-connect.spec.ts 08b-connect-cross-site.spec.ts 09-permissions.spec.ts 10-disable-person.spec.ts 11-session-expiry.spec.ts \
	12-direct-links.spec.ts
e2e-flows-go:
	@docker image inspect mcp-registry:compat-go >/dev/null 2>&1 || { echo "e2e-flows-go: the image mcp-registry:compat-go is missing: run make compat-image-go"; exit 1; }
	COMPAT_IMAGE=mcp-registry:compat-go COMPAT_CLI=mcp-registry COMPAT_FIXED=$(COMPAT_GO_FIXED) \
		E2E_PLAYWRIGHT_ARGS='$(E2E_GO_SPECS)' \
		go test -tags "compat e2e" -count=1 -timeout 30m -v -run '^TestBrowserFlows$$' ./compat/e2e/

# The load comparison (compat/load_test.go, build tags compat and load, so neither make compat nor
# go-verify runs it): the same fixtures and traffic against two images, one stack at a time, a
# Markdown table on stdout and in LOAD_OUT (default a file under the system temp dir). It reports
# and asserts nothing. A is the published 1.0.0 (UPGRADE_FROM_IMAGE, make compat-upgrade-from), B the
# Go image (make compat-image-go); every variable can be given on the make command line:
#   make load-compare LOAD_IMAGE_B=mcp-registry:other LOAD_OUT=/tmp/load.md
# LOAD_FIXED_B is the COMPAT_FIXED the harness boots B with (it only selects the branch a scenario
# asserts; the load test asserts none). About 3 minutes.
LOAD_IMAGE_A := $(UPGRADE_FROM_IMAGE)
LOAD_CLI_A := $(UPGRADE_FROM_CLI)
LOAD_IMAGE_B := mcp-registry:compat-go
LOAD_CLI_B := mcp-registry
LOAD_FIXED_A :=
LOAD_FIXED_B := $(COMPAT_GO_FIXED)
LOAD_OUT :=
load-compare:
	@docker image inspect '$(LOAD_IMAGE_A)' >/dev/null 2>&1 || { echo "load-compare: the image A '$(LOAD_IMAGE_A)' is missing: run make compat-upgrade-from (or give LOAD_IMAGE_A)"; exit 1; }
	@docker image inspect '$(LOAD_IMAGE_B)' >/dev/null 2>&1 || { echo "load-compare: the image B '$(LOAD_IMAGE_B)' is missing: run make compat-image-go (or give LOAD_IMAGE_B)"; exit 1; }
	LOAD_IMAGE_A='$(LOAD_IMAGE_A)' LOAD_CLI_A='$(LOAD_CLI_A)' LOAD_FIXED_A='$(LOAD_FIXED_A)' \
		LOAD_IMAGE_B='$(LOAD_IMAGE_B)' LOAD_CLI_B='$(LOAD_CLI_B)' LOAD_FIXED_B='$(LOAD_FIXED_B)' \
		$(if $(LOAD_OUT),LOAD_OUT='$(LOAD_OUT)') \
		go test -tags "compat load" -count=1 -timeout 30m -run '^TestLoadCompare$$' -v ./compat/

# Proves the suite can fail: runs named scenarios against the reference image (the published 1.0.1,
# REFERENCE_IMAGE) behind a distorting proxy (compat/selfcheck). The base is SELFCHECK_BASE when the
# caller sets it (environment or make command line), else harness.ReferenceImage (COMPAT_REFERENCE_IMAGE,
# else the published reference); the published reference is pulled either way. Slow; CI runs it
# daily (compat-selfcheck.yml), not on pull requests.
compat-selfcheck: compat-reference
	go test -tags "compat selfcheck" -count=1 -timeout 90m -v ./compat/selfcheck/

go-build: check-version
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mcp-registry ./cmd/mcp-registry

# --- The local embedder (build tag onnx) ---------------------------------------------------------
#
# The package internal/modules/search/infra/onnx compiles its cgo files only with `-tags onnx`;
# `make go-verify` never needs native libraries. Local builds and tests of the tagged code use the
# libraries below, downloaded into a git-ignored cache (.cache/native/<platform>/) with every
# SHA-256 verified and nothing downloaded ever executed (native/fetch.sh).
#
# Pins: URLs and SHA-256 values live only in internal/modules/search/infra/onnx/native/pins.env
# (provenance in its header). The Makefile includes it for the file name; fetch.sh and the
# Dockerfile stage go-native read it themselves, so there is one list. A Go test fails if a
# pin is repeated elsewhere.
include internal/modules/search/infra/onnx/native/pins.env
NATIVE_ROOT := .cache/native
NATIVE_PLATFORM := $(shell go env GOOS)-$(shell go env GOARCH)
NATIVE_KEY := $(subst -,_,$(NATIVE_PLATFORM))
NATIVE_DIR := $(NATIVE_ROOT)/$(NATIVE_PLATFORM)
NATIVE_ABS := $(abspath $(NATIVE_DIR))

ONNX_PKG := ./internal/modules/search/infra/onnx/...
# Variables the tagged tests read (names are for local development only, not operator settings).
ONNX_TEST_ENV = CGO_ENABLED=1 CGO_LDFLAGS="-L$(NATIVE_ABS)" ONNX_TEST_LIB="$(NATIVE_ABS)/$(ORT_FILE_$(NATIVE_KEY))" ONNX_TEST_MODEL_DIR="$(abspath $(NATIVE_ROOT))/models"

native-libs:
	@test -n "$(ORT_FILE_$(NATIVE_KEY))" || { echo "native-libs: no pinned libraries for $(NATIVE_PLATFORM)"; exit 1; }
	sh internal/modules/search/infra/onnx/native/fetch.sh $(NATIVE_PLATFORM) $(NATIVE_DIR)

# The model and tokenizer both servers use (revision and SHA-256 pinned in pins.env), in the layout
# <root>/<MODEL_NAME>/, fetched by fetch-model.sh (curl and sha256sum, no Node; nothing downloaded
# is executed). The Dockerfile stage model runs the same script.
native-model:
	sh internal/modules/search/infra/onnx/native/fetch-model.sh $(NATIVE_ROOT)/models

go-vet-onnx: native-libs
	$(ONNX_TEST_ENV) go vet -tags onnx $(ONNX_PKG)

go-lint-onnx: native-libs
	$(ONNX_TEST_ENV) go run $(GOLANGCI_LINT) run --build-tags onnx $(ONNX_PKG)

go-test-onnx: native-libs native-model
	$(ONNX_TEST_ENV) go test -tags onnx -count=1 $(ONNX_PKG)

go-verify-onnx: go-vet-onnx go-lint-onnx go-test-onnx

# Vector parity with the reference, linux containers only: the reference image (REFERENCE_IMAGE,
# make compat-reference) makes the vectors of the committed texts with Node (--network none), then the tagged test runs in a
# golang container of the same architecture with the linux native libraries, so both sides run the
# same ONNX Runtime build on the same machine. Vectors depend on the CPU, so they are never committed.
PARITY_DIR := $(abspath $(NATIVE_ROOT))/parity
PARITY_LINUX := linux-$(shell go env GOARCH)
ONNX_GEN := /repo/internal/modules/search/infra/onnx/testdata

# The parity run is only worth something when the two tests really ran: ONNX_TEST_PARITY_REQUIRED=1
# makes them fail instead of skip when an input is missing, and the recipe fails unless the log
# reports both as passed. The first container runs as the caller's user (on a GitHub-hosted runner
# that is not uid 1000, `node` of the image, and could not write into the output directory; HOME is
# a writable /tmp for node). The second one runs as root on purpose: it only reads its bind mounts
# (all :ro) and writes only to its own /tmp (GOCACHE), so no user id can make it fail. The golang
# image is the Dockerfile's, by digest (a test in infra/onnx keeps the two equal).
PARITY_GO_IMAGE := golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea

go-test-onnx-parity:
	@docker image inspect $(REFERENCE_IMAGE) >/dev/null 2>&1 || { echo "go-test-onnx-parity: the reference image $(REFERENCE_IMAGE) is missing; pull it with: make compat-reference"; exit 1; }
	$(MAKE) native-libs NATIVE_PLATFORM=$(PARITY_LINUX)
	$(MAKE) native-model
	mkdir -p $(PARITY_DIR)
	rm -f $(PARITY_DIR)/node-vectors.json $(PARITY_DIR)/go-test.log
	docker run --rm --network none --user "$$(id -u):$$(id -g)" -e HOME=/tmp -v "$(CURDIR)":/repo:ro -v "$(PARITY_DIR)":/out $(REFERENCE_IMAGE) \
		node $(ONNX_GEN)/gen/vectors.mjs /app/apps/server/node_modules/@huggingface/transformers/dist/transformers.node.mjs \
		/app/models $(ONNX_GEN)/reference.json /out/node-vectors.json
	docker run --rm --network none -v "$(CURDIR)":/src:ro -v "$$(go env GOMODCACHE)":/gomod:ro -v "$(PARITY_DIR)":/out:ro \
		-e GOMODCACHE=/gomod -e GOPROXY=off -e GOTOOLCHAIN=local -e GOCACHE=/tmp/gocache -e GOFLAGS=-buildvcs=false \
		-e CGO_ENABLED=1 -e CGO_LDFLAGS=-L/src/$(NATIVE_ROOT)/$(PARITY_LINUX) \
		-e ONNX_TEST_LIB=/src/$(NATIVE_ROOT)/$(PARITY_LINUX)/libonnxruntime.so.1 -e ONNX_TEST_MODEL_DIR=/src/$(NATIVE_ROOT)/models \
		-e ONNX_TEST_REFERENCE_VECTORS=/out/node-vectors.json -e ONNX_TEST_PARITY_REQUIRED=1 -w /src $(PARITY_GO_IMAGE) \
		go test -tags onnx -count=1 -v -run 'TestTokenIDsMatchTheReference|TestVectorsMatchTheReferenceBitForBit' ./internal/modules/search/infra/onnx/ \
		> $(PARITY_DIR)/go-test.log 2>&1; rc=$$?; cat $(PARITY_DIR)/go-test.log; [ $$rc -eq 0 ] || exit $$rc
	@for t in TestTokenIDsMatchTheReference TestVectorsMatchTheReferenceBitForBit; do \
		grep -q -- "--- PASS: $$t " $(PARITY_DIR)/go-test.log || { echo "go-test-onnx-parity: $$t did not report PASS"; exit 1; }; done
