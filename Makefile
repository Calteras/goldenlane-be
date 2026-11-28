# `make help` lists the targets.
-include .env
export

.PHONY: help run migrate test test-db lint vet build docker release

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
	docker build --target production -t goldenlane-api:local .

# Release images: $(REGISTRY)/goldenlane-api and $(REGISTRY)/goldenlane-migrate, tagged with the commit.
# Both come from the commit you are on, so commit first. No variables to export: they have defaults.
REGISTRY ?= dolanddev
IMAGE_TAG ?= $(shell git rev-parse --short=12 HEAD)
PLATFORM ?= linux/amd64

release: ## Build and push this commit's API and migration images (REGISTRY, PLATFORM)
	@test -z "$$(git status --porcelain)" \
		|| { echo "release: commit first; the images are tagged with the commit ($(IMAGE_TAG))" >&2; exit 1; }
	@test -n "$(REGISTRY)" -a -n "$(IMAGE_TAG)" \
		|| { echo "release: REGISTRY and IMAGE_TAG must not be empty" >&2; exit 1; }
	docker build --platform $(PLATFORM) --target production --build-arg APP_VERSION=$(IMAGE_TAG) -t $(REGISTRY)/goldenlane-api:$(IMAGE_TAG) .
	docker build --platform $(PLATFORM) --target migration -t $(REGISTRY)/goldenlane-migrate:$(IMAGE_TAG) .
	docker push $(REGISTRY)/goldenlane-api:$(IMAGE_TAG)
	docker push $(REGISTRY)/goldenlane-migrate:$(IMAGE_TAG)
	@echo "Deploy it with APP_IMAGE=$(REGISTRY)/goldenlane-api:$(IMAGE_TAG) and MIGRATION_IMAGE=$(REGISTRY)/goldenlane-migrate:$(IMAGE_TAG)"
