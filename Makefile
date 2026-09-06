PROJECT     := icloud-mcp
VERSION     ?= dev
DIST_DIR    := dist
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
GO          ?= go

# Use a fixed digest for the release builder image to make builds reproducible.
# The module minimum is Go 1.25.13; this digest tracks golang:1.25.13.
# To update the digest, use: docker pull golang:1.25.13 &&
# docker image inspect golang:1.25.13 --format '{{index .RepoDigests 0}}'.
GOLANG_IMAGE ?= golang:1.25.13@sha256:cbff9d1a9041b316010f2da6b701b6c0d597718cb90928c85eb597334a0d23d4

# Release archives: linux/amd64, linux/arm64, darwin/arm64.
# All binaries are static (CGO_ENABLED=0), with build paths and debug data removed.
# CI also builds windows/amd64 to check compatibility, but does not package it.
# Binaries include the version through -X main.version=$(VERSION).
# To set the version, use: make build VERSION=v0.3.0.
# GitHub tag releases use release-all after CI passes.
# Local make release uses a Docker image with a fixed digest.
LDFLAGS  := -s -w -X main.version=$(VERSION)
BUILDFLAGS := -trimpath -ldflags='$(LDFLAGS)'
TARGETS  := linux/amd64 linux/arm64 darwin/arm64
RELEASE_FILES := LICENSE THIRD_PARTY_NOTICES.md

.PHONY: build check-release-version check-release-clean release release-all install test lint vet cover clean help

build: ## Build a local binary with the host toolchain. Include VERSION (default dev).
	@mkdir -p $(BIN_DIR)
	$(GO) build $(BUILDFLAGS) -o $(BIN_DIR)/$(PROJECT) ./cmd/$(PROJECT)

# release builds linux/arm64 in the fixed Go container.
# It does not need a host Go toolchain.
# release-all uses the host toolchain to build each TARGETS pair.
# These pure-Go builds use CGO_ENABLED=0 and do not need Docker.
check-release-version:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "VERSION must be set to a non-dev release version (for example, v0.3.0)" >&2; \
		exit 1; \
	fi

check-release-clean:
	@git diff --quiet && git diff --cached --quiet && \
		test -z "$$(git ls-files --others --exclude-standard)" || { \
			echo "release requires a clean Git worktree" >&2; exit 1; \
		}

release: check-release-version check-release-clean ## Static linux/arm64 archive with a fixed Go 1.25.13 container.
	rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR)
	docker run --rm -v $(PWD):/src -w /src \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
		$(GOLANG_IMAGE) \
		go build -trimpath -ldflags='$(LDFLAGS)' -o $(DIST_DIR)/$(PROJECT) ./cmd/$(PROJECT)
	@stage="$(DIST_DIR)/.package-linux-arm64"; \
		rm -rf "$$stage"; mkdir -p "$$stage"; \
		cp "$(DIST_DIR)/$(PROJECT)" "$$stage/$(PROJECT)"; \
		cp $(RELEASE_FILES) "$$stage/"; \
		tar -czf "$(DIST_DIR)/$(PROJECT)-$(VERSION)-linux-arm64.tar.gz" \
			-C "$$stage" $(PROJECT) $(RELEASE_FILES); \
		rm -rf "$$stage"
	@cd $(DIST_DIR) && shasum -a 256 \
		"$(PROJECT)-$(VERSION)-linux-arm64.tar.gz" \
		> "$(PROJECT)-$(VERSION)-checksums.txt"

release-all: check-release-version check-release-clean ## Cross-compile and package all TARGETS with the host toolchain.
	rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR)
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "Building $$t -> $(DIST_DIR)/$(PROJECT)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(BUILDFLAGS) \
			-o $(DIST_DIR)/$(PROJECT)-$$os-$$arch ./cmd/$(PROJECT) || exit 1; \
	done
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; stage="$(DIST_DIR)/.package-$$os-$$arch"; \
		rm -rf "$$stage"; mkdir -p "$$stage"; \
		cp "$(DIST_DIR)/$(PROJECT)-$$os-$$arch" "$$stage/$(PROJECT)"; \
		cp $(RELEASE_FILES) "$$stage/"; \
		tar -czf "$(DIST_DIR)/$(PROJECT)-$(VERSION)-$$os-$$arch.tar.gz" \
			-C "$$stage" $(PROJECT) $(RELEASE_FILES) || exit 1; \
		rm -rf "$$stage"; \
	done
	@cd $(DIST_DIR) && shasum -a 256 \
		$(PROJECT)-$(VERSION)-linux-amd64.tar.gz \
		$(PROJECT)-$(VERSION)-linux-arm64.tar.gz \
		$(PROJECT)-$(VERSION)-darwin-arm64.tar.gz \
		> "$(PROJECT)-$(VERSION)-checksums.txt"
	@ls -la $(DIST_DIR)

install: build ## Build a host-compatible binary and copy it to INSTALL_DIR.
	mkdir -p $(INSTALL_DIR)
	cp $(BIN_DIR)/$(PROJECT) $(INSTALL_DIR)/$(PROJECT)
	@echo "Installed: $(INSTALL_DIR)/$(PROJECT)"

test: ## Run unit tests with race detection and coverage.
	$(GO) test ./... -race -cover

cover: ## Run unit tests and create a coverage report for HTML display.
	@mkdir -p $(DIST_DIR)
	$(GO) test ./... -race -coverprofile=$(DIST_DIR)/coverage.out
	$(GO) tool cover -func=$(DIST_DIR)/coverage.out | tail -1
	@echo "HTML report: $(GO) tool cover -html=$(DIST_DIR)/coverage.out"

vet: ## Run go vet.
	$(GO) vet ./...

# Use a fixed golangci-lint version so local make lint and CI use the same version.
GOLANGCI_LINT_VERSION := v2.13.2

lint: vet ## Run go vet and a fixed golangci-lint version with go run.
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./...

clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) $(DIST_DIR)

help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'
