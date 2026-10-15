# RepoMind — Make targets
# All targets are idempotent and safe to run repeatedly.

SHELL          := /usr/bin/env bash
GO             ?= go
GOFLAGS        ?= -trimpath
PKG            := github.com/repomind/repomind
BIN_DIR        := bin
BIN_NAME       := repomind
COVERAGE_FILE  := coverage.txt
DB_URL         ?= postgres://repomind:repomind@localhost:5432/repomind?sslmode=disable
MIGRATIONS_DIR := migrations

# Image
IMAGE_REPO     ?= ghcr.io/repomind/repomind
IMAGE_TAG      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format code
	$(GO) fmt ./...

.PHONY: vet
vet: ## Static analysis (go vet)
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint (requires golangci-lint installed)
	golangci-lint run ./...

.PHONY: test
test: ## Run unit tests with race detector
	$(GO) test $(GOFLAGS) -race -count=1 ./...

.PHONY: test-cover
test-cover: ## Run tests with coverage report
	$(GO) test $(GOFLAGS) -race -count=1 -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVERAGE_FILE) | tail -n 1

.PHONY: test-integration
test-integration: ## Run integration tests (testcontainers required)
	$(GO) test $(GOFLAGS) -race -count=1 -tags=integration ./...

.PHONY: build
build: ## Build the server binary
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags="-s -w -X main.version=$(IMAGE_TAG)" -o $(BIN_DIR)/$(BIN_NAME) ./cmd/repomind

.PHONY: run
run: ## Run the server locally
	$(GO) run ./cmd/repomind

.PHONY: run-dev
run-dev: ## Run locally with configs/config.dev.env + .env + .env.secrets loaded
	@set -a; \
		[ -f configs/config.dev.env ] && . configs/config.dev.env; \
		[ -f .env ] && . .env; \
		[ -f .env.secrets ] && . .env.secrets; \
		set +a; \
		$(GO) run ./cmd/repomind

.PHONY: docker-build
docker-build: ## Build production container
	docker build -f deploy/Dockerfile -t $(IMAGE_REPO):$(IMAGE_TAG) -t $(IMAGE_REPO):latest .

.PHONY: docker-pull
docker-pull: ## Pre-pull external images (postgres, migrate) so compose boots fast
	pwsh -NoProfile -File scripts/docker-pull-images.ps1 -Verify

.PHONY: docker-up
docker-up: ## Start full stack with docker-compose (loads .env + .env.secrets)
	@if [ ! -f .env ]; then \
		echo "ERROR: .env not found at repo root."; \
		echo "       Copy configs/config.dev.env to .env and fill in non-secret values."; \
		exit 1; \
	fi
	@if [ ! -f .env.secrets ]; then \
		echo "ERROR: .env.secrets not found at repo root."; \
		echo "       This file holds JIRA / CONFLUENCE / AI / GITHUB secrets."; \
		echo "       See .env header for which keys belong here."; \
		exit 1; \
	fi
	docker compose -f deploy/docker-compose.yml \
		--env-file .env \
		--env-file .env.secrets \
		up --build

.PHONY: docker-down
docker-down: ## Stop docker-compose stack
	docker compose -f deploy/docker-compose.yml down -v

.PHONY: migrate-up
migrate-up: ## Apply database migrations
	migrate -path $(MIGRATIONS_DIR) -database "$(DB_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back last database migration
	migrate -path $(MIGRATIONS_DIR) -database "$(DB_URL)" down 1

.PHONY: sqlc
sqlc: ## Regenerate type-safe SQL bindings
	sqlc generate

.PHONY: hooks
hooks: ## Install repo git hooks (secret-scanning pre-commit)
	git config core.hooksPath scripts/git-hooks
	chmod +x scripts/git-hooks/pre-commit 2>/dev/null || true
	@echo "Git hooks installed: core.hooksPath -> scripts/git-hooks"
	@echo "Bypass with 'git commit --no-verify' only when you understand why."

.PHONY: scan-secrets
scan-secrets: ## Dry-run the pre-commit secret scan against staged changes
	@sh scripts/git-hooks/pre-commit && echo "scan-secrets: clean"

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(COVERAGE_FILE) coverage.html
