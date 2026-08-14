COMPOSE := docker compose
TOOLS   := $(COMPOSE) run --rm tools

.DEFAULT_GOAL := help
.PHONY: help init build up down restart logs ps test tidy clean

help: ## Show available commands
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "\033[36m%-18s\033[0m %s\n",$$1,$$2}'

init: ## One-time setup: create .env from example
	@test -f .env || cp .env.example .env
	@echo ".env ready — fill in secrets as needed"

build: ## Build all images
	$(COMPOSE) build

up: ## Start the dev stack (app + postgres)
	$(COMPOSE) up -d app

down: ## Stop the stack
	$(COMPOSE) down

restart: ## Restart the app service
	$(COMPOSE) restart app

logs: ## Follow logs (s=<service>, default app)
	$(COMPOSE) logs -f $(or $(s),app)

ps: ## Show stack status
	$(COMPOSE) ps

test: ## Run unit + application tests (no services needed)
	$(TOOLS) go test ./...

tidy: ## go mod tidy inside the container
	$(TOOLS) go mod tidy

clean: ## Stop stack and remove volumes + build artifacts
	$(COMPOSE) down -v
	rm -rf tmp
