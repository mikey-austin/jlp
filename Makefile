COMPOSE := docker compose
TOOLS   := $(COMPOSE) run --rm tools
PROD_COMPOSE := docker compose -f deploy/compose.prod.yml --env-file deploy/.env.prod

.DEFAULT_GOAL := help
.PHONY: help init build up up-auth down restart logs ps test tidy clean migrate migrate-new sqlc db-shell test-integration vendor-js lint fmt arch-check seed rebuild-model deploy-local deploy deploy-logs

help: ## Show available commands
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "\033[36m%-18s\033[0m %s\n",$$1,$$2}'

init: ## One-time setup: create .env from example, generate Authelia dev users
	@test -f .env || cp .env.example .env
	@echo ".env ready — fill in secrets as needed"
	@set -a; . ./.env; set +a; sh scripts/gen-authelia-users.sh

build: ## Build all images
	$(COMPOSE) build

up: ## Start the dev stack (app + postgres)
	$(COMPOSE) up -d app

up-auth: ## Start dev stack including Caddy + Authelia (https://jlp.localhost:8443)
	APP_AUTH_MODE=authelia $(COMPOSE) --profile auth up -d

down: ## Stop the stack (including profile-gated services like Caddy/Authelia)
	$(COMPOSE) --profile auth down

restart: ## Restart the app service
	$(COMPOSE) restart app

logs: ## Follow logs (s=<service>, default app)
	$(COMPOSE) logs -f $(or $(s),app)

ps: ## Show stack status
	$(COMPOSE) ps

test: ## Run unit + application tests (no services needed)
	$(TOOLS) go test ./...

lint: ## Run all linters (govet, staticcheck, errcheck, ineffassign, unused, depguard)
	$(TOOLS) golangci-lint run ./...

fmt: ## gofmt the whole tree
	$(TOOLS) gofmt -w .

arch-check: ## Enforce PRD §75 dependency-direction rules only (depguard)
	$(TOOLS) golangci-lint run --enable-only depguard ./...

tidy: ## go mod tidy inside the container
	$(TOOLS) go mod tidy

clean: ## Stop stack and remove volumes + build artifacts
	$(COMPOSE) down -v
	rm -rf tmp

migrate: ## Apply database migrations
	$(TOOLS) go run ./cmd/jlp migrate

seed: ## Populate a dev-friendly identity, session, and document (idempotent)
	$(TOOLS) go run ./cmd/jlp seed

rebuild-model: ## Recompute every identity's learner_observations from learning_events (safe to rerun)
	$(TOOLS) go run ./cmd/jlp rebuild-model

migrate-new: ## Create a migration (n=short_name)
	@test -n "$(n)" || (echo "usage: make migrate-new n=add_table"; exit 1)
	@f=internal/adapters/postgres/migrationsfs/$$(date +%Y%m%d%H%M%S)_$(n).sql; \
	printf -- "-- +goose Up\n\n-- +goose Down\n" > $$f; echo created $$f

sqlc: ## Regenerate sqlc query code
	$(TOOLS) sqlc generate

db-shell: ## psql into the dev database
	$(COMPOSE) exec postgres psql -U jlp jlp

test-integration: ## Adapter tests against compose services
	$(COMPOSE) up -d postgres
	$(TOOLS) go test -tags integration ./internal/adapters/...

vendor-js: ## Vendor pinned htmx + alpine into web/static/js
	$(TOOLS) sh -c "curl -fsSL https://unpkg.com/htmx.org@2.0.10/dist/htmx.min.js -o web/static/js/htmx.min.js && \
	                curl -fsSL https://unpkg.com/alpinejs@3.16.1/dist/cdn.min.js -o web/static/js/alpine.min.js"

deploy-local: ## Run the production stack locally (https://<JLP_DOMAIN>:8444, see deploy/.env.prod)
	$(PROD_COMPOSE) build
	$(PROD_COMPOSE) up -d
	$(PROD_COMPOSE) run --rm app migrate
	@set -a; . deploy/.env.prod; set +a; sh scripts/wait-healthy.sh "https://$${JLP_DOMAIN}:8444/healthz"

deploy: ## Deploy to $(DEPLOY_HOST) over SSH (set in .env)
	@set -ea; [ -f .env ] && . ./.env; set +a; \
	test -n "$$DEPLOY_HOST" || { echo "set DEPLOY_HOST in .env"; exit 1; }; \
	DOCKER_HOST=ssh://$$DEPLOY_HOST $(PROD_COMPOSE) build; \
	DOCKER_HOST=ssh://$$DEPLOY_HOST $(PROD_COMPOSE) up -d; \
	DOCKER_HOST=ssh://$$DEPLOY_HOST $(PROD_COMPOSE) run --rm app migrate; \
	echo "deployed to $$DEPLOY_HOST"

deploy-logs: ## Tail remote app logs
	@set -a; [ -f .env ] && . ./.env; set +a; \
	DOCKER_HOST=ssh://$$DEPLOY_HOST $(PROD_COMPOSE) logs -f app
