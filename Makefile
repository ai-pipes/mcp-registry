GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GO_ARCH_LINT := github.com/fe3dback/go-arch-lint@v1.19.2
GOFUMPT := mvdan.cc/gofumpt@v0.12.0
# The version of the gateway: the root VERSION file, the only source (no Node needed).
VERSION := $(strip $(shell cat VERSION 2>/dev/null))

# One guard for every target that stamps the version into a binary or an image.
.PHONY: check-version
check-version:
	@echo '$(VERSION)' | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "check-version: VERSION must hold MAJOR.MINOR.PATCH, got '$(VERSION)'"; exit 1; }

.PHONY: native-libs native-model go-test-onnx go-test-onnx-parity go-vet-onnx go-lint-onnx go-verify-onnx compat compat-selfcheck compat-schemas compat-reference print-reference-image compat-upgrade-from print-upgrade-from-image compat-image compat-reference-run load-compare e2e-flows go-lint go-lint-new go-fmt-check go-arch-lint go-cover go-verify go-test-int go-build print-go-arch-lint print-golangci-lint

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
# make compat-reference-run.
REFERENCE_IMAGE := ghcr.io/ai-pipes/mcp-registry:1.0.1@sha256:42726e227e5c2522b35c28c41e1feb6ceb8471ba3447ec5399a6c0559a72baf9
REFERENCE_REPO := $(firstword $(subst :, ,$(REFERENCE_IMAGE)))
REFERENCE_DIGEST := $(lastword $(subst @, ,$(REFERENCE_IMAGE)))

compat-reference:
	docker pull -q $(REFERENCE_IMAGE)
	@docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' $(REFERENCE_IMAGE) | grep -qx '$(REFERENCE_REPO)@$(REFERENCE_DIGEST)' || \
		{ echo "compat-reference: the local image does not carry $(REFERENCE_REPO)@$(REFERENCE_DIGEST)"; exit 1; }

print-reference-image:
	@echo $(REFERENCE_IMAGE)

# The image the upgrade scenario (TestUpgradeKeepsEverything) starts from in make compat and in
# make compat-reference-run: the published TypeScript 1.0.0 (the release operators run), pinned by the digest of
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

# The image built from this tree (the Dockerfile's default target, the image a release ships),
# tagged mcp-registry:compat: the default image under test of the suite and the browser flows
# (harness defaultImage). It is built natively for the host architecture; PLATFORM builds another one
# (emulated where needed): make compat-image PLATFORM=linux/amd64, tagged
# mcp-registry:compat-linux-amd64 so that the host image is not overwritten. The Dockerfile reads the
# version from the VERSION file itself (no build argument), as go-build does; the image must report it.
PLATFORM :=
IMAGE := mcp-registry:compat$(if $(PLATFORM),-$(subst /,-,$(PLATFORM)))
compat-image: check-version
	docker build $(if $(PLATFORM),--platform $(PLATFORM)) --target go -t $(IMAGE) .
	@got="$$(docker run --rm $(if $(PLATFORM),--platform $(PLATFORM)) $(IMAGE) mcp-registry --version)"; \
	if [ "$$got" != "$(VERSION)" ]; then echo "compat-image: the image reports version '$$got', VERSION says '$(VERSION)'"; exit 1; fi

# The image TestUpgradeKeepsEverything starts from under make compat: the published 1.0.0
# (UPGRADE_FROM_IMAGE, make compat-upgrade-from), so the image under test takes over a database the
# release operators run created, and rotates its key. Give it on the make command line to start from
# another image; empty (make compat COMPAT_UPGRADE_FROM=) makes the scenario a restart test of the
# image under test.
COMPAT_UPGRADE_FROM := $(UPGRADE_FROM_IMAGE)
# The packages compat and compat-reference-run test (the whole suite by default).
COMPAT_PKGS := ./compat/...

# The whole suite against the image built from this tree (make compat-image) with every decided
# deviation (COMPAT_FIXED unset means all) and the real upgrade from the published 1.0.0. The
# reference (make compat-reference) must exist too: some scenarios compare with it on the same
# machine, and the database hand-over scenarios start from it; so must the upgrade source.
# COMPAT_ARGS adds go test arguments, e.g. make compat COMPAT_ARGS='-run ^TestPlatform' (the -run
# patterns per area are in CONTRIBUTING.md). Nothing here sets COMPAT_REFERENCE: it matters only for
# -update, which the harness allows only for the reference (see compat-reference-run). -p 1 runs the
# test packages (compat, compat/harness, compat/fakes, compat/cmd) one after another: concurrent
# stacks on one machine stall the apps, and a stalled app turns into flaky timeouts.
compat:
	@docker image inspect $(REFERENCE_IMAGE) >/dev/null 2>&1 || { echo "compat: the reference image $(REFERENCE_IMAGE) is missing: run make compat-reference"; exit 1; }
	@[ -n "$$COMPAT_IMAGE" ] || docker image inspect mcp-registry:compat >/dev/null 2>&1 || { echo "compat: the image mcp-registry:compat is missing: run make compat-image"; exit 1; }
	@$(if $(COMPAT_UPGRADE_FROM),docker image inspect $(COMPAT_UPGRADE_FROM) >/dev/null 2>&1 || { echo "compat: the upgrade source $(COMPAT_UPGRADE_FROM) is missing: run make compat-upgrade-from"; exit 1; },true)
	$(if $(COMPAT_UPGRADE_FROM),COMPAT_UPGRADE_FROM='$(COMPAT_UPGRADE_FROM)' COMPAT_UPGRADE_FROM_CLI='$(UPGRADE_FROM_CLI)') \
		go test -tags compat -count=1 -p 1 -timeout 45m $(COMPAT_ARGS) $(COMPAT_PKGS)

