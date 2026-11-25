# Talksasa Pay developer tasks. Go runs in a pinned container (scripts/go.sh);
# override with `make GO=go <target>` if Go 1.26+ is installed locally.
SHELL := /bin/bash
GO    ?= ./scripts/go.sh
SQLC  := docker run --rm -u $(shell id -u):$(shell id -g) -v $(CURDIR):/src -w /src sqlc/sqlc:1.29.0
LINT  := docker run --rm -u $(shell id -u):$(shell id -g) -v $(CURDIR):/src -w /src \
           -v $(HOME)/go/pkg:/gopath/pkg -e GOPATH=/gopath -e GOMODCACHE=/gopath/pkg/mod \
           -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOLANGCI_LINT_CACHE=/tmp/lintcache golangci/golangci-lint:v2.11.0
SECRETS := deploy/dev-secrets
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: help dev-secrets up down db-reset migrate migrate-down migrate-version sqlc build test test-unit lint vuln fmt run admin worker-logs image smoke verify web-install web-gen web-check web-build e2e e2e-mpesa deploy-local deploy-staging deploy-production

help: ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

dev-secrets: ## generate throwaway local secrets (gitignored)
	@./scripts/dev-secrets.sh

up: dev-secrets ## start postgres, migrate, api
	docker compose up -d --build

down: ## stop the stack
	docker compose down

db-reset: ## DESTROY the local database volume and start fresh
	docker compose down -v
	docker compose up -d postgres

migrate: ## apply migrations to the local database
	TSP_ENV=development TSP_MIGRATE_DATABASE_URL_FILE=$(SECRETS)/migrate_database_url $(GO) run ./cmd/talksasa-pay migrate up

migrate-down: ## roll back one migration (development only)
	TSP_ENV=development TSP_MIGRATE_DATABASE_URL_FILE=$(SECRETS)/migrate_database_url $(GO) run ./cmd/talksasa-pay migrate down

migrate-version: ## show schema version
	TSP_ENV=development TSP_MIGRATE_DATABASE_URL_FILE=$(SECRETS)/migrate_database_url $(GO) run ./cmd/talksasa-pay migrate version

sqlc: ## regenerate internal/store from db/queries
	$(SQLC) generate

build: ## compile the binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/talksasa-pay ./cmd/talksasa-pay

test: dev-secrets ## all tests incl. database integration tests (needs `docker compose up -d postgres`)
	TSP_TEST_DATABASE_URL="$$(cat $(SECRETS)/test_admin_database_url)" TSP_REQUIRE_DB_TESTS=1 $(GO) test -race -count=1 ./...

test-unit: ## unit tests only (database tests skip)
	$(GO) test -race -count=1 ./...

lint: ## golangci-lint (incl. gosec) + sqlc vet
	$(LINT) golangci-lint run ./...
	$(SQLC) diff

vuln: ## govulncheck
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt: ## gofmt
	$(GO) fmt ./...

image: ## build the production image
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t talksasa-pay:$(VERSION) .

# Admin CLI against the local database, e.g.
#   make admin ARGS='product create --name "Cloud Hosting" --prefix HOST --actor "Jane"'
admin: ## run an admin command: make admin ARGS='app list'
	TSP_ENV=development TSP_DATABASE_URL_FILE=$(SECRETS)/database_url TSP_MASTER_KEYS_FILE=$(SECRETS)/master_keys \
	TSP_API_KEY_PEPPER_FILE=$(SECRETS)/api_key_pepper $(GO) run ./cmd/talksasa-pay admin $(ARGS)

worker-logs: ## follow the Docker stack's worker logs
	docker compose logs -f --tail 100 worker

smoke: ## end-to-end smoke test against the Docker stack
	./scripts/smoke.sh

verify: ## everything: Go tests, lint, vuln scan, web checks, smoke, browser and M-Pesa end-to-end tests
	$(MAKE) test
	$(MAKE) lint
	$(MAKE) vuln
	$(MAKE) web-check
	$(MAKE) smoke
	$(MAKE) e2e
	$(MAKE) e2e-mpesa

PNPM := COREPACK_ENABLE_DOWNLOAD_PROMPT=0 NEXT_TELEMETRY_DISABLED=1 corepack pnpm

web-install: ## install frontend dependencies (pnpm via corepack)
	$(PNPM) install --frozen-lockfile

web-gen: ## regenerate TypeScript API types from api/*.openapi.yaml
	$(PNPM) gen:api

web-check: web-install ## frontend: generated types current, typecheck, lint, unit tests
	$(PNPM) --filter @talksasa/api-client check-gen
	$(PNPM) -r typecheck
	$(PNPM) -r lint
	$(PNPM) -r test

web-build: ## production builds of the admin and pay apps
	$(PNPM) -r --filter './web/apps/*' build

e2e: ## browser end-to-end tests against the Docker stack (Playwright container)
	./scripts/e2e.sh

e2e-mpesa: ## M-Pesa end to end: API -> fake Daraja -> callback -> signed webhook verified by the PHP SDK
	./scripts/e2e-mpesa.sh

# Deploys: see docs/runbooks/deploy.md. Host via TSP_DEPLOY_HOST or ARGS='--host user@host'.
deploy-local: ## rebuild + restart the local stack, then smoke test
	./scripts/deploy.sh local

deploy-staging: ## test, build, ship and roll out to staging
	./scripts/deploy.sh staging $(ARGS)

deploy-production: ## same for production (clean main == origin/main, typed confirmation)
	./scripts/deploy.sh production $(ARGS)
