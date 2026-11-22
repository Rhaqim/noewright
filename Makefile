DSN ?= postgres://loom:loom@localhost:5432/loom?sslmode=disable
export NOEWRIGHT_DSN := $(DSN)

.PHONY: help up down migrate seed example test-engine test-cli test-schema test-all build clean generate check-facade check llms-full release

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Start PostgreSQL 16 in Docker
	docker compose up -d
	@echo "Waiting for Postgres to be healthy..."
	@until docker compose exec postgres pg_isready -U loom -d loom >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres is ready."

down: ## Stop and remove containers
	docker compose down

migrate: ## Apply Noewright schema to the database
	go run ./cmd/noewright migrate

seed: ## Seed example agents and prompts
	go run ./cmd/noewright seed examples/story/seed.yaml

example: ## Run the example story application
	go run ./examples/story

test-engine: ## Run engine + harness integration tests (requires Postgres)
	go test -v -count=1 -timeout 120s ./internal/enginetest/...

test-cli: ## Run CLI integration tests (requires Postgres)
	go test -v -count=1 -timeout 60s ./internal/clitest/...

test-schema: ## Run schema/migration tests incl. the Postgres upgrade paths (requires Postgres)
	go test -v -count=1 -timeout 120s ./schema/...

# -p 1 is REQUIRED: these packages share one Postgres database and the same
# loom_ prefixed tables. Run in parallel, they interleave schema migrations with
# each other's data and fail spuriously (a migration widening a unique key while
# another package is inserting rows). Serialising costs a few seconds and makes
# the suite deterministic.
test-all: ## Run all Postgres-backed tests serially (requires `make up`)
	go test -p 1 -v -count=1 -timeout 300s \
		./schema/... \
		./internal/enginetest/... \
		./internal/clitest/...

generate: llms-full ## Regenerate the facade (aliases.go) and llms-full.txt
	go generate ./...

llms-full: ## Regenerate llms-full.txt (curated guide + full go doc API reference)
	./scripts/gen-llms-full.sh

check-facade: ## Fail if aliases.go is stale (run `make generate` and commit)
	go generate ./...
	@if ! git diff --quiet -- aliases.go; then \
		echo "ERROR: aliases.go is out of date. Run 'make generate' and commit the result."; \
		git --no-pager diff -- aliases.go; \
		exit 1; \
	fi
	@echo "facade is up to date."

# NOEWRIGHT_DSN is cleared explicitly. This target is documented as the non-DB
# check, but the Makefile exports NOEWRIGHT_DSN unconditionally at the top — so
# without this the Postgres-backed tests do NOT skip, they spend their ping
# timeout dialling a database that is not running and then fail. Clearing it
# restores the intended "skip if no DSN" behaviour. LOOM_DSN (the pre-rename
# name, still read as a fallback) is cleared too, in case the shell sets it.
check: check-facade ## Run non-DB checks: facade sync, vet, build, race tests (no Postgres needed)
	go vet ./...
	go build ./...
	NOEWRIGHT_DSN= LOOM_DSN= go test -race -count=1 ./...
	cd generator/anthropic && go vet ./... && go build ./... && go test -race -count=1 ./...

release: ## Tag noewright (and generator/anthropic if it changed) as VERSION=vX.Y.Z on main; prints the push command
	@./scripts/release.sh "$(VERSION)"

build: ## Build the noewright binary
	go build -o bin/noewright ./cmd/noewright

clean: ## Remove build artefacts
	rm -rf bin/