# The reference leg: the same suite with the published 1.0.1 (REFERENCE_IMAGE) as the image under
# test, so every reference branch of the decided deviations stays asserted and the vector and
# ranking comparisons become determinism checks. Everything the harness would otherwise default to
# the image built from this tree is set explicitly: the reference's CLI, no decided deviation
# (COMPAT_FIXED set and empty) and the upgrade source with its CLI. Goldens are rewritten only here
# (the harness refuses -update for any other image, and with a decided deviation selected):
#   make compat-reference-run COMPAT_REFERENCE=1 COMPAT_PKGS=./compat/ COMPAT_ARGS='-run ^TestCLIVersion$$ -update'
# (one package and the one scenario that owns the golden: -update is global, and only the packages
# that import the harness know the flag).
REFERENCE_CLI := node dist/cli.js
compat-reference-run:
	@docker image inspect $(REFERENCE_IMAGE) >/dev/null 2>&1 || { echo "compat-reference-run: the reference image $(REFERENCE_IMAGE) is missing: run make compat-reference"; exit 1; }
	@$(if $(COMPAT_UPGRADE_FROM),docker image inspect $(COMPAT_UPGRADE_FROM) >/dev/null 2>&1 || { echo "compat-reference-run: the upgrade source $(COMPAT_UPGRADE_FROM) is missing: run make compat-upgrade-from"; exit 1; },true)
	COMPAT_IMAGE='$(REFERENCE_IMAGE)' COMPAT_CLI='$(REFERENCE_CLI)' COMPAT_FIXED= \
		$(if $(COMPAT_UPGRADE_FROM),COMPAT_UPGRADE_FROM='$(COMPAT_UPGRADE_FROM)' COMPAT_UPGRADE_FROM_CLI='$(UPGRADE_FROM_CLI)') \
		$(if $(COMPAT_REFERENCE),COMPAT_REFERENCE='$(COMPAT_REFERENCE)') \
		go test -tags compat -count=1 -p 1 -timeout 45m $(COMPAT_ARGS) $(COMPAT_PKGS)

# The browser flows (e2e/flows, Playwright with Chromium) against the image COMPAT_IMAGE (default
# mcp-registry:compat, see compat-image, with every decided deviation), orchestrated from Go. Needs
# `pnpm install` and `pnpm exec playwright install chromium`. Not part of `make compat` (tag e2e).
# It also runs the control API's own tests (TestControl*); the Docker-free ones (TestControlRefuses*,
# TestControlEcho*: the guarantee behind the API's token and fixed operations; TestPlaywrightArgs*:
# the arguments refused) run in go-verify too. Traces of a failed run:
# node_modules/.cache/playwright/flows-results (or E2E_OUTPUT_DIR). E2E_PLAYWRIGHT_ARGS='03-tokens.spec.ts'
# runs one spec file (space-separated arguments only: a test title with spaces cannot be passed,
# pass its file; --list and --pass-with-no-tests are refused, as are --retries, --grep-invert,
# --shard, --last-failed, --only-changed, -c and --config in any form).
e2e-flows:
	@[ -n "$$COMPAT_IMAGE" ] || docker image inspect mcp-registry:compat >/dev/null 2>&1 || { echo "e2e-flows: the image mcp-registry:compat is missing: run make compat-image"; exit 1; }
	go test -tags "compat e2e" -count=1 -timeout 30m -v -run '^(TestBrowserFlows|TestControl)' ./compat/e2e/

# The load comparison (compat/load_test.go, build tags compat and load, so neither make compat nor
# go-verify runs it): the same fixtures and traffic against two images, one stack at a time, a
# Markdown table on stdout and in LOAD_OUT (default a file under the system temp dir). It reports
# and asserts nothing. A is the published 1.0.0 (UPGRADE_FROM_IMAGE, make compat-upgrade-from), B the
# image built from this tree (make compat-image); every variable can be given on the make command line:
#   make load-compare LOAD_IMAGE_B=mcp-registry:other LOAD_OUT=/tmp/load.md
# LOAD_FIXED_A/B are the COMPAT_FIXED the harness boots A and B with (it only selects the branch a
# scenario asserts; the load test asserts none). About 3 minutes.
LOAD_IMAGE_A := $(UPGRADE_FROM_IMAGE)
LOAD_CLI_A := $(UPGRADE_FROM_CLI)
LOAD_IMAGE_B := mcp-registry:compat
LOAD_CLI_B := mcp-registry
LOAD_FIXED_A :=
LOAD_FIXED_B := all
LOAD_OUT :=
load-compare:
	@docker image inspect '$(LOAD_IMAGE_A)' >/dev/null 2>&1 || { echo "load-compare: the image A '$(LOAD_IMAGE_A)' is missing: run make compat-upgrade-from (or give LOAD_IMAGE_A)"; exit 1; }
	@docker image inspect '$(LOAD_IMAGE_B)' >/dev/null 2>&1 || { echo "load-compare: the image B '$(LOAD_IMAGE_B)' is missing: run make compat-image (or give LOAD_IMAGE_B)"; exit 1; }
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
