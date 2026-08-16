COMPOSE := docker compose
TOOLS   := $(COMPOSE) run --rm tools
PROD_COMPOSE := docker compose -f deploy/compose.prod.yml --env-file deploy/.env.prod

.DEFAULT_GOAL := help
.PHONY: help init build up up-auth up-mail up-mqtt up-signal down restart logs ps test test-race tidy clean migrate migrate-new sqlc db-shell test-integration vendor-js vendor-fonts lint fmt arch-check seed rebuild-model demo-ingest deploy-local deploy deploy-logs ollama-pull eval ext-build send-summary mqtt-tap mqtt-demo slack-smoke signal-register a2a-chat

help: ## Show available commands
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "\033[36m%-18s\033[0m %s\n",$$1,$$2}'

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

up-mail: ## Start the dev stack plus Mailpit (SMTP capture UI at http://localhost:8025) for the weekly summary
	$(COMPOSE) --profile mail up -d

ollama-pull: ## Pull a local model into the ollama service (m=qwen3:4b), starting it if needed
	@test -n "$(m)" || (echo "usage: make ollama-pull m=qwen3:4b"; exit 1)
	$(COMPOSE) --profile ollama up -d ollama
	$(COMPOSE) --profile ollama exec ollama ollama pull $(m)

up-mqtt: ## Start the dev stack plus mosquitto (MQTT event bridge, PRD §31-33/§12/§59), app pointed at it
	APP_MQTT_URL=tcp://mosquitto:1883 $(COMPOSE) --profile mqtt up -d

mqtt-tap: ## Tail every learner/# MQTT topic (needs `make up-mqtt` first)
	$(COMPOSE) --profile mqtt exec mosquitto mosquitto_sub -t 'learner/#' -v

up-signal: ## Start the dev stack plus the signal-cli JSON-RPC sidecar (Signal channel adapter, PRD §20/§20.1); set APP_SIGNAL_RPCURL + APP_SIGNAL_NUMBER together in .env once a device is linked (see deploy/signal/README.md), then `make restart`
	$(COMPOSE) --profile signal up -d

signal-register: ## One-time Signal device link: prints a QR/URI to scan with the Signal app on the account's phone (needs `make up-signal` first; see deploy/signal/README.md)
	$(COMPOSE) --profile signal exec signal-cli signal-cli --config /var/lib/signal-cli link -n "JLP"

a2a-chat: ## Start the dev stack plus the generic A2A chat client (http://localhost:8090), app's A2A adapter enabled; set A2A_AGENT_URL to point it at a different agent instead (see clients/a2a-chat/README.md)
	APP_A2A_ENABLED=true $(COMPOSE) --profile a2a-chat up -d

mqtt-demo: ## Publish a sample vocabulary.lookup ingest event over MQTT (needs `make up-mqtt` first); appears on /vocabulary for the "dev" identity
	$(COMPOSE) --profile mqtt exec mosquitto mosquitto_pub -t 'learner/dev/vocabulary/ingest' -m \
		'{"type":"vocabulary.lookup","expression":"待ち遠しい","reading":"まちどおしい","definition":"looking forward to, can hardly wait","example":"日曜日が待ち遠しいです。","source":{"type":"mqtt-demo","title":"make mqtt-demo"}}'

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

test-race: ## Run unit + application tests with the race detector (no services needed; catches goroutine data races `make test` alone misses)
	$(TOOLS) sh -c "CGO_ENABLED=1 go test -race ./..."

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

eval: ## Run the Japanese-correction eval corpus against the configured AI provider; regression-flags vs the previous report (PRD §48/§49)
	$(TOOLS) go run ./cmd/jlp eval

