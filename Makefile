PROJECT     := icloud-mcp
VERSION     ?= dev
DIST_DIR    := dist
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
GO          ?= go

# Pin the release builder image by digest so make release is reproducible.
# The module minimum is Go 1.25.12; this digest tracks golang:1.25.12.
# Bump via: docker pull golang:1.25.12 &&
# docker image inspect golang:1.25.12 --format '{{index .RepoDigests 0}}'.
GOLANG_IMAGE ?= golang:1.25.12@sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980

# Release targets: linux/amd64, linux/arm64, darwin/arm64. All static
# (CGO_ENABLED=0), trimmed, stripped. The binaries embed the version via
# -X main.version=$(VERSION) (override with: make build VERSION=v0.3.0).
LDFLAGS  := -s -w -X main.version=$(VERSION)
BUILDFLAGS := -trimpath -ldflags='$(LDFLAGS)'
TARGETS  := linux/amd64 linux/arm64 darwin/arm64
RELEASE_FILES := LICENSE THIRD_PARTY_NOTICES.md

.PHONY: build check-release-version check-release-clean release release-all install test lint vet cover clean help

build: ## Local binary (dev), host toolchain; embeds VERSION (default dev).
	@mkdir -p $(BIN_DIR)
	$(GO) build $(BUILDFLAGS) -o $(BIN_DIR)/$(PROJECT) ./cmd/$(PROJECT)

# release builds a single target (default linux/arm64, the original pinned
# platform) INSIDE a pinned golang:1.25 container so no host Go toolchain is
# assumed for the deliverable. release-all uses the host toolchain to
# cross-compile every TARGETS pair without Docker (CGO=0 pure-Go cross-compile).
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

release: check-release-version check-release-clean ## Static linux/arm64 archive via pinned Go 1.25.12 container.
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

test: ## Unit tests (race + coverage).
	$(GO) test ./... -race -cover

cover: ## Unit tests with coverage report + HTML.
	@mkdir -p $(DIST_DIR)
	$(GO) test ./... -race -coverprofile=$(DIST_DIR)/coverage.out
	$(GO) tool cover -func=$(DIST_DIR)/coverage.out | tail -1
	@echo "HTML report: $(GO) tool cover -html=$(DIST_DIR)/coverage.out"

vet: ## go vet.
	$(GO) vet ./...

# Pin golangci-lint so local make lint and CI do not drift on @latest.
GOLANGCI_LINT_VERSION := v2.1.6

lint: vet ## go vet + pinned golangci-lint via go run.
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./...

clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) $(DIST_DIR)

help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'
