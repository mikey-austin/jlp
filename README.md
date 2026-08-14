# JLP — Japanese Learning Platform

Write Japanese, get AI-backed corrections with deterministic diffs, accept
or reject them, and watch your learner statistics build up over time.
Phase 1 MVP: sessions, a writing editor with autosave, AI structured
feedback, learning events, basic statistics, AI observability, and a
JSON API — all run locally via `make` on Docker Compose.

## Quickstart

```sh
make init      # copy .env.example -> .env, generate Authelia dev users
make build     # build the app image
make up        # start postgres + app (hot reload via air)
make migrate   # apply database migrations
make seed      # create a demo identity, session, and document
```

Then open **http://localhost:8080**.

If port 8080 is already taken on your machine, set `APP_HOST_PORT` in
`.env` (e.g. `APP_HOST_PORT=28080`) before `make up` and open
`http://localhost:$APP_HOST_PORT` instead — the container always listens
on 8080 internally, only the published host port changes.

## Everyday commands (`make help`)

```
help               Show available commands
init               One-time setup: create .env from example, generate Authelia dev users
build              Build all images
up                 Start the dev stack (app + postgres)
up-auth            Start dev stack including Caddy + Authelia (https://jlp.localhost:8443)
down               Stop the stack (including profile-gated services like Caddy/Authelia)
restart            Restart the app service
logs               Follow logs (s=<service>, default app)
ps                 Show stack status
test               Run unit + application tests (no services needed)
lint               Run all linters (govet, staticcheck, errcheck, ineffassign, unused, depguard)
fmt                gofmt the whole tree
arch-check         Enforce PRD §75 dependency-direction rules only (depguard)
tidy               go mod tidy inside the container
clean              Stop stack and remove volumes + build artifacts
migrate            Apply database migrations
seed               Populate a dev-friendly identity, session, and document (idempotent)
rebuild-model      Recompute every identity's learner_observations from learning_events (safe to rerun)
migrate-new        Create a migration (n=short_name)
sqlc               Regenerate sqlc query code
db-shell           psql into the dev database
test-integration   Adapter tests against compose services
vendor-js          Vendor pinned htmx + alpine into web/static/js
deploy-local       Run the production stack locally (https://<JLP_DOMAIN>:8444, see deploy/.env.prod)
deploy             Deploy to $(DEPLOY_HOST) over SSH (set in .env)
deploy-logs        Tail remote app logs
```

Every command goes through `make` and runs inside containers — there is
no host-installed Go, golangci-lint, or sqlc requirement. If you need a
one-off Go/psql/sqlc command that isn't already a target, add one rather
than reaching for `docker compose run` directly.

## Auth modes

Two `auth.Authenticator` adapters are wired behind `APP_AUTH_MODE`:

- **`static` (default)** — every request is the same dev identity
  (`dev` / `Dev Learner`, configurable via `APP_AUTH_STATIC_ID` /
  `APP_AUTH_STATIC_DISPLAYNAME`). No login flow, no certificates —
  just `make up` and go straight to `http://localhost:8080`.
- **`authelia`** — real forward-auth login via Caddy + Authelia. Run
  `make up-auth` and open **https://jlp.localhost:8443**. The dev TLS
  cert is self-signed, so the browser will show a certificate warning on
  first visit — click through it (or add the cert to your trust store)
  to reach the login page. Dev credentials come from
  `AUTHELIA_DEV_PASSWORD` in `.env` (default `devpassword`); `make init`
  generates the corresponding Authelia users file.

## AI providers

`APP_AI_PROVIDER` selects the `ai.StructuredGenerator` behind the Teacher
agent; every call — whichever provider — is wrapped by an observability
decorator that records latency, cost, and success to `ai_requests` (see
the `/ai` page).

- **`fake` (default)** — a deterministic in-process provider with no
  network calls and no API key. This is what `make test` and a clean
  `make up` use out of the box, and what the seeded session's feedback
  flow exercises end to end.
- **`anthropic`** — live calls to Claude. Set in `.env`:

  ```
  APP_AI_PROVIDER=anthropic
  ANTHROPIC_API_KEY=sk-ant-...
  ```

  Optional overrides (leave blank to use the built-in defaults —
  `claude-sonnet-5` / `https://api.anthropic.com`):

  ```
  APP_AI_ANTHROPIC_MODEL=
  APP_AI_ANTHROPIC_BASEURL=
  ```

  `make test` always uses the fake provider regardless of `.env` — a
  live key is never required to run the test suite.

## Deploying

The production stack (`deploy/compose.prod.yml`) is a separate compose
project (`jlp-prod`) from the dev stack above — a distroless app image,
Caddy terminating TLS, and Authelia forward-auth, all fronting Postgres.
It runs the same way whether you're verifying it locally or deploying to
a real host:

