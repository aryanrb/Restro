.DEFAULT_GOAL := help
API := api
-include $(API)/.env
export

help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n",$$1,$$2}'

gen: ## Regenerate everything: proto (buf) + sqlc, then tidy
	buf lint proto
	buf generate proto
	cd $(API) && go tool sqlc generate && go mod tidy

sqlc: ## Regenerate sqlc only (db/queries -> internal/db/sqlc)
	cd $(API) && go tool sqlc generate

dev: ## Run Postgres (compose) + the API on the host
	docker compose -f deploy/docker-compose.dev.yaml up -d postgres
	cd $(API) && go run ./cmd/api

run: ## Run the API (expects DATABASE_URL)
	cd $(API) && go run ./cmd/api

test: ## Run tests (-p 1 so DB integration tests don't race on the shared database)
	cd $(API) && go test -p 1 ./...

migrate: ## Apply migrations against $$DATABASE_URL (also auto-applied on boot)
	cd $(API) && go tool tern migrate --migrations ./db/migrations --conn-string "$(DATABASE_URL)"

new: ## Create a new migration: make new name=add_x_table
	cd $(API) && go tool tern new --migrations ./db/migrations $(name)

tidy: ## go mod tidy
	cd $(API) && go mod tidy

.PHONY: help gen sqlc dev run test migrate new tidy
