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

.PHONY: netd
netd: ## Build the privileged network helper daemon into bin/macvz-netd
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/macvz-netd ./cmd/macvz-netd

.PHONY: cri
cri: ## Build the experimental CRI feasibility server into bin/macvz-cri
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/macvz-cri ./cmd/macvz-cri

.PHONY: cri-k3s
cri-k3s: cri ## Run the CRI-P8 k3s compatibility suite (set MACVZ_INTEGRATION=1 to run live)
	./test/e2e/cri-k3s/run.sh

.PHONY: cri-soak
cri-soak: cri ## Run the CRI-P8 bounded soak (set MACVZ_INTEGRATION=1 to run live)
	./test/e2e/cri-k3s/soak.sh

.PHONY: cri-k3s-inloop
cri-k3s-inloop: ## Run the CRI-P9 real kubelet/k3s in-loop suite (set MACVZ_INTEGRATION=1 + KUBECONFIG to run live)
	./test/e2e/cri-k3s/k3s-inloop.sh

.PHONY: cri-linuxpod-poc
cri-linuxpod-poc: ## Run the #88 LinuxPod shared-namespace PoC (set MACVZ_LINUXPOD_POC=1 to run live)
	./test/e2e/cri-linuxpod/run.sh

.PHONY: cri-linuxpod-c2
cri-linuxpod-c2: ## Run the #89 LinuxPod post-create addContainer probe (set MACVZ_LINUXPOD_POC=1 to run live)
	MACVZ_LINUXPOD_PROBE=c2 ./test/e2e/cri-linuxpod/run.sh

.PHONY: cri-linuxpod-c4
cri-linuxpod-c4: ## Run the #91 LinuxPod HotplugProvider boundary probe (set MACVZ_LINUXPOD_POC=1 to run live)
	MACVZ_LINUXPOD_PROBE=c4 ./test/e2e/cri-linuxpod/run.sh

.PHONY: cri-linuxpod-r1
cri-linuxpod-r1: ## Run the #93 guest-side hotplug device discovery probe (set MACVZ_LINUXPOD_POC=1 to run live)
	MACVZ_LINUXPOD_PROBE=r1 ./test/e2e/cri-linuxpod/run.sh

.PHONY: bench
bench: ## Build the density/RAM benchmark harness into bin/mvz-bench
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/mvz-bench ./cmd/mvz-bench

.PHONY: mesh
mesh: ## Build the mesh key/config helper into bin/macvz-mesh
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/macvz-mesh ./cmd/macvz-mesh

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

.PHONY: compat
compat: ## Run the P6 workload compatibility fixture against a real cluster (issue #53)
	./test/e2e/p6-compat/run.sh

.PHONY: soak
soak: ## Run the long-duration P9 soak harness against a real cluster (issue #71)
	./test/e2e/soak/run.sh

.PHONY: cri-feasibility
cri-feasibility: ## Probe apple/container surfaces for the CRI feasibility track
	./scripts/cri-feasibility.sh

.PHONY: release
release: ## Build, sign, and package a darwin/arm64 release + install bundle into dist/ (see docs/RELEASE.md, docs/PACKAGING.md)
	VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) ./scripts/macos-release.sh

.PHONY: install-rehearsal
install-rehearsal: ## Rehearse install→upgrade→rollback→uninstall in a temp prefix (no root; issue #70)
	./scripts/macvz-install-rehearsal.sh

.PHONY: sign
sign: build ## Ad-hoc codesign bin/$(BINARY) for local development
	codesign --force --sign - $(BIN_DIR)/$(BINARY)
	codesign --verify --strict --verbose=2 $(BIN_DIR)/$(BINARY)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) dist

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
