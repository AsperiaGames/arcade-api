# arcade-api — build, test and deploy
#
# Mirrors the house Makefile pattern from Storm-Gate: self-documenting targets,
# `make` with no argument prints this help.

IMAGE_NAME  := arcade-api
# Must match `app` in fly.toml. `make logs` and `make status` address the
# deployed app by this name, so a mismatch fails against a name that simply
# does not exist rather than reporting anything useful.
FLY_APP     := ac-arcade-api
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS     := -s -w \
	-X github.com/HoseaCodes/arcade-api/internal/config.Version=$(VERSION) \
	-X github.com/HoseaCodes/arcade-api/internal/config.Commit=$(COMMIT)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "arcade-api — $(VERSION) ($(COMMIT))"
	@echo ""
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- Development -------------------------------------------------------------

.PHONY: run
run: ## Run the server locally (needs .env and a database)
	go run ./cmd/server

.PHONY: db-up
db-up: ## Start the local Postgres
	docker compose up -d postgres

.PHONY: db-down
db-down: ## Stop the local Postgres
	docker compose down

.PHONY: build
build: ## Build the binary into ./out
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o out/server ./cmd/server

# --- Quality -----------------------------------------------------------------

.PHONY: test
test: ## Run all tests (starts throwaway Postgres containers; needs Docker)
	go test ./...

.PHONY: test-verbose
test-verbose: ## Run all tests with per-test output
	go test ./... -v

.PHONY: cover
cover: ## Run tests and open a coverage report
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out

.PHONY: race
race: ## Run tests under the race detector
	go test ./... -race

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w ./cmd ./internal ./migrations

.PHONY: lint
lint: ## Vet and check formatting
	go vet ./...
	@unformatted=$$(gofmt -l ./cmd ./internal ./migrations); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	@echo "lint clean"

.PHONY: check
check: lint test ## Lint and test — run this before pushing

# --- Container ---------------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the production image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE_NAME):$(VERSION) .

.PHONY: docker-run
docker-run: ## Run the production image against your .env
	docker run --rm -p 8080:8080 --env-file .env $(IMAGE_NAME):$(VERSION)

# --- Deploy ------------------------------------------------------------------

.PHONY: check-env
check-env: ## Verify the tools and secrets a deploy needs
	@command -v flyctl >/dev/null || { echo "flyctl not installed"; exit 1; }
	@flyctl auth whoami >/dev/null 2>&1 || { echo "not logged in: run 'flyctl auth login'"; exit 1; }
	@echo "ready to deploy $(FLY_APP) at $(VERSION)"

.PHONY: release-preview
release-preview: ## Show the version and notes the next push to main would publish
	@NOTES_FILE=$$(mktemp) sh -c '.github/scripts/release-plan.sh; echo; cat "$$NOTES_FILE"; rm -f "$$NOTES_FILE"'

.PHONY: deploy
deploy: check-env ## Deploy to Fly.io (CI does this on every push to main)
	flyctl deploy --remote-only \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT)

.PHONY: logs
logs: ## Tail production logs
	flyctl logs -a $(FLY_APP)

.PHONY: status
status: ## Show the deployed version (reads /health)
	@curl -fsS https://$(FLY_APP).fly.dev/health | sed 's/^/  /'

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf out coverage.out coverage.html
