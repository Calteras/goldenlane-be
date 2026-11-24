# `make help` lists the targets.
-include .env
export

.PHONY: help run migrate test test-db lint vet build docker

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

migrate: ## Apply the database migrations (needs PG_URL)
	go run ./cmd/migrate -path migrations up

run: migrate ## Migrate, then run the API with the settings in .env
	go run ./cmd/app

test: ## Unit tests (database tests are skipped without TEST_PG_URL)
	go test ./...

test-db: ## All tests against TEST_PG_URL, one package at a time (they share the database)
	@test -n "$(TEST_PG_URL)" || { echo "set TEST_PG_URL to an empty PostgreSQL database"; exit 1; }
	go test -p 1 -race ./...

vet: ## go vet
	go vet ./...

lint: ## golangci-lint (needs it installed)
	golangci-lint run --timeout=10m

build: ## Build the binaries into bin/
	go build -trimpath -ldflags='-s -w' -o bin/ ./cmd/app ./cmd/migrate ./cmd/healthcheck

docker: ## Build the production image
	docker build --target production -t streetfood-customer-api:local .