```sh
sh scripts/gen-prod-secrets.sh          # Authelia JWT/session/storage secrets + one login user
cp deploy/.env.prod.example deploy/.env.prod   # fill in JLP_DOMAIN, POSTGRES_PASSWORD, etc.
make deploy-local                       # builds, starts, migrates, waits for /healthz
```

`make deploy-local` verifies the full production stack on your own
machine at **https://localhost:8444** (self-signed cert — click
through the browser warning). It runs alongside the dev stack without
colliding: distinct compose project, network subnet, and volumes.

To deploy to a real host instead, set `DEPLOY_HOST` (a docker-context-style
`user@host`) in `.env`, then:

```sh
make deploy         # builds, starts, and migrates over DOCKER_HOST=ssh://$DEPLOY_HOST
make deploy-logs    # tail the remote app service's logs
```

`gen-prod-secrets.sh` is idempotent — rerunning it (e.g. as part of a
later `make deploy-local`) never rotates secrets out from under a
running stack unless `FORCE=1` is set. `deploy/.env.prod` is gitignored
and must never be committed.

## Architecture

Modular monolith in Go with strict hexagonal boundaries:

```
domain  ←  application  ←  ports  ←  adapters
```

- **`internal/domain`** — pure business types and logic (sessions,
  writing documents, corrections, rune-level diffing, learning events).
  No imports of adapters, application, ports, or infrastructure packages
  (`net/http`, database drivers, config).
- **`internal/application`** — use-case services (`sessions.Service`,
  `writing.Service`, `feedback.Service`, `analytics.Service`,
  `learning.Recorder`) that depend on `internal/ports` interfaces only,
  never on concrete adapters.
- **`internal/ports`** — the interfaces the application layer depends on:
  `storage` (repositories), `ai` (`StructuredGenerator`), `auth`
  (`Authenticator`), `events` (`EventBus`).
- **`internal/adapters`** — concrete implementations: `postgres`
  (pgx + sqlc), `http` (chi server, HTML + JSON API), `staticauth` /
  `authelia`, `inprocbus`, `fakeai` / `anthropic`.
- **`internal/agent/teacher`** — the AI agent that reviews writing; it
  reaches the AI port and domain types only, never a repository.

These boundaries are enforced by `.golangci.yml`'s `depguard` rules
(encoding PRD §75 Rules 1–3) and checked in isolation by `make
arch-check`, separately from the full `make lint`.

**Reference docs:**
- PRD: [`docs/japanese_learning_platform_jlp_prd.md`](docs/japanese_learning_platform_jlp_prd.md)
- Design spec: [`docs/superpowers/specs/2026-08-14-jlp-platform-design.md`](docs/superpowers/specs/2026-08-14-jlp-platform-design.md)
- Phase 1 implementation plan: [`docs/superpowers/plans/2026-08-14-jlp-phase1-mvp.md`](docs/superpowers/plans/2026-08-14-jlp-phase1-mvp.md)
- Roadmap: [`docs/superpowers/plans/2026-08-14-jlp-roadmap.md`](docs/superpowers/plans/2026-08-14-jlp-roadmap.md)

## Testing and quality gates

```sh
make test              # unit + application tests, fake AI provider, no services needed
make test-integration  # adapter tests against a real postgres (docker compose)
make lint              # govet, staticcheck, errcheck, ineffassign, unused, depguard
make arch-check         # depguard only, PRD §75 dependency-direction rules
```

All four are expected to pass before every commit; `test-integration`
and `arch-check` are the ones easiest to forget locally since they need
services running or a narrower lint pass, respectively.

## Project layout

```
cmd/jlp/                     entrypoint: serve | migrate | seed
internal/config/             viper -> typed Config, validation
internal/domain/             learner, session, writing, correction, diff, event
internal/application/        sessions, writing, feedback, learning, analytics
internal/ports/               auth, ai, events, storage interfaces
internal/adapters/           http, postgres, staticauth, authelia, inprocbus, fakeai, anthropic
internal/agent/teacher/      the Teacher AI agent (ReviewWriting)
internal/observability/      AI request/cost/latency recording decorator
internal/prompts/            embedded, versioned prompt templates
internal/schemas/            embedded JSON Schemas + validator
web/templates/                layout, pages, HTMX partials
web/static/                   CSS, vendored htmx/alpine, PWA assets
db/queries/                   sqlc query files
internal/adapters/postgres/migrationsfs/  goose SQL migrations (embedded)
deploy/                        production compose, Caddyfile, Authelia config
```
