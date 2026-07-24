PROJECT     := icloud-mcp
VERSION     ?= dev
DIST_DIR    := dist
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
GO          ?= go

# Pin the release builder image by digest so make release is reproducible
# (floating golang:1.25 tags move). Bump via: docker pull golang:1.25 &&
# docker image inspect golang:1.25 --format '{{index .RepoDigests 0}}'.
GOLANG_IMAGE ?= golang:1.25@sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980

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
# platform) INSIDE a pinned golang:1.25 container so no host Go toolchain is
# assumed for the deliverable. release-all uses the host toolchain to
# cross-compile every TARGETS pair without Docker (CGO=0 pure-Go cross-compile).
release: ## Static linux/arm64 binary via pinned golang:1.25 container (no host toolchain required).
	docker run --rm -v $(PWD):/src -w /src \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
		$(GOLANG_IMAGE) \
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

# Pin golangci-lint so local make lint and CI do not drift on @latest.
GOLANGCI_LINT_VERSION ?= v2.1.6

lint: vet ## go vet + golangci-lint (pinned version via go run when not on PATH).
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m ./...; \
	else \
		echo "golangci-lint not on PATH; running pinned $(GOLANGCI_LINT_VERSION) via go run"; \
		$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./...; \
	fi

clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) $(DIST_DIR)

help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'
