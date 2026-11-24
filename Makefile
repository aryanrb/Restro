.DEFAULT_GOAL := help
API := api
-include $(API)/.env
export

help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n",$$1,$$2}'

run: ## Run the API locally (migrations auto-apply on boot; reads api/.env)
	./scripts/dev.sh

up: ## Run the full stack in Docker (Postgres + API, migrations + seed on boot)
	docker compose -f deploy/docker-compose.yaml up --build

down: ## Stop the Docker stack
	docker compose -f deploy/docker-compose.yaml down

dev: ## Start the dev Postgres in Docker, then run the API on the host
	docker compose -f deploy/docker-compose.dev.yaml up -d postgres
	cd $(API) && go run ./cmd/api

test: ## Run all tests (creates a local throwaway test DB)
	./scripts/test.sh

reset-db: ## Drop & recreate a local DB for a clean slate: make reset-db name=restro
	./scripts/reset-db.sh $(name)

gen: ## Regenerate everything: proto (buf) + sqlc, then tidy
	buf lint proto
	buf generate proto
	cd $(API) && go tool sqlc generate && go mod tidy

sqlc: ## Regenerate sqlc only (db/queries -> internal/db/sqlc)
	cd $(API) && go tool sqlc generate

migrate: ## Apply migrations against $$DATABASE_URL (normally auto-applied on boot)
	cd $(API) && go tool tern migrate --migrations ./db/migrations --conn-string "$(DATABASE_URL)"

new: ## Create a new migration: make new name=add_x_table
	cd $(API) && go tool tern new --migrations ./db/migrations $(name)

tidy: ## go mod tidy
	cd $(API) && go mod tidy

.PHONY: help run up down dev test reset-db gen sqlc migrate new tidy
