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

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
