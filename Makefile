GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GO_ARCH_LINT := github.com/fe3dback/go-arch-lint@v1.19.2
GOFUMPT := mvdan.cc/gofumpt@v0.12.0
VERSION := $(shell node -p "require('./apps/server/package.json').version")

.PHONY: native-libs native-model go-test-onnx go-vet-onnx go-lint-onnx go-verify-onnx compat compat-selfcheck compat-schemas compat-image-ts compat-image-go compat-go e2e-flows e2e-flows-go go-lint go-lint-new go-fmt-check go-arch-lint go-verify go-test-int go-build print-go-arch-lint print-golangci-lint

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
#
# Milestone 2 (identity) added the scenarios that need people, sign-in, sessions and tokens but no
# /mcp and no catalog. Milestone 3 (catalog) added the scenarios of the catalog REST and of the
# commands create-server and sync, and TestPlatformBearerNeverOpensTheWebAPI; the three steps of
# TestCLICreateUser that listed /api/servers run again. Left out because they cannot run at all yet:
#   - TestConfigNetworkAndRetention, TestConfigSecretsFromFiles: their cleanup demands that the
#     scenario's MCP ledger saw an answer (milestone 6), whatever subtests are skipped.
#   - TestUpgradeKeepsEverything, TestGatewayAuditAPI, TestConfigModelDirMissing and every
#     connections, search, gateway, MCP scenario and the operator commands reindex,
#     rotate-secrets-key, reset-key-check (milestones 4 to 7).
COMPAT_GO_TESTS := TestConfigRefused TestPlatformStartsOnReferenceDatabase TestPlatformReverseHandOver \
	TestPlatformWithoutPeople TestPlatformMigrationsAndKeyCheck TestPlatformStaticFilesAndSPA \
	TestIdentityOIDC TestIdentityOIDCFailedCallbackAudit TestIdentityOIDCProviderDownAtStart \
	TestIdentityNoOIDC TestIdentityDefaultRateLimits TestIdentityDisablingAPerson \
	TestCLIVersion TestCLICreateUser TestCLICreateUserRoleFlags TestCLICreateToken TestCLILoginLink TestCLILoginLinkWithOIDC \
	TestFirstStartAnnouncement TestFirstStartAnnouncementNeedsAnEmptyDatabaseAndNoOIDC \
	TestConfigEmptyMeansUnset TestPlatformMethodsWithoutOIDC TestPlatformCSRF TestPlatformHealthz \
	TestPlatformAccessLog TestPlatformBodyLimit TestPlatformSecurityHeaders TestIdentityOIDCBindingKept \
	TestCatalogOptimisticConcurrency TestCatalogValidation TestCatalogServers TestCLIServers \
	TestPlatformBearerNeverOpensTheWebAPI

