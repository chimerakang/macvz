# macvz-kubelet build tooling

BINARY      := macvz-kubelet
PKG         := github.com/chimerakang/macvz
CMD         := ./cmd/macvz-kubelet
BIN_DIR     := bin

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

VPKG        := $(PKG)/internal/version
LDFLAGS     := -s -w \
	-X $(VPKG).Version=$(VERSION) \
	-X $(VPKG).Commit=$(COMMIT) \
	-X $(VPKG).Date=$(DATE)

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build the binary into bin/ with version stamping
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD)

.PHONY: bench
bench: ## Build the density/RAM benchmark harness into bin/mvz-bench
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/mvz-bench ./cmd/mvz-bench

.PHONY: test
test: ## Run unit tests
	go test ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (must be installed)
	golangci-lint run

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -s -w .

.PHONY: tidy
tidy: ## Tidy module dependencies
	go mod tidy

.PHONY: e2e
e2e: ## Run the multi-node end-to-end suite against a real cluster (see docs/E2E.md)
	MACVZ_E2E=1 go test -tags e2e -count=1 -v -timeout 30m ./test/e2e/

.PHONY: release
release: ## Build, sign, and package a darwin/arm64 release into dist/ (see docs/RELEASE.md)
	VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) ./scripts/macos-release.sh

.PHONY: sign
sign: build ## Ad-hoc codesign bin/$(BINARY) for local development
	codesign --force --sign - $(BIN_DIR)/$(BINARY)
	codesign --verify --strict --verbose=2 $(BIN_DIR)/$(BINARY)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) dist

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
