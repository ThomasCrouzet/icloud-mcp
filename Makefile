PROJECT     := icloud-mcp
VERSION     ?= dev
DIST_DIR    := dist
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
GO          ?= go

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build release install test lint vet clean

build: ## Local binary (dev), host toolchain.
	$(GO) build -o $(BIN_DIR)/$(PROJECT) ./cmd/$(PROJECT)

release: ## Static linux/arm64 binary via a golang:1.25 container (no host toolchain required).
	docker run --rm -v $(PWD):/src -w /src \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
		golang:1.25 \
		go build -trimpath -ldflags='$(LDFLAGS)' -o $(DIST_DIR)/$(PROJECT) ./cmd/$(PROJECT)

install: release ## Build + copy to INSTALL_DIR (overridable, default $(HOME)/.local/bin).
	mkdir -p $(INSTALL_DIR)
	cp $(DIST_DIR)/$(PROJECT) $(INSTALL_DIR)/$(PROJECT)
	@echo "Installed: $(INSTALL_DIR)/$(PROJECT)"

test: ## Unit tests (race + coverage).
	$(GO) test ./... -race -cover

vet: ## go vet.
	$(GO) vet ./...

lint: vet ## go vet + golangci-lint (if installed).
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, skipped"

clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) $(DIST_DIR)
