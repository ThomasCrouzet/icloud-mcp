PROJECT     := icloud-mcp
VERSION     ?= dev
DIST_DIR    := dist
BIN_DIR     := bin
INSTALL_DIR ?= $(HOME)/.local/bin
GO          ?= go

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build release install test lint vet clean

build: ## Binaire local (dev), toolchain hôte.
	$(GO) build -o $(BIN_DIR)/$(PROJECT) ./cmd/$(PROJECT)

release: ## Binaire statique linux/arm64 via conteneur golang:1.25 (pas de dépendance à une toolchain hôte).
	docker run --rm -v $(PWD):/src -w /src \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
		golang:1.25 \
		go build -trimpath -ldflags='$(LDFLAGS)' -o $(DIST_DIR)/$(PROJECT) ./cmd/$(PROJECT)

install: release ## Build + copie vers INSTALL_DIR (surchargeable, défaut $(HOME)/.local/bin).
	mkdir -p $(INSTALL_DIR)
	cp $(DIST_DIR)/$(PROJECT) $(INSTALL_DIR)/$(PROJECT)
	@echo "Installé : $(INSTALL_DIR)/$(PROJECT)"

test: ## Tests unitaires (race + couverture).
	$(GO) test ./... -race -cover

vet: ## go vet.
	$(GO) vet ./...

lint: vet ## go vet + golangci-lint (si installé).
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint non installé, ignoré"

clean: ## Nettoyage des artefacts de build.
	rm -rf $(BIN_DIR) $(DIST_DIR)