# Subtests of the scenarios above that need a later milestone, as a go test -skip expression: one
# entry per line below, joined with `|` (slash-separated regular expressions per level; spaces of a
# subtest name are underscores). Parent tests are never skipped by an entry with more levels than
# they have, and an entry with fewer levels than a subtest's name skips that subtest too, which is
# how the names with a slash or with parentheses are matched by a prefix of their first level
# (a subtest name containing slashes has more levels: go test reads each slash as a level
# separator). Every entry names the milestone whose work removes it:
#   milestone 7 (the operator CLI)
#   - TestPlatformMigrationsAndKeyCheck, "the old key alone: healthy, one warning;
#     rotate-secrets-key re-seals the check": runs the command rotate-secrets-key.
#   milestone 6 (the /mcp route; the token check of identity is ready but not wired before)
#   - TestPlatformStaticFilesAndSPA, "/api, /mcp and /healthz are never the shell": also asks /mcp
#     (401 and a bearer-token error); its /api half is identity's, but the subtest is one.
#   - TestPlatformSecurityHeaders, "mcp 401" and "foreign host on mcp": requests to /mcp.
#   - TestIdentityOIDCProviderDownAtStart, "the server works without the provider: health, MCP
#     with a token": a token opens /mcp.
#   - TestIdentityNoOIDC: "an sk_ token: shape, stored only as SHA-256, listed without its value,
#     works on /mcp", "a revoked token is refused; ...", "disabling a person ends their sessions,
#     tokens and links at once; ...", "control: with a trusted proxy a changing X-Forwarded-For is
#     a new client each time", "the 11th failed link attempt within a minute from one address is
#     429 with Retry-After" (its last assertion is a bad token on /mcp; TestIdentityDefaultRateLimits
#     covers the 11th attempt's 429, its Retry-After: 60 and that a valid code still works, but NOT
#     the 12th attempt's Retry-After range nor the rightmost X-Forwarded-For rule, which are judged
#     only once this subtest runs) and "the 31st failed token check within a minute from one
#     address is 429; a valid token still works": each uses a token on /mcp.
#   - TestIdentityDefaultRateLimits, "the 31st failed token check is 429 whatever X-Forwarded-For
#     says; a valid token still works": failed token checks on /mcp.
#   - TestCLICreateToken, "the token opens /mcp; a changed token does not" and "a second token for
#     the same person is another token that works too".
#   milestone 6 also (the catalog scenarios of milestone 3 reach it)
#   - TestCatalogServers, "admin routes refuse members before the body is read", its subtest
#     "GET calls" and "anonymous GET calls" (GET /api/calls, the call log; every other route is
#     judged);
#     "upstream calls": a tool call through /mcp.
#   - TestPlatformBearerNeverOpensTheWebAPI, "the same token opens /mcp (control)" and "a bad token
#     on /mcp is 401 with a challenge"; the loop over /api paths in its body runs.
#   milestone 5 (connections: PUT /api/connections/<slug>/pat)
#   - TestCLIServers, "sync of a personal-auth server uses the connection of the administrator it
#     names".
#   milestone 4 (search: the index and its command)
#   - TestCLIServers, "reindex rebuilds the search index and prints the number of indexed tools".
COMPAT_GO_SKIP_ENTRIES := \
	^TestPlatformMigrationsAndKeyCheck$$/^the_old_key_alone:_healthy,_one_warning;_rotate-secrets-key_re-seals_the_check$$ \
	^TestPlatformStaticFilesAndSPA$$/^$$/^api,_$$/^mcp_and_$$/^healthz_are_never_the_shell$$ \
	^TestPlatformSecurityHeaders$$/^mcp_401$$ \
	^TestPlatformSecurityHeaders$$/^foreign_host_on_mcp$$ \
	^TestIdentityOIDCProviderDownAtStart$$/^the_server_works_without_the_provider:_health,_MCP_with_a_token$$ \
	^TestIdentityNoOIDC$$/^an_sk__token:_shape, \
	^TestIdentityNoOIDC$$/^a_revoked_token_is_refused; \
	^TestIdentityNoOIDC$$/^disabling_a_person_ends_their_sessions,_tokens_and_links_at_once; \
	^TestIdentityNoOIDC$$/^control:_with_a_trusted_proxy_a_changing_X-Forwarded-For \
	^TestIdentityNoOIDC$$/^the_11th_failed_link_attempt_within_a_minute \
	^TestIdentityNoOIDC$$/^the_31st_failed_token_check_within_a_minute \
	^TestIdentityDefaultRateLimits$$/^the_31st_failed_token_check_is_429 \
	^TestCLICreateToken$$/^the_token_opens_ \
	^TestCLICreateToken$$/^a_second_token_for_the_same_person_is_another_token_that_works_too$$ \
	^TestCatalogServers$$/^admin_routes_refuse_members_before_the_body_is_read$$/^GET_calls$$ \
	^TestCatalogServers$$/^admin_routes_refuse_members_before_the_body_is_read$$/^anonymous_GET_calls$$ \
	^TestCatalogServers$$/^upstream_calls$$ \
	^TestPlatformBearerNeverOpensTheWebAPI$$/^the_same_token_opens_ \
	^TestPlatformBearerNeverOpensTheWebAPI$$/^a_bad_token_on_ \
	^TestCLIServers$$/^sync_of_a_personal-auth_server_uses_ \
	^TestCLIServers$$/^reindex_rebuilds_

space := $(subst ,, )
COMPAT_GO_SKIP := $(subst $(space),|,$(strip $(COMPAT_GO_SKIP_ENTRIES)))
COMPAT_GO_RUN := ^($(subst $(space),|,$(strip $(COMPAT_GO_TESTS))))$$

