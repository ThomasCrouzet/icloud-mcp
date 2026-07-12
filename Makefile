PROJECT     := icloud-mcp
VERSION     ?= dev
DIST_DIR    := dist
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
GO          ?= go

# Release targets: linux/amd64, linux/arm64, darwin/arm64. All static
# (CGO_ENABLED=0), trimmed, stripped. The binaries embed the version via
# -X main.version=$(VERSION) (override with: make release VERSION=v0.2.0).
LDFLAGS  := -s -w -X main.version=$(VERSION)
BUILDFLAGS := -trimpath -ldflags='$(LDFLAGS)'
TARGETS  := linux/amd64 linux/arm64 darwin/arm64

.PHONY: build release release-all install test lint vet cover clean help

build: ## Local binary (dev), host toolchain.
	$(GO) build -o $(BIN_DIR)/$(PROJECT) ./cmd/$(PROJECT)

# release builds a single target (default linux/arm64, the original pinned
# platform) INSIDE a golang:1.25 container so no host Go toolchain is assumed
# for the deliverable. release-all uses the host toolchain to cross-compile
# every TARGETS pair without Docker (CGO=0 pure-Go cross-compile).
release: ## Static linux/arm64 binary via a golang:1.25 container (no host toolchain required).
	docker run --rm -v $(PWD):/src -w /src \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
		golang:1.25 \
		go build -trimpath -ldflags='$(LDFLAGS)' -o $(DIST_DIR)/$(PROJECT) ./cmd/$(PROJECT)

release-all: ## Cross-compile all TARGETS (linux/amd64, linux/arm64, darwin/arm64) with the host toolchain.
	@mkdir -p $(DIST_DIR)
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "Building $$t -> $(DIST_DIR)/$(PROJECT)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(BUILDFLAGS) \
			-o $(DIST_DIR)/$(PROJECT)-$$os-$$arch ./cmd/$(PROJECT) || exit 1; \
	done
	@ls -la $(DIST_DIR)

install: release ## Build + copy to INSTALL_DIR (overridable, default $(HOME)/.local/bin).
	mkdir -p $(INSTALL_DIR)
	cp $(DIST_DIR)/$(PROJECT) $(INSTALL_DIR)/$(PROJECT)
	@echo "Installed: $(INSTALL_DIR)/$(PROJECT)"

test: ## Unit tests (race + coverage).
	$(GO) test ./... -race -cover

cover: ## Unit tests with coverage report + HTML.
	$(GO) test ./... -race -coverprofile=$(DIST_DIR)/coverage.out
	$(GO) tool cover -func=$(DIST_DIR)/coverage.out | tail -1
	@echo "HTML report: $(GO) tool cover -html=$(DIST_DIR)/coverage.out"

vet: ## go vet.
	$(GO) vet ./...

lint: vet ## go vet + golangci-lint (if installed).
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run --timeout=5m || echo "golangci-lint not installed, skipped"

clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) $(DIST_DIR)

help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'
