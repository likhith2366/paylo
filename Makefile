.DEFAULT_GOAL := help
.PHONY: help build test test-short test-race lint fmt up down reset ps logs psql seed ml-data ml-train k6

# Testcontainers is chatty; strip the emoji progress lines so real output shows.
FILTER := grep -v "🐳\|✅\|⏳\|🔔\|🚫"

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Compile every service
	go build ./...

test: ## Unit + integration tests (requires Docker)
	go test ./... -timeout 20m 2>&1 | $(FILTER)

test-short: ## Unit tests only, no containers
	go test ./... -short

test-race: ## Integration tests under the race detector
	go test ./... -race -timeout 30m 2>&1 | $(FILTER)

# The concurrency test is the project's thesis: 100 simultaneous requests with
# one idempotency key must produce exactly one bank authorization.
test-idempotency: ## Run the concurrency proof on its own
	go test ./internal/payments/ -run TestConcurrentDuplicateIdempotencyKey -v -count=1 -timeout 10m 2>&1 | $(FILTER)

lint: ## go vet and staticcheck
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || \
		echo "staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest"

fmt: ## Format Go sources
	gofmt -s -w .

up: ## Start the full local stack
	docker compose up -d --build

down: ## Stop the stack, keep data
	docker compose down

reset: ## Stop and destroy volumes (migrations re-run on next up)
	docker compose down -v

ps: ## Service health
	docker compose ps --format 'table {{.Service}}\t{{.Status}}\t{{.Ports}}'

logs: ## Follow all logs
	docker compose logs -f

psql: ## Open a psql shell
	docker compose exec postgres psql -U paylo -d paylo

seed: ## Create a test merchant and print its API key
	go run ./scripts/seed

ml-data: ## Download the Kaggle fraud datasets
	python ml/download_data.py

ml-train: ## Train the fraud model
	python ml/training/train.py --dataset sparkov

k6: ## Load-test the idempotency endpoint
	k6 run test/load/idempotency.js
