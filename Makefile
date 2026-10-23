PROJECT     := icloud-mcp
VERSION     ?= dev
DIST_DIR    := dist
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
GO          ?= go
EVIDENCE_DIR ?=

# Use a fixed digest for the release builder image to make builds reproducible.
# The module minimum is Go 1.26.0; this digest tracks golang:1.26.10.
# To update the digest, use: docker pull golang:1.26.10 &&
# docker image inspect golang:1.26.10 --format '{{index .RepoDigests 0}}'.
GOLANG_IMAGE ?= golang:1.26.10@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82

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

.PHONY: build check-release-version check-release-clean release release-all install test protocol-evidence lint vet cover clean help

build: ## Build a local binary with the host toolchain. Include VERSION (default dev).
	@mkdir -p $(BIN_DIR)
	$(GO) build $(BUILDFLAGS) -o $(BIN_DIR)/$(PROJECT) ./cmd/$(PROJECT)

# release builds linux/arm64 in the fixed Go container.
# It does not need a host Go toolchain.
# release-all uses the host toolchain to build each TARGETS pair.
# These pure-Go builds use CGO_ENABLED=0 and do not need Docker.
check-release-version:
	@if ! printf '%s\n' "$(VERSION)" | LC_ALL=C grep -Eq \
		'^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.]+)?$$'; then \
		echo "VERSION must be a release version (for example, v0.4.2)" >&2; \
		exit 1; \
	fi

check-release-clean:
	@git diff --quiet && git diff --cached --quiet && \
		test -z "$$(git ls-files --others --exclude-standard)" || { \
			echo "release requires a clean Git worktree" >&2; exit 1; \
		}

release: check-release-version check-release-clean ## Static linux/arm64 archive with a fixed Go 1.26.10 container.
	@set -eu; \
		mkdir -p "$(DIST_DIR)"; \
		dist="$$(cd "$(DIST_DIR)" && pwd -P)"; \
		archive="$(PROJECT)-$(VERSION)-linux-arm64.tar.gz"; \
		checksums="$(PROJECT)-$(VERSION)-checksums.txt"; \
		for output in "$$archive" "$$checksums"; do \
			if [ -e "$$dist/$$output" ] || [ -L "$$dist/$$output" ]; then \
				echo "release output already exists: $$dist/$$output" >&2; \
				exit 1; \
			fi; \
		done; \
		stage="$$(mktemp -d "$$dist/.release-$(VERSION).XXXXXX")"; \
		cleanup_stage() { \
			status=$$?; \
			if [ "$$status" -eq 0 ]; then \
				if ! find "$$stage" -depth -delete; then \
					status=1; \
					echo "Release staging cleanup failed: $$stage" >&2; \
				fi; \
			else \
				echo "Release staging retained: $$stage" >&2; \
			fi; \
			exit "$$status"; \
		}; \
		trap cleanup_stage 0; \
		mkdir -p "$$stage/package"; \
		docker run --rm -v "$(CURDIR):/src" -v "$$stage:/out" -w /src \
			-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
			$(GOLANG_IMAGE) \
			go build -trimpath -ldflags='$(LDFLAGS)' -o /out/$(PROJECT) ./cmd/$(PROJECT); \
		cp "$$stage/$(PROJECT)" "$$stage/package/$(PROJECT)"; \
		cp $(RELEASE_FILES) "$$stage/package/"; \
		tar -czf "$$stage/$$archive" -C "$$stage/package" $(PROJECT) $(RELEASE_FILES); \
		(cd "$$stage" && shasum -a 256 "$$archive" >"$$checksums"); \
		ln "$$stage/$$archive" "$$dist/$$archive"; \
		ln "$$stage/$$checksums" "$$dist/$$checksums"