demo-ingest: ## POST 3 sample vocabulary lookups (PRD §12) against the running dev app; safe to rerun (client_event_id makes it idempotent)
	@set -a; [ -f .env ] && . ./.env; set +a; \
	PORT=$${APP_HOST_PORT:-8080}; \
	curl -sf -X POST "http://localhost:$$PORT/api/v1/vocabulary/events" -H "Content-Type: application/json" \
		-d '{"type":"vocabulary.lookup","expression":"取り組む","reading":"とりくむ","definition":"to tackle, to work on","example":"新しい仕事に取り組みます。","source":{"type":"novel","title":"コンビニ人間"},"client_event_id":"demo-torikumu"}' && echo; \
	curl -sf -X POST "http://localhost:$$PORT/api/v1/vocabulary/events" -H "Content-Type: application/json" \
		-d '{"type":"vocabulary.lookup","expression":"それはそれとして","reading":"","definition":"be that as it may; setting that aside","example":"それはそれとして、明日の会議の準備をしましょう。","source":{"type":"novel","title":"コンビニ人間"},"client_event_id":"demo-sorehasoretoshite"}' && echo; \
	curl -sf -X POST "http://localhost:$$PORT/api/v1/vocabulary/events" -H "Content-Type: application/json" \
		-d '{"type":"vocabulary.lookup","expression":"気配","reading":"けはい","definition":"sign, indication, hint of presence","example":"誰かがいる気配がした。","source":{"type":"novel","title":"コンビニ人間"},"client_event_id":"demo-kehai"}' && echo

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

vendor-fonts: ## Vendor pinned Instrument Sans + JetBrains Mono woff2 into web/static/fonts (design system, PRD §39/§45: no CDN fonts at runtime)
	$(TOOLS) sh -c "mkdir -p web/static/fonts && \
	                curl -fsSL https://cdn.jsdelivr.net/npm/@fontsource/instrument-sans@5.3.0/files/instrument-sans-latin-400-normal.woff2 -o web/static/fonts/instrument-sans-latin-400-normal.woff2 && \
	                curl -fsSL https://cdn.jsdelivr.net/npm/@fontsource/instrument-sans@5.3.0/files/instrument-sans-latin-500-normal.woff2 -o web/static/fonts/instrument-sans-latin-500-normal.woff2 && \
	                curl -fsSL https://cdn.jsdelivr.net/npm/@fontsource/instrument-sans@5.3.0/files/instrument-sans-latin-600-normal.woff2 -o web/static/fonts/instrument-sans-latin-600-normal.woff2 && \
	                curl -fsSL https://cdn.jsdelivr.net/npm/@fontsource/instrument-sans@5.3.0/files/instrument-sans-latin-700-normal.woff2 -o web/static/fonts/instrument-sans-latin-700-normal.woff2 && \
	                curl -fsSL https://cdn.jsdelivr.net/npm/@fontsource/jetbrains-mono@5.3.0/files/jetbrains-mono-latin-400-normal.woff2 -o web/static/fonts/jetbrains-mono-latin-400-normal.woff2 && \
	                curl -fsSL https://cdn.jsdelivr.net/npm/@fontsource/jetbrains-mono@5.3.0/files/jetbrains-mono-latin-500-normal.woff2 -o web/static/fonts/jetbrains-mono-latin-500-normal.woff2 && \
	                curl -fsSL https://cdn.jsdelivr.net/npm/@fontsource/jetbrains-mono@5.3.0/files/jetbrains-mono-latin-700-normal.woff2 -o web/static/fonts/jetbrains-mono-latin-700-normal.woff2"
	@echo "vendored $$(du -ch web/static/fonts/*.woff2 | tail -1 | cut -f1) of woff2 into web/static/fonts/ — fonts.css is committed by hand, not generated"

ext-build: ## Zip chrome-extension/ (excluding shim/ and README) into dist/jlp-extension.zip
	$(TOOLS) sh -c "mkdir -p dist && rm -f dist/jlp-extension.zip && cd chrome-extension && zip -r ../dist/jlp-extension.zip . -x 'shim/*' -x 'README.md'"

send-summary: up-mail ## Trigger one weekly summary send immediately (needs APP_SUMMARY_TO set; brings up Mailpit + postgres first)
	$(TOOLS) go run ./cmd/jlp send-summary

slack-smoke: ## Post one test message via the Slack bot token (needs APP_SLACK_BOTTOKEN + APP_SLACK_SMOKECHANNEL set; no live Slack test runs in `make test`)
	$(TOOLS) go run ./cmd/jlp slack-smoke

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