# The suite against the Go image. The reference image (mcp-registry:compat-ts) must exist as well:
# some scenarios compare with it on the same machine, and the new-database scenario starts from it.
# COMPAT_FIXED: the deviations (compat/harness/deviations.go) whose decided behaviour the Go server
# has: config-errors (milestone 1), audit-login-via, create-user-keeps-role and oidc-binding-kept (milestone 2),
# create-server-sync-exit (milestone 3). Left out, each until the milestone that implements it:
# token-429-retry-after and request-id-on-refusals (the /mcp route, milestone 6), oauth-state-kept
# (connections, milestone 5), reindex-wording (the reindex command, milestone 4 or 7),
# ready-before-serving (search, milestone 4). Before it runs, the pattern is
# listed (nothing is started): it must match exactly the scenarios named in COMPAT_GO_TESTS, so a
# renamed or mistyped scenario fails here instead of passing by running nothing.
compat-go:
	@docker image inspect mcp-registry:compat-ts >/dev/null 2>&1 || { echo "compat-go: the reference image mcp-registry:compat-ts is missing: run make compat-image-ts"; exit 1; }
	@docker image inspect mcp-registry:compat-go >/dev/null 2>&1 || { echo "compat-go: the image mcp-registry:compat-go is missing: run make compat-image-go"; exit 1; }
	@listed="$$(go test -tags compat -count=1 -list '$(COMPAT_GO_RUN)' ./compat/ | grep -c '^Test')"; \
	if [ "$$listed" != "$(words $(COMPAT_GO_TESTS))" ]; then \
		echo "compat-go: the pattern '$(COMPAT_GO_RUN)' matches $$listed scenario(s), COMPAT_GO_TESTS names $(words $(COMPAT_GO_TESTS)): $(COMPAT_GO_TESTS)"; exit 1; fi
	COMPAT_IMAGE=mcp-registry:compat-go COMPAT_CLI=mcp-registry COMPAT_FIXED=config-errors,audit-login-via,create-user-keeps-role,oidc-binding-kept,create-server-sync-exit \
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

# The browser flows that need only identity and the catalog, against the Go image (build it with
# compat-image-go; the reference image must exist too, as for compat-go). The flows that wait for a
# later milestone are not selected, each named here with it:
#   03-tokens (the token opens /mcp, 6), 09-permissions (a call through /mcp and the call log, 6),
#   08-connect and 08b-connect-cross-site (OAuth connect, 5), 12-direct-links (the /connections page
#   asks GET /api/connections, 5).
# TestControlOperationsAgainstARealStack is not run for the same reason (it uses /mcp, 6); the control
# API's other tests are image independent and run in e2e-flows. Same guard, same specs, only the
# selection differs: the arguments go through E2E_PLAYWRIGHT_ARGS, which refuses the ways to run
# fewer tests silently. An E2E_PLAYWRIGHT_ARGS given on the make command line replaces the list.
E2E_GO_SPECS := 00-guard-selftest.spec.ts 01-first-start-link.spec.ts 02-oidc-login.spec.ts \
	04-create-server.spec.ts 05-form-errors.spec.ts 06-hide-tool.spec.ts 07-edit-conflict.spec.ts \
	10-disable-person.spec.ts 11-session-expiry.spec.ts
e2e-flows-go:
	@docker image inspect mcp-registry:compat-go >/dev/null 2>&1 || { echo "e2e-flows-go: the image mcp-registry:compat-go is missing: run make compat-image-go"; exit 1; }
	COMPAT_IMAGE=mcp-registry:compat-go COMPAT_CLI=mcp-registry COMPAT_FIXED=config-errors,audit-login-via,create-user-keeps-role,oidc-binding-kept,create-server-sync-exit \
		E2E_PLAYWRIGHT_ARGS='$(E2E_GO_SPECS)' \
		go test -tags "compat e2e" -count=1 -timeout 30m -v -run '^TestBrowserFlows$$' ./compat/e2e/

# Proves the suite can fail: runs named scenarios against the reference image behind a distorting
# proxy (compat/selfcheck). Slow; CI runs it daily (compat-selfcheck.yml), not on pull requests.
compat-selfcheck: compat-image-ts
	go test -tags "compat selfcheck" -count=1 -timeout 90m -v ./compat/selfcheck/

go-build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mcp-registry ./cmd/mcp-registry

# --- The local embedder (build tag onnx) ---------------------------------------------------------
#
# The package internal/modules/search/infra/onnx compiles its cgo files only with `-tags onnx`;
# `make go-verify` never needs native libraries. Local builds and tests of the tagged code use the
# libraries below, downloaded into a git-ignored cache (.cache/native/<platform>/) with every
# SHA-256 verified and nothing downloaded ever executed (native/fetch.sh).
#
# Pins (provenance, measured in the embedder spike of milestone 4):
#   ONNX Runtime 1.30.0, CPU: the libraries inside the npm package onnxruntime-node@1.30.0, the very
#     files the TypeScript reference image loads. github.com/microsoft/onnxruntime returned 404
#     when this was pinned. The exact 1.30.0 build is a parity condition: another version produced
#     vectors that differ by up to 0.0166 on amd64.
#   libtokenizers 1.27.0: the static library of github.com/daulet/tokenizers, release assets.
NATIVE_ROOT := .cache/native
NATIVE_PLATFORM := $(shell go env GOOS)-$(shell go env GOARCH)
NATIVE_DIR := $(NATIVE_ROOT)/$(NATIVE_PLATFORM)
NATIVE_ABS := $(abspath $(NATIVE_DIR))