release-all: check-release-version check-release-clean ## Cross-compile and package all TARGETS with the host toolchain.
	@set -eu; \
		mkdir -p "$(DIST_DIR)"; \
		dist="$$(cd "$(DIST_DIR)" && pwd -P)"; \
		checksums="$(PROJECT)-$(VERSION)-checksums.txt"; \
		archives=; \
		for target in $(TARGETS); do \
			os=$${target%/*}; arch=$${target#*/}; \
			archives="$$archives $(PROJECT)-$(VERSION)-$$os-$$arch.tar.gz"; \
		done; \
		outputs="$$archives $$checksums"; \
		for output in $$outputs; do \
			if [ -e "$$dist/$$output" ] || [ -L "$$dist/$$output" ]; then \
				echo "release output already exists: $$dist/$$output" >&2; \
				exit 1; \
			fi; \
		done; \
		stage="$$(mktemp -d "$$dist/.release-$(VERSION).XXXXXX")"; \
		cleanup_stage() { \
			status=$$?; \
			if [ "$$status" -eq 0 ]; then \
				if ! find "$$stage" -depth -delete; then \
					status=1; \
					echo "Release staging cleanup failed: $$stage" >&2; \
				fi; \
			else \
				echo "Release staging retained: $$stage" >&2; \
			fi; \
			exit "$$status"; \
		}; \
		trap cleanup_stage 0; \
		for target in $(TARGETS); do \
			os=$${target%/*}; arch=$${target#*/}; \
			binary="$$stage/$(PROJECT)-$$os-$$arch"; \
			package="$$stage/package-$$os-$$arch"; \
			archive="$(PROJECT)-$(VERSION)-$$os-$$arch.tar.gz"; \
			echo "Building $$target"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(BUILDFLAGS) \
				-o "$$binary" ./cmd/$(PROJECT); \
			mkdir -p "$$package"; \
			cp "$$binary" "$$package/$(PROJECT)"; \
			cp $(RELEASE_FILES) "$$package/"; \
			tar -czf "$$stage/$$archive" -C "$$package" $(PROJECT) $(RELEASE_FILES); \
		done; \
		(cd "$$stage" && shasum -a 256 \
			$(PROJECT)-$(VERSION)-linux-amd64.tar.gz \
			$(PROJECT)-$(VERSION)-linux-arm64.tar.gz \
			$(PROJECT)-$(VERSION)-darwin-arm64.tar.gz \
			>"$$checksums"); \
		for output in $$archives; do \
			ln "$$stage/$$output" "$$dist/$$output"; \
		done; \
		ln "$$stage/$$checksums" "$$dist/$$checksums"; \
		ls -la "$$dist"

install: build ## Build a host-compatible binary and copy it to INSTALL_DIR.
	mkdir -p $(INSTALL_DIR)
	cp $(BIN_DIR)/$(PROJECT) $(INSTALL_DIR)/$(PROJECT)
	@echo "Installed: $(INSTALL_DIR)/$(PROJECT)"

test: ## Run Go tests with race detection and coverage.
	$(GO) test ./... -race -cover

protocol-evidence: ## Run synthetic protocol scenarios. Set EVIDENCE_DIR to a new external directory.
	@test -n "$(EVIDENCE_DIR)" || { echo "Set EVIDENCE_DIR to a new directory outside the repository" >&2; exit 1; }
	python3 scripts/protocol_evidence.py --output "$(EVIDENCE_DIR)"

cover: ## Run Go tests and create a coverage report for HTML display.
	@mkdir -p $(DIST_DIR)
	$(GO) test ./... -race -coverprofile=$(DIST_DIR)/coverage.out
	$(GO) tool cover -func=$(DIST_DIR)/coverage.out | tail -1
	@echo "HTML report: $(GO) tool cover -html=$(DIST_DIR)/coverage.out"

vet: ## Run go vet.
	$(GO) vet ./...

# Use a fixed golangci-lint version so local make lint and CI use the same version.
GOLANGCI_LINT_VERSION := v2.15.0

lint: vet ## Run go vet and a fixed golangci-lint version with go run.
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./...

clean: ## Move build artifacts to a recoverable backup under /tmp.
	@set -eu; \
		repository="$$(pwd -P)"; \
		user_home="$$(cd "$(HOME)" && pwd -P)"; \
		for path in "$(BIN_DIR)" "$(DIST_DIR)"; do \
			case "$$path" in ''|.|..|/) \
				echo "refusing unsafe build artifact path: $$path" >&2; \
				exit 1; \
			;; esac; \
			if [ -e "$$path" ] || [ -L "$$path" ]; then \
				if [ -d "$$path" ] && [ ! -L "$$path" ]; then \
					absolute="$$(cd "$$path" && pwd -P)"; \
				else \
					parent="$$(dirname "$$path")"; \
					name="$$(basename "$$path")"; \
					absolute="$$(cd "$$parent" && pwd -P)/$$name"; \
				fi; \
				case "$$repository/" in "$$absolute/"*) \
					echo "refusing build artifact path that contains the repository: $$absolute" >&2; \
					exit 1; \
				;; esac; \
				if [ "$$absolute" = "$$user_home" ]; then \
					echo "refusing to move the user home directory: $$absolute" >&2; \
					exit 1; \
				fi; \
			fi; \
		done; \
		backup=; moved=0; \
		report_backup() { \
			status=$$?; \
			if [ -n "$$backup" ]; then \
				echo "Build artifacts backup: $$backup"; \
			fi; \
			exit "$$status"; \
		}; \
		trap report_backup 0; \
		for item in "bin|$(BIN_DIR)" "dist|$(DIST_DIR)"; do \
			label=$${item%%|*}; path=$${item#*|}; \
			if [ ! -e "$$path" ] && [ ! -L "$$path" ]; then \
				continue; \
			fi; \
			if [ -z "$$backup" ]; then \
				backup="$$(mktemp -d /tmp/icloud-mcp-clean.XXXXXX)"; \
			fi; \
			mv "$$path" "$$backup/$$label"; \
			moved=1; \
		done; \
		if [ "$$moved" -eq 0 ]; then \
			echo "No build artifacts to back up."; \
		fi

help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'
