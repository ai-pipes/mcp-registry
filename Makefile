GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GO_ARCH_LINT := github.com/fe3dback/go-arch-lint@v1.14.0
GOFUMPT := mvdan.cc/gofumpt@v0.12.0
VERSION := $(shell node -p "require('./apps/server/package.json').version")

.PHONY: go-lint go-lint-new go-fmt-check go-arch-lint go-verify go-build print-go-arch-lint print-golangci-lint

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

go-verify: go-lint go-arch-lint
	go vet ./...
	go test ./...

go-build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mcp-registry ./cmd/mcp-registry