ORT_NPM_URL := https://registry.npmjs.org/onnxruntime-node/-/onnxruntime-node-1.30.0.tgz
ORT_NPM_SHA256 := 6e3390d6b783e7be946fad629292799da28d0b42f84856e50d2c1b0383291e75
TOK_BASE_URL := https://github.com/daulet/tokenizers/releases/download/v1.27.0

ORT_MEMBER_darwin-arm64 := package/bin/napi-v6/darwin/arm64/libonnxruntime.1.30.0.dylib
ORT_FILE_darwin-arm64 := libonnxruntime.1.30.0.dylib
ORT_SHA_darwin-arm64 := 685d2be5dba1309c89d3a5324b7fd06a5c42f1a61bfea32d14c4cc28d072121d
TOK_SHA_TGZ_darwin-arm64 := fb84b8b2e349a5952767ffe80ccd862fc44084de47f3b0cc3f0b7c9d4e649cf7
TOK_SHA_darwin-arm64 := c91ae814afb8fe4f000099972208f60c3aa2d13899a7cd5a31fee2e9e2efbac7

ORT_MEMBER_linux-arm64 := package/bin/napi-v6/linux/arm64/libonnxruntime.so.1
ORT_FILE_linux-arm64 := libonnxruntime.so.1
ORT_SHA_linux-arm64 := 1f549d46250b005580b597f4164984aaf75b3bcb9f3aeb07c7f8a8fe60b23c76
TOK_SHA_TGZ_linux-arm64 := e96545ad05930c26f51f63d932ee6d3bbd32bbed149e102c5290d587a2293067
TOK_SHA_linux-arm64 := aa4d3a7d28023e439fa7a645fb606195acafb5c33c8cd01928ed1aacfc4c074e

ORT_MEMBER_linux-amd64 := package/bin/napi-v6/linux/x64/libonnxruntime.so.1
ORT_FILE_linux-amd64 := libonnxruntime.so.1
ORT_SHA_linux-amd64 := ffb75a925ba05e47b235bb66e3e3911714a80b328a9c9425539feb204aa32a23
TOK_SHA_TGZ_linux-amd64 := 72556cdca798dd4ea7cdaba308e5f0d68a8cb93b67c96edf485b7a0edd7b07f4
TOK_SHA_linux-amd64 := e6862b31745bb7d07980fcee70e49cd3b4318097609180f5d2d3fb394f305d50

ONNX_PKG := ./internal/modules/search/infra/onnx/...
# Variables the tagged tests read (names are for local development only, not operator settings).
ONNX_TEST_ENV = CGO_ENABLED=1 CGO_LDFLAGS="-L$(NATIVE_ABS)" ONNX_TEST_LIB="$(NATIVE_ABS)/$(ORT_FILE_$(NATIVE_PLATFORM))" ONNX_TEST_MODEL_DIR="$(abspath $(NATIVE_ROOT))/models"

native-libs:
	@test -n "$(ORT_FILE_$(NATIVE_PLATFORM))" || { echo "native-libs: no pinned libraries for $(NATIVE_PLATFORM)"; exit 1; }
	ORT_URL="$(ORT_NPM_URL)" ORT_TGZ_SHA256="$(ORT_NPM_SHA256)" ORT_MEMBER="$(ORT_MEMBER_$(NATIVE_PLATFORM))" \
	ORT_FILE="$(ORT_FILE_$(NATIVE_PLATFORM))" ORT_FILE_SHA256="$(ORT_SHA_$(NATIVE_PLATFORM))" \
	TOK_URL="$(TOK_BASE_URL)/libtokenizers.$(NATIVE_PLATFORM).tar.gz" TOK_TGZ_SHA256="$(TOK_SHA_TGZ_$(NATIVE_PLATFORM))" \
	TOK_FILE_SHA256="$(TOK_SHA_$(NATIVE_PLATFORM))" \
	sh internal/modules/search/infra/onnx/native/fetch.sh $(NATIVE_PLATFORM) $(NATIVE_DIR)

# The model and tokenizer the TypeScript server uses (revision and SHA-256 pinned in the script),
# in the layout <root>/Xenova/multilingual-e5-small/. Needs node, like VERSION above.
native-model:
	node scripts/download-model.mjs $(NATIVE_ROOT)/models

go-vet-onnx: native-libs
	$(ONNX_TEST_ENV) go vet -tags onnx $(ONNX_PKG)

go-lint-onnx: native-libs
	$(ONNX_TEST_ENV) go run $(GOLANGCI_LINT) run --build-tags onnx $(ONNX_PKG)

go-test-onnx: native-libs native-model
	$(ONNX_TEST_ENV) go test -tags onnx -count=1 $(ONNX_PKG)

go-verify-onnx: go-vet-onnx go-lint-onnx go-test-onnx
