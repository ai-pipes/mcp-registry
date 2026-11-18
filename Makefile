GOLANGCI_LINT := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GOFUMPT := mvdan.cc/gofumpt@v0.12.0
VERSION := $(shell node -p "require('./apps/server/package.json').version")

.PHONY: go-lint go-lint-new go-fmt-check go-verify go-build

go-lint:
	go run $(GOLANGCI_LINT) run

go-lint-new:
	go run $(GOLANGCI_LINT) run --new-from-rev=HEAD

go-fmt-check:
	@out="$$(go run $(GOFUMPT) -l .)"; \
	if [ -n "$$out" ]; then echo "gofumpt: needs formatting:"; echo "$$out"; exit 1; fi

go-verify: go-lint
	go vet ./...
	go test ./...

go-build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mcp-registry ./cmd/mcp-registry
