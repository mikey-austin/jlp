# JLP Phase 1 (MVP) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the fundamental JLP loop — Write → Feedback → Learn → Record → Adapt — as a browser-usable app: sessions, Japanese writing editor with autosave, AI structured corrections with deterministic diffs, learning events, basic learner statistics, AI observability + ratings, PWA shell, all operated via `make` on Docker Compose.

**Architecture:** Modular monolith in Go with hexagonal boundaries (`domain` ← `application` ← `ports` ← `adapters`). HTMX + Go templates UI and a parallel JSON API share the same application services. One in-process event bus behind a port; PostgreSQL behind repositories; AI behind a `StructuredGenerator` capability port with fake + Anthropic adapters and an observability decorator.

**Tech Stack:** Go ≥1.25, Chi v5, HTMX 2 + Alpine.js 3 (vendored), PostgreSQL 17, pgx/v5 + sqlc, goose (as library, embedded migrations), viper, slog, sergi/go-diff, santhosh-tekuri/jsonschema/v6, anthropic-sdk-go, Caddy 2 + Authelia, air (hot reload), golangci-lint, Docker Compose.

**Reference docs:** PRD `docs/japanese_learning_platform_jlp_prd.md`; design spec `docs/superpowers/specs/2026-08-14-jlp-platform-design.md` (authoritative for tech decisions); master roadmap `docs/superpowers/plans/2026-08-14-jlp-roadmap.md`.

## Global Constraints

- Module path: `github.com/mikeyaustin/jlp`. Go ≥ 1.25.
- **Every command goes through `make`.** Never invoke `go`, `docker compose`, or `psql` directly outside Makefile targets — if a target is missing, add it. The Go toolchain runs only inside containers (`docker compose run --rm tools …`); never assume host-installed Go.
- **Browser verification is mandatory and done by YOU, the implementing subagent.** Any task with a `Browser verification` step: run `make up`, then use the claude-in-chrome tools (load them via one ToolSearch call: `select:mcp__claude-in-chrome__tabs_context_mcp,mcp__claude-in-chrome__navigate,mcp__claude-in-chrome__computer,mcp__claude-in-chrome__read_page,mcp__claude-in-chrome__tabs_create_mcp,mcp__claude-in-chrome__javascript_tool,mcp__claude-in-chrome__read_console_messages`) to drive `http://localhost:8080` and confirm each listed check, including "no console errors". The task is NOT complete until this passes.
- PRD §75 architectural rules are law. In particular: Rule 1/2 (domain imports no adapters/AI/HTTP/SQL), Rule 4 (AI output schema-validated), Rule 5 (backend computes deterministic diffs), Rule 6 (all learner state identity-scoped — every repository method takes an identity ID), Rule 10 (prompts versioned), Rule 11 (AI interactions recorded in `ai_requests`).
- No CDN/external assets. htmx/alpine/CSS/fonts are vendored under `web/static/`.
- Japanese text safety: all offsets and counts are **rune-based**, never bytes, never UTF-16 units. Tests must include multi-byte Japanese strings.
- TDD: write the failing test first, watch it fail (`make test`), implement, watch it pass. Commit at the end of every task with the given message.
- `make test` must pass before any commit; `make test-integration` must pass for tasks touching adapters; `make lint` and `make arch-check` apply from Task 18 (which introduces them) onward.
- Secrets live in `.env` (gitignored). `.env.example` is committed and must stay current. `make test` must never require a live API key — the fake AI adapter and httptest-recorded responses cover everything.
- IDs are `github.com/google/uuid` v4 strings unless a table says otherwise.

## File Structure (target state after this plan)

```
cmd/jlp/main.go                      entrypoint: serve | migrate | seed
internal/config/config.go            viper → typed Config, validation
internal/domain/learner/identity.go  Identity, IdentityID
internal/domain/session/session.go   Session, Profile
internal/domain/writing/document.go  Document, DocumentVersion, rune stats
internal/domain/correction/…         Correction types, Apply()
internal/domain/diff/diff.go         rune-level Segment diff
internal/domain/event/event.go       LearningEvent, event Type constants
internal/application/sessions/…      session use cases
internal/application/writing/…       autosave/versions use cases
internal/application/feedback/…      feedback pipeline
internal/application/learning/…      event Recorder
internal/application/analytics/…     statistics queries
internal/ports/auth/auth.go          Authenticator port
internal/ports/ai/ai.go              StructuredGenerator port + types
internal/ports/events/bus.go         EventBus port
internal/ports/storage/…             repository interfaces (one file per aggregate)
internal/adapters/http/…             chi server, handlers, render, middleware
internal/adapters/postgres/…         pgx pool, sqlc output, repo implementations
internal/adapters/staticauth/…       dev identity adapter
internal/adapters/authelia/…         forward-auth header adapter
internal/adapters/inprocbus/…        in-process EventBus
internal/adapters/fakeai/…           deterministic fake StructuredGenerator
internal/adapters/anthropic/…        Anthropic adapter (forced tool use)
internal/observability/airequests.go AI observability decorator
internal/agent/teacher/teacher.go    Teacher agent (ReviewWriting)
internal/prompts/…                   embedded versioned prompt templates
internal/schemas/…                   embedded JSON Schemas + validator
web/templates/…                      layout, pages, HTMX partials
web/static/…                         css, vendored js, PWA assets
internal/adapters/postgres/migrationsfs/…  goose SQL migrations (embedded)
db/queries/…                         sqlc query files
deploy/…                             prod compose, Caddyfile, authelia config
Makefile, Dockerfile, docker-compose.yml, .air.toml, sqlc.yaml, .golangci.yml
```

---

## Epic A — Foundation

### Task 1: Repo scaffold, Docker Compose, Makefile, hello server

**Files:**
- Create: `go.mod`, `cmd/jlp/main.go`, `internal/adapters/http/server.go`, `internal/adapters/http/render.go`, `web/templates/layout.html.tmpl`, `web/templates/home.html.tmpl`, `web/static/css/app.css`, `Dockerfile`, `docker-compose.yml`, `.air.toml`, `Makefile`, `.env.example`, `.gitignore`, `.dockerignore`
- Test: `internal/adapters/http/server_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `httpx.NewServer(opts Options) *Server` with `Options{Addr string}` and `(*Server).HandlerForTest() http.Handler` (the chi mux, for handler tests); `httpx.Render(w, r, name string, data any)` template helper; Make targets `build up down restart logs ps test clean help`; compose services `app`, `postgres`, `tools`. Package name for `internal/adapters/http` is **`httpx`**.

- [ ] **Step 1: Write scaffold files**

`go.mod` (versions here are floors; `go mod tidy` in the container resolves exact ones):

```
module github.com/mikeyaustin/jlp

go 1.25
```

`cmd/jlp/main.go`:

```go
package main

import (
	"log/slog"
	"os"

	httpx "github.com/mikeyaustin/jlp/internal/adapters/http"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	switch cmd {
	case "serve":
		srv := httpx.NewServer(httpx.Options{Addr: ":8080"})
		slog.Info("listening", "addr", ":8080")
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
	default:
		slog.Error("unknown command", "cmd", cmd)
		os.Exit(2)
	}
}
```

`internal/adapters/http/server.go` (package **httpx**):

```go
package httpx

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Options struct {
	Addr string
}

type Server struct {
	http.Server
}

func NewServer(opts Options) *Server {
	s := &Server{}
	s.Addr = opts.Addr
	s.Handler = s.routes()
	return s
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	fs := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	r.Handle("/static/*", fs)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		Render(w, r, "home", map[string]any{"Title": "JLP"})
	})
	return r
}

func (s *Server) HandlerForTest() http.Handler { return s.Handler }
```

`internal/adapters/http/render.go` — parse templates from disk each render in dev (air restarts cover code; disk parse covers template-only edits):

```go
package httpx

import (
	"html/template"
	"log/slog"
	"net/http"
)

func Render(w http.ResponseWriter, r *http.Request, page string, data any) {
	t, err := template.ParseFiles(
		"web/templates/layout.html.tmpl",
		"web/templates/"+page+".html.tmpl",
	)
	if err != nil {
		slog.Error("template parse", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("template exec", "page", page, "err", err)
	}
}
```

`web/templates/layout.html.tmpl`:

```html
{{define "layout"}}<!doctype html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} — JLP</title>
  <link rel="stylesheet" href="/static/css/app.css">
</head>
<body>
  <header class="topbar"><a href="/" class="brand">日本語 JLP</a><nav id="topnav"></nav></header>
  <main>{{template "content" .}}</main>
</body>
</html>{{end}}
```

`web/templates/home.html.tmpl`:

```html
{{define "content"}}<section class="home"><h1>ようこそ</h1><p>JLP — Japanese output learning platform.</p></section>{{end}}
```

`web/static/css/app.css` (starter tokens; grows in later tasks):

```css
:root { --bg:#ffffff; --fg:#1a1a1a; --accent:#b7282e; --muted:#666; --border:#ddd;
        font-family: "Hiragino Sans","Noto Sans JP",system-ui,sans-serif; }
* { box-sizing:border-box; } body { margin:0; background:var(--bg); color:var(--fg); }
.topbar { display:flex; gap:1rem; align-items:center; padding:.5rem 1rem; border-bottom:1px solid var(--border); }
.brand { font-weight:700; color:var(--accent); text-decoration:none; }
main { padding:1rem; max-width:1400px; margin:0 auto; }
```

`Dockerfile`:

```dockerfile
FROM golang:1.25-alpine AS base
WORKDIR /src
ENV CGO_ENABLED=0

FROM base AS dev
RUN apk add --no-cache git curl postgresql17-client && \
    go install github.com/air-verse/air@latest && \
    go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest && \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
CMD ["air", "-c", ".air.toml"]

FROM base AS build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/jlp ./cmd/jlp

FROM gcr.io/distroless/static-debian12 AS prod
WORKDIR /app
COPY --from=build /out/jlp /app/jlp
COPY web /app/web
EXPOSE 8080
ENTRYPOINT ["/app/jlp"]
CMD ["serve"]
```

`.air.toml`:

```toml
root = "."
tmp_dir = "tmp"
[build]
cmd = "go build -o ./tmp/jlp ./cmd/jlp"
bin = "./tmp/jlp"
args_bin = ["serve"]
include_ext = ["go", "tmpl", "css", "js", "json", "sql", "md"]
exclude_dir = ["tmp", "docs", "deploy"]
[log]
main_only = true
```

`docker-compose.yml`:

```yaml
services:
  app:
    build: { context: ., target: dev }
    ports: ["8080:8080"]
    volumes:
      - .:/src
      - gomod:/go/pkg/mod
      - gocache:/root/.cache/go-build
    environment:
      APP_DATABASE_URL: postgres://jlp:jlp@postgres:5432/jlp?sslmode=disable
      APP_AUTH_MODE: ${APP_AUTH_MODE:-static}
      APP_AI_PROVIDER: ${APP_AI_PROVIDER:-fake}
      APP_AI_ANTHROPIC_APIKEY: ${ANTHROPIC_API_KEY:-}
    depends_on:
      postgres: { condition: service_healthy }

  postgres:
    image: postgres:17-alpine
    environment: { POSTGRES_USER: jlp, POSTGRES_PASSWORD: jlp, POSTGRES_DB: jlp }
    ports: ["5432:5432"]
    volumes: [ "pgdata:/var/lib/postgresql/data" ]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U jlp"]
      interval: 2s
      timeout: 3s
      retries: 15

  tools:
    build: { context: ., target: dev }
    profiles: ["tools"]
    volumes:
      - .:/src
      - gomod:/go/pkg/mod
      - gocache:/root/.cache/go-build
    environment:
      APP_DATABASE_URL: postgres://jlp:jlp@postgres:5432/jlp?sslmode=disable

volumes: { pgdata: {}, gomod: {}, gocache: {} }
```

`Makefile` (tabs, not spaces, for recipes):

```make
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
```

`.env.example`:

```
# Copy to .env (make init). Never commit .env.
ANTHROPIC_API_KEY=
APP_AI_PROVIDER=fake          # fake | anthropic
APP_AUTH_MODE=static          # static | authelia
AUTHELIA_DEV_PASSWORD=devpassword
DEPLOY_HOST=                  # user@host for make deploy
```

`.gitignore`:

```
.env
tmp/
*.local
deploy/authelia/secrets/
```

`.dockerignore`:

```
.git
tmp
docs
```

- [ ] **Step 2: Resolve dependencies and boot the stack**

Run: `make init && make tidy` (fetches chi; creates go.sum), then `make build && make up`.
Expected: `make ps` shows `app` and `postgres` running; `make logs` shows air building and `"listening"`.

- [ ] **Step 3: Write the failing-then-passing handler test**

`internal/adapters/http/server_test.go`:

```go
package httpx

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Templates are read from web/templates relative to repo root.
	os.Chdir("../../..")
	os.Exit(m.Run())
}

func TestHealthz(t *testing.T) {
	srv := NewServer(Options{Addr: ":0"})
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("healthz body = %s", rec.Body.String())
	}
}

func TestHomeRenders(t *testing.T) {
	srv := NewServer(Options{Addr: ":0"})
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("home status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ようこそ") {
		t.Fatalf("home body missing greeting")
	}
}
```

Run: `make test` — Expected: PASS (2 tests).

- [ ] **Step 4: Browser verification (claude-in-chrome, do it yourself)**

1. `make up`, wait for `make logs` to show "listening".
2. Navigate to `http://localhost:8080/` — page shows "ようこそ" with the JLP topbar, styled (CSS loaded, not plain serif).
3. Navigate to `http://localhost:8080/healthz` — shows `{"status":"ok"}`.
4. Read console messages — no errors.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: scaffold Go monolith with compose stack, Makefile, and hello server"
```

---

### Task 2: Typed configuration with fail-fast validation

**Files:**
- Create: `internal/config/config.go`
- Modify: `cmd/jlp/main.go` (load config before serve), `internal/adapters/http/server.go` (Options gets nothing new yet — only Addr comes from config)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Load() (config.Config, error)` where:

```go
type Config struct {
	Server   Server
	Database Database
	Auth     Auth
	AI       AI
}
type Server struct{ Port int; BaseURL string }
type Database struct{ URL string }
type Auth struct {
	Mode           string // "static" | "authelia"
	TrustedProxies []string
	Static         StaticIdentity
}
type StaticIdentity struct{ ID, DisplayName string }
type AI struct {
	Provider  string // "fake" | "anthropic"
	Anthropic Anthropic
}
type Anthropic struct{ APIKey, Model, BaseURL string }
```

Env mapping: `APP_` prefix, `_` as nesting separator — `APP_SERVER_PORT`, `APP_DATABASE_URL`, `APP_AUTH_MODE`, `APP_AUTH_STATIC_ID`, `APP_AI_PROVIDER`, `APP_AI_ANTHROPIC_APIKEY`, `APP_AI_ANTHROPIC_MODEL`, `APP_AI_ANTHROPIC_BASEURL`. Defaults: port 8080, mode static, static identity `dev/Dev Learner`, provider fake, model `claude-sonnet-5`.

- [ ] **Step 1: Write the failing tests**

`internal/config/config_test.go`:

```go
package config

import "testing"

func TestDefaults(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8080 || cfg.Auth.Mode != "static" || cfg.AI.Provider != "fake" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Auth.Static.ID != "dev" {
		t.Fatalf("static identity default: %+v", cfg.Auth.Static)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("APP_DATABASE_URL", "postgres://x")
	t.Setenv("APP_SERVER_PORT", "9999")
	t.Setenv("APP_AI_PROVIDER", "anthropic")
	t.Setenv("APP_AI_ANTHROPIC_APIKEY", "sk-test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 9999 || cfg.AI.Anthropic.APIKey != "sk-test" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"missing db url":            {},
		"bad auth mode":             {"APP_DATABASE_URL": "postgres://x", "APP_AUTH_MODE": "oauth"},
		"anthropic without key":     {"APP_DATABASE_URL": "postgres://x", "APP_AI_PROVIDER": "anthropic"},
		"unknown ai provider":       {"APP_DATABASE_URL": "postgres://x", "APP_AI_PROVIDER": "hal9000"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `make test` — Expected: FAIL, `Load` undefined.

- [ ] **Step 3: Implement `Load`**

`internal/config/config.go`:

```go
package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

// (struct definitions exactly as in the Interfaces block above)

func Load() (Config, error) {
	v := viper.New()
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.baseurl", "http://localhost:8080")
	v.SetDefault("auth.mode", "static")
	v.SetDefault("auth.trustedproxies", []string{"127.0.0.1/32", "172.16.0.0/12"})
	v.SetDefault("auth.static.id", "dev")
	v.SetDefault("auth.static.displayname", "Dev Learner")
	v.SetDefault("ai.provider", "fake")
	v.SetDefault("ai.anthropic.model", "claude-sonnet-5")
	v.SetDefault("ai.anthropic.baseurl", "https://api.anthropic.com")
	v.SetDefault("database.url", "")

	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv+Unmarshal quirk: bind each key explicitly so env vars land in structs.
	for _, key := range []string{"server.port", "server.baseurl", "database.url",
		"auth.mode", "auth.static.id", "auth.static.displayname",
		"ai.provider", "ai.anthropic.apikey", "ai.anthropic.model", "ai.anthropic.baseurl"} {
		if err := v.BindEnv(key); err != nil {
			return Config{}, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if c.Database.URL == "" {
		return fmt.Errorf("config: APP_DATABASE_URL is required")
	}
	if !slices.Contains([]string{"static", "authelia"}, c.Auth.Mode) {
		return fmt.Errorf("config: APP_AUTH_MODE must be static|authelia, got %q", c.Auth.Mode)
	}
	if !slices.Contains([]string{"fake", "anthropic"}, c.AI.Provider) {
		return fmt.Errorf("config: APP_AI_PROVIDER must be fake|anthropic, got %q", c.AI.Provider)
	}
	if c.AI.Provider == "anthropic" && c.AI.Anthropic.APIKey == "" {
		return fmt.Errorf("config: APP_AI_ANTHROPIC_APIKEY required when provider=anthropic")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("config: invalid port %d", c.Server.Port)
	}
	return nil
}
```

In `cmd/jlp/main.go`, replace the serve case:

```go
	case "serve":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		srv := httpx.NewServer(httpx.Options{Addr: fmt.Sprintf(":%d", cfg.Server.Port)})
		slog.Info("listening", "port", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
```

- [ ] **Step 4: Run tests, verify pass, verify fail-fast in the stack**

Run: `make test` — Expected: PASS.
Run: `make restart && make logs` — Expected: app boots normally (compose provides `APP_DATABASE_URL`).

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: typed viper config with APP_* env scheme and fail-fast validation"
```

---

### Task 3: Migrations (goose embedded), sqlc, postgres pool, db Make targets

**Files:**
- Create: `internal/adapters/postgres/migrationsfs/00001_identities.sql`, `db/queries/identities.sql`, `sqlc.yaml`, `internal/adapters/postgres/pool.go`, `internal/adapters/postgres/migrate.go`, `internal/adapters/postgres/identities.go`, `internal/ports/storage/identities.go`, `internal/domain/learner/identity.go`

> Migrations live under `internal/adapters/postgres/migrationsfs/` (not `db/migrations/`) because `go:embed` cannot reach outside the package directory. sqlc reads its schema from there too.
- Modify: `cmd/jlp/main.go` (add `migrate` command), `Makefile` (migrate, migrate-new, sqlc, db-shell, test-integration targets)
- Test: `internal/adapters/postgres/identities_test.go` (integration-tagged)

**Interfaces:**
- Consumes: `config.Load()`.
- Produces:
  - `learner.IdentityID` (string), `learner.Identity{ID IdentityID; DisplayName string; Attributes map[string]string}`
  - `storage.IdentityRepository interface { Upsert(ctx context.Context, id learner.Identity) error; Get(ctx context.Context, id learner.IdentityID) (learner.Identity, error) }`
  - `postgres.NewPool(ctx, url string) (*pgxpool.Pool, error)`; `postgres.Migrate(ctx, url string) error` (embedded goose, applies all up)
  - `postgres.NewIdentityRepository(pool *pgxpool.Pool) *IdentityRepository` implementing the port
  - Make targets: `migrate`, `migrate-new n=<name>`, `sqlc`, `db-shell`, `test-integration`
  - Convention for ALL later sqlc work: queries in `db/queries/<aggregate>.sql`, generated package `internal/adapters/postgres/sqlcgen`

- [ ] **Step 1: Write domain type + migration + sqlc config**

`internal/domain/learner/identity.go`:

```go
package learner

type IdentityID string

type Identity struct {
	ID          IdentityID
	DisplayName string
	Attributes  map[string]string
}
```

`internal/adapters/postgres/migrationsfs/00001_identities.sql`:

```sql
-- +goose Up
CREATE TABLE identities (
    id           text PRIMARY KEY,
    display_name text NOT NULL,
    attributes   jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE identities;
```

`db/queries/identities.sql`:

```sql
-- name: UpsertIdentity :exec
INSERT INTO identities (id, display_name, attributes)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET display_name = EXCLUDED.display_name;

-- name: GetIdentity :one
SELECT id, display_name, attributes, created_at FROM identities WHERE id = $1;
```

`sqlc.yaml`:

```yaml
version: "2"
sql:
  - engine: "postgresql"
    schema: "internal/adapters/postgres/migrationsfs"
    queries: "db/queries"
    gen:
      go:
        package: "sqlcgen"
        out: "internal/adapters/postgres/sqlcgen"
        sql_package: "pgx/v5"
```

- [ ] **Step 2: Pool, migrate, repository**

`internal/adapters/postgres/pool.go`:

```go
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func NewPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return pool, pool.Ping(ctx)
}
```

`internal/adapters/postgres/migrate.go` (goose as a library over the embedded FS; uses database/sql via pgx stdlib driver):

```go
package postgres

import (
	"context"
	"database/sql"
	"embed"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed all:migrationsfs
var migrationsFS embed.FS

func Migrate(ctx context.Context, url string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(ctx, db, "migrationsfs")
}
```

`internal/ports/storage/identities.go`:

```go
package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type IdentityRepository interface {
	Upsert(ctx context.Context, id learner.Identity) error
	Get(ctx context.Context, id learner.IdentityID) (learner.Identity, error)
}
```

`internal/adapters/postgres/identities.go`:

```go
package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type IdentityRepository struct{ q *sqlcgen.Queries }

func NewIdentityRepository(pool *pgxpool.Pool) *IdentityRepository {
	return &IdentityRepository{q: sqlcgen.New(pool)}
}

func (r *IdentityRepository) Upsert(ctx context.Context, id learner.Identity) error {
	attrs, err := json.Marshal(id.Attributes)
	if err != nil {
		return err
	}
	return r.q.UpsertIdentity(ctx, sqlcgen.UpsertIdentityParams{
		ID: string(id.ID), DisplayName: id.DisplayName, Attributes: attrs,
	})
}

func (r *IdentityRepository) Get(ctx context.Context, id learner.IdentityID) (learner.Identity, error) {
	row, err := r.q.GetIdentity(ctx, string(id))
	if err != nil {
		return learner.Identity{}, err
	}
	var attrs map[string]string
	_ = json.Unmarshal(row.Attributes, &attrs)
	return learner.Identity{ID: learner.IdentityID(row.ID), DisplayName: row.DisplayName, Attributes: attrs}, nil
}
```

Add to `cmd/jlp/main.go` switch:

```go
	case "migrate":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := postgres.Migrate(context.Background(), cfg.Database.URL); err != nil {
			slog.Error("migrate", "err", err)
			os.Exit(1)
		}
		slog.Info("migrations applied")
```

Makefile additions:

```make
migrate: ## Apply database migrations
	$(TOOLS) go run ./cmd/jlp migrate

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
```

- [ ] **Step 3: Generate and write the failing integration test**

Run: `make sqlc` — generates `internal/adapters/postgres/sqlcgen/`. Commit generated code (do NOT gitignore it).

`internal/adapters/postgres/identities_test.go`:

```go
//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

func testURL(t *testing.T) string {
	url := os.Getenv("APP_DATABASE_URL")
	if url == "" {
		t.Skip("APP_DATABASE_URL not set")
	}
	return url
}

func TestIdentityUpsertGet(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repo := NewIdentityRepository(pool)

	id := learner.Identity{ID: "test-mikey", DisplayName: "美紀", Attributes: map[string]string{"role": "learner"}}
	if err := repo.Upsert(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, id.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "美紀" {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 4: Run everything**

Run: `make tidy && make test` — Expected: PASS (integration tests excluded by build tag).
Run: `make test-integration` — Expected: PASS.
Run: `make migrate && make db-shell` then `\dt` — Expected: `identities` and `goose_db_version` tables exist. Exit psql.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: embedded goose migrations, sqlc pipeline, identity repository"
```

---

## Epic B — Identity & Auth

### Task 4: Auth port, static dev identity, identity middleware

**Files:**
- Create: `internal/ports/auth/auth.go`, `internal/adapters/staticauth/staticauth.go`, `internal/adapters/http/middleware.go`
- Modify: `internal/adapters/http/server.go` (Options grows; apply middleware; nav shows identity), `web/templates/layout.html.tmpl` (render `.Identity` in topnav), `cmd/jlp/main.go` (build pool, repos, authenticator from config)
- Test: `internal/adapters/http/middleware_test.go`

**Interfaces:**
- Consumes: `learner.Identity`, `storage.IdentityRepository`, `config.Config`.
- Produces:
  - `auth.Authenticator interface { Authenticate(r *http.Request) (learner.Identity, error) }`; `auth.ErrUnauthenticated`
  - `staticauth.New(id, displayName string) auth.Authenticator`
  - `httpx.RequireIdentity(a auth.Authenticator, repo storage.IdentityRepository) func(http.Handler) http.Handler` — authenticates, upserts the identity row once per process per identity (guarded by `sync.Map`), stores it in context
  - `httpx.IdentityFrom(ctx context.Context) (learner.Identity, bool)` — **every later handler uses this**
  - `httpx.Options` becomes `{Addr string; Auth auth.Authenticator; Identities storage.IdentityRepository}`
  - Every template's data map now includes key `"Identity"` (a `learner.Identity`); `Render` stays unchanged (handlers pass it).

- [ ] **Step 1: Write the failing middleware test**

`internal/adapters/http/middleware_test.go`:

```go
package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

type fakeAuth struct {
	id  learner.Identity
	err error
}

func (f fakeAuth) Authenticate(*http.Request) (learner.Identity, error) { return f.id, f.err }

type fakeIdentityRepo struct{ upserts int }

func (f *fakeIdentityRepo) Upsert(context.Context, learner.Identity) error { f.upserts++; return nil }
func (f *fakeIdentityRepo) Get(_ context.Context, id learner.IdentityID) (learner.Identity, error) {
	return learner.Identity{ID: id}, nil
}

func TestRequireIdentityInjectsAndUpsertsOnce(t *testing.T) {
	repo := &fakeIdentityRepo{}
	mw := RequireIdentity(fakeAuth{id: learner.Identity{ID: "dev", DisplayName: "Dev"}}, repo)
	var seen learner.Identity
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = IdentityFrom(r.Context())
	}))
	for range 3 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}
	if seen.ID != "dev" || repo.upserts != 1 {
		t.Fatalf("seen=%+v upserts=%d", seen, repo.upserts)
	}
}

func TestRequireIdentityRejects(t *testing.T) {
	mw := RequireIdentity(fakeAuth{err: errors.New("nope")}, &fakeIdentityRepo{})
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}
```

Run: `make test` — Expected: FAIL (undefined: RequireIdentity).

- [ ] **Step 2: Implement port, adapter, middleware**

`internal/ports/auth/auth.go`:

```go
package auth

import (
	"errors"
	"net/http"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type Authenticator interface {
	Authenticate(r *http.Request) (learner.Identity, error)
}
```

`internal/adapters/staticauth/staticauth.go`:

```go
package staticauth

import (
	"net/http"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

type Static struct{ identity learner.Identity }

func New(id, displayName string) auth.Authenticator {
	return Static{identity: learner.Identity{ID: learner.IdentityID(id), DisplayName: displayName}}
}

func (s Static) Authenticate(*http.Request) (learner.Identity, error) { return s.identity, nil }
```

`internal/adapters/http/middleware.go`:

```go
package httpx

import (
	"context"
	"net/http"
	"sync"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type ctxKey int

const identityKey ctxKey = 1

func IdentityFrom(ctx context.Context) (learner.Identity, bool) {
	id, ok := ctx.Value(identityKey).(learner.Identity)
	return id, ok
}

func RequireIdentity(a auth.Authenticator, repo storage.IdentityRepository) func(http.Handler) http.Handler {
	var seen sync.Map
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if _, done := seen.LoadOrStore(id.ID, true); !done {
				if err := repo.Upsert(r.Context(), id); err != nil {
					http.Error(w, "identity error", http.StatusInternalServerError)
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
		})
	}
}
```

Wire in `server.go`: `Options{Addr string; Auth auth.Authenticator; Identities storage.IdentityRepository}`; in `routes()` wrap everything except `/healthz` and `/static/*` in a chi `r.Group` using `r.Use(RequireIdentity(opts.Auth, opts.Identities))`. Home handler passes `"Identity": ident` into `Render`. Layout topnav: `<nav id="topnav">{{with .Identity}}<span class="who">{{.DisplayName}}</span>{{end}}</nav>`.

In `cmd/jlp/main.go` serve case: `pool := postgres.NewPool(...)`, `identities := postgres.NewIdentityRepository(pool)`, `authn := staticauth.New(cfg.Auth.Static.ID, cfg.Auth.Static.DisplayName)` (only mode `static` exists until Task 5 — if mode is `authelia`, exit with "not yet supported" error for now).

- [ ] **Step 3: Run tests**

Run: `make test` — Expected: PASS. Fix the Task 1 server tests if the new Options shape broke them (pass `staticauth.New("dev","Dev Learner")` and the fake repo from the middleware test — export nothing new for this; construct fakes in each test file as needed).

- [ ] **Step 4: Browser verification**

`make up`, navigate `http://localhost:8080/` — topbar shows **Dev Learner**; `/healthz` still works without auth; no console errors.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: auth port, static dev identity, identity-scoped request middleware"
```

---

### Task 5: Authelia + Caddy login flow (authelia auth adapter)

**Files:**
- Create: `internal/adapters/authelia/authelia.go`, `deploy/caddy/Caddyfile.dev`, `deploy/authelia/configuration.dev.yml`, `scripts/gen-authelia-users.sh`
- Modify: `docker-compose.yml` (add `caddy`, `authelia` services under profile `auth`), `Makefile` (`init` generates users file; `up-auth` target), `cmd/jlp/main.go` (mode `authelia` wires the adapter), `.env.example` (document `AUTHELIA_DEV_PASSWORD`)
- Test: `internal/adapters/authelia/authelia_test.go`

**Interfaces:**
- Consumes: `auth.Authenticator`, `config.Auth.TrustedProxies`.
- Produces: `authelia.New(trustedCIDRs []string) (auth.Authenticator, error)` — reads `Remote-User` (identity ID) and `Remote-Name` (display name) headers, but ONLY when the direct peer (`r.RemoteAddr`) is inside a trusted CIDR; otherwise returns `auth.ErrUnauthenticated`. Make targets: `up-auth` (stack including caddy+authelia).

- [ ] **Step 1: Write the failing adapter test**

`internal/adapters/authelia/authelia_test.go`:

```go
package authelia

import (
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyHeaderAuth(t *testing.T) {
	a, err := New([]string{"172.16.0.0/12"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.5:44321"
	r.Header.Set("Remote-User", "mikey")
	r.Header.Set("Remote-Name", "Mikey Austin")
	id, err := a.Authenticate(r)
	if err != nil || string(id.ID) != "mikey" || id.DisplayName != "Mikey Austin" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
}

func TestUntrustedPeerRejected(t *testing.T) {
	a, _ := New([]string{"172.16.0.0/12"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:1234" // spoofed header from outside
	r.Header.Set("Remote-User", "mallory")
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestMissingHeaderRejected(t *testing.T) {
	a, _ := New([]string{"172.16.0.0/12"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.5:44321"
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("expected rejection")
	}
}
```

Run: `make test` — Expected: FAIL (undefined: New).

- [ ] **Step 2: Implement the adapter**

`internal/adapters/authelia/authelia.go`:

```go
package authelia

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

type Authelia struct{ trusted []netip.Prefix }

func New(cidrs []string) (auth.Authenticator, error) {
	a := Authelia{}
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("authelia: bad trusted proxy %q: %w", c, err)
		}
		a.trusted = append(a.trusted, p)
	}
	return a, nil
}

func (a Authelia) Authenticate(r *http.Request) (learner.Identity, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return learner.Identity{}, auth.ErrUnauthenticated
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return learner.Identity{}, auth.ErrUnauthenticated
	}
	ok := false
	for _, p := range a.trusted {
		if p.Contains(addr) {
			ok = true
			break
		}
	}
	user := r.Header.Get("Remote-User")
	if !ok || user == "" {
		return learner.Identity{}, auth.ErrUnauthenticated
	}
	name := r.Header.Get("Remote-Name")
	if name == "" {
		name = user
	}
	return learner.Identity{ID: learner.IdentityID(user), DisplayName: name}, nil
}
```

In `cmd/jlp/main.go`, replace the "not yet supported" branch: `authn, err = authelia.New(cfg.Auth.TrustedProxies)`.

**Important:** the `RealIP` chi middleware rewrites nothing relevant here — the adapter uses `r.RemoteAddr` (the TCP peer), which inside compose is Caddy's container IP (in `172.16.0.0/12` by default). Do not use `X-Forwarded-For` for the trust decision.

- [ ] **Step 3: Compose + Caddy + Authelia config**

Add to `docker-compose.yml` (both under `profiles: ["auth"]`):

```yaml
  caddy:
    image: caddy:2
    profiles: ["auth"]
    ports: ["8443:8443"]
    volumes:
      - ./deploy/caddy/Caddyfile.dev:/etc/caddy/Caddyfile:ro
      - caddydata:/data
    depends_on: [app, authelia]

  authelia:
    image: authelia/authelia:4
    profiles: ["auth"]
    volumes:
      - ./deploy/authelia/configuration.dev.yml:/config/configuration.yml:ro
      - ./deploy/authelia/users.dev.yml:/config/users.yml:ro
      - autheliadata:/data
```

(add `caddydata: {}` and `autheliadata: {}` to the volumes map)

`deploy/caddy/Caddyfile.dev`:

```
{
	local_certs
}

jlp.localhost:8443 {
	forward_auth authelia:9091 {
		uri /api/authz/forward-auth
		copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
	}
	reverse_proxy app:8080
}

auth.jlp.localhost:8443 {
	reverse_proxy authelia:9091
}
```

`deploy/authelia/configuration.dev.yml` (dev-only static secrets are deliberate; production gets real ones in Task 18):

```yaml
theme: light
server:
  address: 'tcp://0.0.0.0:9091'
log:
  level: info
identity_validation:
  reset_password:
    jwt_secret: 'insecure-dev-jwt-secret-please-never-in-prod'
authentication_backend:
  file:
    path: '/config/users.yml'
access_control:
  default_policy: one_factor
session:
  secret: 'insecure-dev-session-secret-please-never-in-prod'
  cookies:
    - domain: 'jlp.localhost'
      authelia_url: 'https://auth.jlp.localhost:8443'
      default_redirection_url: 'https://jlp.localhost:8443'
storage:
  encryption_key: 'insecure-dev-encryption-key-32chars-x'
  local:
    path: '/data/db.sqlite3'
notifier:
  filesystem:
    filename: '/data/notification.txt'
```

> If the Authelia container rejects this schema on boot (`make logs s=authelia`), align key names with the docs for the running 4.x version — the intent is fixed: file user backend, one_factor everywhere, session cookie on `jlp.localhost`, sqlite storage.

`scripts/gen-authelia-users.sh` (chmod +x):

```bash
#!/usr/bin/env sh
# Generates deploy/authelia/users.dev.yml with a hashed dev password.
set -eu
PASSWORD="${AUTHELIA_DEV_PASSWORD:-devpassword}"
HASH=$(docker run --rm authelia/authelia:4 authelia crypto hash generate argon2 \
  --password "$PASSWORD" | sed 's/^Digest: //')
cat > deploy/authelia/users.dev.yml <<EOF
users:
  mikey:
    displayname: "Mikey Austin"
    password: "$HASH"
    email: mikey@jlp.localhost
    groups: [learners]
EOF
echo "wrote deploy/authelia/users.dev.yml"
```

Makefile: `init` additionally runs `sh scripts/gen-authelia-users.sh` (sourcing `.env` first: `set -a; . ./.env; set +a;`), and:

```make
up-auth: ## Start dev stack including Caddy + Authelia (https://jlp.localhost:8443)
	APP_AUTH_MODE=authelia $(COMPOSE) --profile auth up -d
```

Add `deploy/authelia/users.dev.yml` to `.gitignore`.

- [ ] **Step 4: Run tests**

Run: `make test` — Expected: PASS.

- [ ] **Step 5: Browser verification (full login flow)**

1. `make init && make up-auth`; `make logs s=authelia` until "Listening".
2. Navigate `https://jlp.localhost:8443` — Chrome shows a self-signed-cert warning: proceed via Advanced → Continue.
3. Expect redirect to the Authelia login form at `auth.jlp.localhost:8443`. Fill username `mikey`, password from `.env` (`AUTHELIA_DEV_PASSWORD`, default `devpassword`), submit.
4. Expect redirect back to the app; topbar shows **Mikey Austin**.
5. Also confirm `http://localhost:8080` (static mode fallback is NOT active here since APP_AUTH_MODE=authelia): direct app access without the proxy must show **401 unauthorized** — this proves header auth can't be bypassed.
6. `make down` then `make up` to return to static mode for later tasks.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "feat: Authelia forward-auth adapter behind Caddy with trusted-proxy enforcement"
```

---

## Epic C — Sessions & Writing

### Task 6: Session domain, repository, use cases, sessions UI

**Files:**
- Create: `internal/domain/session/session.go`, `internal/ports/storage/sessions.go`, `internal/adapters/postgres/migrationsfs/00002_sessions.sql`, `db/queries/sessions.sql`, `internal/adapters/postgres/sessions.go`, `internal/application/sessions/service.go`, `internal/adapters/http/sessions.go`, `web/templates/sessions.html.tmpl`, `web/templates/workspace.html.tmpl`, `web/static/js/htmx.min.js` (vendored), `web/static/js/alpine.min.js` (vendored)
- Modify: `internal/adapters/http/server.go` (routes + Options gains `Sessions *sessions.Service`), `web/templates/layout.html.tmpl` (script tags + nav link), `Makefile` (`vendor-js` target)
- Test: `internal/application/sessions/service_test.go`, `internal/adapters/postgres/sessions_test.go` (integration)

**Interfaces:**
- Consumes: `learner.IdentityID`, `httpx.IdentityFrom`, sqlc conventions from Task 3.
- Produces:

```go
// domain/session
type ID string
type Session struct {
	ID         ID
	IdentityID learner.IdentityID
	Title      string
	Purpose    string
	Profile    Profile
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
type Profile struct {
	Audience            string
	Tone                string
	Register            string // "casual" | "polite" | "formal" | ""
	TeacherMode         string // "teacher" | "strict-corrector" | "naturalness-coach" | "socratic"
	ExplanationLanguage string // "ja" | "en" | "both"
	Strictness          string // "lenient" | "balanced" | "strict"
}

// ports/storage/sessions.go
type SessionRepository interface {
	Create(ctx context.Context, s session.Session) error
	Get(ctx context.Context, identity learner.IdentityID, id session.ID) (session.Session, error)
	List(ctx context.Context, identity learner.IdentityID) ([]session.Session, error)
}

// application/sessions
func NewService(repo storage.SessionRepository) *Service
func (s *Service) Create(ctx context.Context, identity learner.IdentityID, title, purpose string, p session.Profile) (session.Session, error) // validates title non-empty; defaults TeacherMode "teacher", ExplanationLanguage "both", Strictness "balanced"; generates uuid
func (s *Service) List(ctx context.Context, identity learner.IdentityID) ([]session.Session, error)
func (s *Service) Get(ctx context.Context, identity learner.IdentityID, id session.ID) (session.Session, error)
```

- Routes: `GET /sessions` (page: list + create form), `POST /sessions` (form post → 303 redirect to `/sessions/{id}`), `GET /sessions/{id}` (workspace page — 3-pane layout with placeholder editor pane until Task 7).
- `make vendor-js` downloads pinned htmx 2.x and alpine 3.x into `web/static/js/` (curl in tools container); files are committed.

- [ ] **Step 1: Failing application-service test** — `internal/application/sessions/service_test.go` with an in-memory `fakeSessionRepo` (map keyed by `identity+"/"+id`, mirrors the port); cases: create defaults applied (`TeacherMode=="teacher"`, uuid assigned), empty-title error, `List` returns only the caller's identity's sessions (create under two identities, list one), `Get` for another identity returns `storage.ErrNotFound` (add `var ErrNotFound = errors.New("not found")` to a new `internal/ports/storage/errors.go`). Run `make test` → FAIL.
- [ ] **Step 2: Implement domain + service** until `make test` passes.
- [ ] **Step 3: Migration + sqlc + postgres repo.** `00002_sessions.sql`:

```sql
-- +goose Up
CREATE TABLE sessions (
    id          uuid PRIMARY KEY,
    identity_id text NOT NULL REFERENCES identities(id),
    title       text NOT NULL,
    purpose     text NOT NULL DEFAULT '',
    profile     jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_identity_idx ON sessions (identity_id, updated_at DESC);

-- +goose Down
DROP TABLE sessions;
```

`db/queries/sessions.sql` — `CreateSession :exec` (insert all fields), `GetSession :one` (`WHERE id = $1 AND identity_id = $2`), `ListSessions :many` (`WHERE identity_id = $1 ORDER BY updated_at DESC`). Run `make sqlc`. Postgres repo marshals `Profile` to jsonb; `Get` maps `pgx.ErrNoRows` → `storage.ErrNotFound`. Integration test mirrors the fake-repo cases (create/get/list/cross-identity). `make test-integration` → PASS.
- [ ] **Step 4: HTTP + templates.** `vendor-js` Makefile target:

```make
vendor-js: ## Vendor pinned htmx + alpine into web/static/js
	$(TOOLS) sh -c "curl -fsSL https://unpkg.com/htmx.org@2/dist/htmx.min.js -o web/static/js/htmx.min.js && \
	                curl -fsSL https://unpkg.com/alpinejs@3/dist/cdn.min.js -o web/static/js/alpine.min.js"
```

Layout gains `<script src="/static/js/htmx.min.js" defer></script><script src="/static/js/alpine.min.js" defer></script>` and a `セッション` nav link. `sessions.html.tmpl`: table of sessions (title → link, purpose, updated) + a create form (fields: title, purpose select with the PRD §6.1 values, teacher-mode select, explanation-language select, strictness select) posting to `/sessions`. `workspace.html.tmpl`: `<div class="workspace"><section id="editor-pane">（エディタは次のタスクで）</section><aside id="context-pane">` showing title/purpose/profile `</aside><section id="feedback-pane"></section></div>` with CSS grid `grid-template-columns: 1fr 300px;` and feedback pane full-width below editor. Handlers in `internal/adapters/http/sessions.go` use `IdentityFrom` + `Options.Sessions`; handler test asserts POST then GET list contains the created title.
- [ ] **Step 5: Browser verification.** `make up && make migrate`. In Chrome: open `/sessions`, create session titled `旅行について書く` with purpose `Blog post`, expect redirect to workspace showing the 3 panes and the profile in the context pane; back to `/sessions`, the session is listed. Console clean.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: sessions domain, identity-scoped repository, sessions + workspace UI"`

---

### Task 7: Documents, autosave, versions (the writing editor)

**Files:**
- Create: `internal/domain/writing/document.go`, `internal/ports/storage/documents.go`, `internal/adapters/postgres/migrationsfs/00003_documents.sql`, `db/queries/documents.sql`, `internal/adapters/postgres/documents.go`, `internal/application/writing/service.go`, `internal/adapters/http/documents.go`, `web/static/js/app.js`
- Modify: `web/templates/workspace.html.tmpl` (real editor pane), `internal/adapters/http/server.go` (route + `Options.Writing *writing.Service` — note the import alias `appwriting "…/application/writing"` to avoid clashing with `domain/writing`), `internal/adapters/http/sessions.go` (workspace handler loads document)
- Test: `internal/domain/writing/document_test.go`, `internal/application/writing/service_test.go`, `internal/adapters/postgres/documents_test.go` (integration)

**Interfaces:**
- Consumes: session/service types from Task 6.
- Produces:

```go
// domain/writing
type DocumentID string
type Document struct {
	ID         DocumentID
	SessionID  session.ID
	IdentityID learner.IdentityID
	Content    string
	Version    int
	UpdatedAt  time.Time
}
func (d Document) RuneCount() int // utf8.RuneCountInString(d.Content)

// ports/storage/documents.go
type DocumentRepository interface {
	GetOrCreateForSession(ctx context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error)
	Save(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) // increments Version and appends a document_versions row in ONE transaction
	Get(ctx context.Context, identity learner.IdentityID, id writing.DocumentID) (writing.Document, error)
	ListVersions(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, limit int) ([]writing.Document, error)
}

// application/writing
func NewService(docs storage.DocumentRepository) *Service
func (s *Service) Open(ctx context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error)
func (s *Service) Autosave(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error)
```

- Route: `POST /documents/{id}` with form field `content` → JSON `{"version":N,"saved_at":"…","runes":N}`. (Task 8 adds event recording to Autosave.)
- `web/static/js/app.js` exposes the autosave wiring used by the workspace template (see Step 4 — later tasks reuse `window.jlp.runeOffset`).

- [ ] **Step 1: Failing domain + service tests.** `document_test.go`: `RuneCount` of `"昨日、映画を見た。"` == 9 (NOT byte length 27). `service_test.go` with fake repo: `Open` creates once then returns same doc; `Autosave` bumps version; cross-identity `Autosave` returns `storage.ErrNotFound`. `make test` → FAIL, then implement, then PASS.
- [ ] **Step 2: Migration `00003_documents.sql`:**

```sql
-- +goose Up
CREATE TABLE documents (
    id          uuid PRIMARY KEY,
    session_id  uuid NOT NULL REFERENCES sessions(id),
    identity_id text NOT NULL REFERENCES identities(id),
    content     text NOT NULL DEFAULT '',
    version     int  NOT NULL DEFAULT 1,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX documents_session_idx ON documents (session_id);

CREATE TABLE document_versions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    document_id uuid NOT NULL REFERENCES documents(id),
    version     int NOT NULL,
    content     text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE document_versions;
DROP TABLE documents;
```

Queries: `GetDocumentBySession :one`, `InsertDocument :one`, `GetDocument :one` (id+identity), `UpdateDocumentContent :one` (`SET content=$3, version=version+1, updated_at=now() WHERE id=$1 AND identity_id=$2 RETURNING *`), `InsertDocumentVersion :exec`, `ListDocumentVersions :many`. `Save` uses `pool.Begin(ctx)` + `sqlcgen.New(tx)` (sqlc's `WithTx`), commits both writes atomically. Integration test: a fresh document starts at version 1; two saves leave it at version 3 with two rows in `document_versions`. `make sqlc && make test-integration` → PASS.
- [ ] **Step 3: HTTP handler + workspace editor.** Editor pane in `workspace.html.tmpl`:

```html
<section id="editor-pane">
  <textarea id="editor" lang="ja" spellcheck="false" data-doc-id="{{.Document.ID}}"
            placeholder="ここに日本語で書いてください…">{{.Document.Content}}</textarea>
  <div class="editor-status"><span id="save-state" data-state="saved">保存済み</span>
       <span id="rune-count">{{.Document.RuneCount}}</span>字</div>
</section>
<script src="/static/js/app.js" defer></script>
```

`web/static/js/app.js`:

```js
window.jlp = window.jlp || {};
// Convert a UTF-16 index (textarea selectionStart/End) to a rune offset.
window.jlp.runeOffset = (text, utf16Index) => Array.from(text.slice(0, utf16Index)).length;

(function () {
  const ta = document.getElementById("editor");
  if (!ta) return;
  const state = document.getElementById("save-state");
  const runes = document.getElementById("rune-count");
  let timer = null, composing = false;
  ta.addEventListener("compositionstart", () => { composing = true; });
  ta.addEventListener("compositionend", () => { composing = false; queue(); });
  ta.addEventListener("input", () => { if (!composing) queue(); });
  function queue() {
    state.dataset.state = "dirty"; state.textContent = "未保存";
    clearTimeout(timer); timer = setTimeout(save, 1500);
  }
  async function save() {
    const body = new URLSearchParams({ content: ta.value });
    const res = await fetch(`/documents/${ta.dataset.docId}`, { method: "POST", body });
    if (!res.ok) { state.dataset.state = "error"; state.textContent = "保存失敗"; return; }
    const j = await res.json();
    state.dataset.state = "saved"; state.textContent = "保存済み";
    runes.textContent = j.runes;
  }
})();
```

(IME safety: no save is queued mid-composition, so half-composed kana are never persisted as a version.) Workspace handler (sessions.go) calls `Options.Writing.Open` and passes `"Document"` to the template. CSS: `#editor { width:100%; min-height:60vh; font-size:1.1rem; line-height:1.9; }`.
- [ ] **Step 4: Browser verification.** `make up && make migrate`. Open the Task 6 session's workspace. Type `昨日友達と映画を見に行って、とても面白いでした。` (use javascript_tool to set the textarea value + dispatch an `input` event — CDP typing doesn't exercise the IME, that's fine). Expect: 保存済み within ~2s, rune count = 24. Reload page → text persists. `make db-shell`: `select version from documents;` ≥ 2, `select count(*) from document_versions;` ≥ 1. Console clean.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: writing editor with IME-safe autosave and document versions"`

---

## Epic D — Learning Events

### Task 8: Learning events, in-process event bus, activity feed

**Files:**
- Create: `internal/domain/event/event.go`, `internal/ports/events/bus.go`, `internal/ports/storage/learningevents.go`, `internal/adapters/inprocbus/bus.go`, `internal/adapters/postgres/migrationsfs/00004_learning_events.sql`, `db/queries/learning_events.sql`, `internal/adapters/postgres/learningevents.go`, `internal/application/learning/recorder.go`, `web/templates/partials/activity.html.tmpl`
- Modify: `internal/application/writing/service.go` (Autosave + first-Open record events), `internal/adapters/http/sessions.go` (activity partial route), `web/templates/workspace.html.tmpl` (activity list in context pane), `internal/adapters/http/server.go`, `cmd/jlp/main.go` (wire bus + recorder)
- Test: `internal/adapters/inprocbus/bus_test.go`, `internal/application/learning/recorder_test.go`, updated `internal/application/writing/service_test.go`

**Interfaces:**
- Consumes: learner/session types; `storage.ErrNotFound` convention.
- Produces:

```go
// domain/event
type Type string
const (
	TypeWritingCreated       Type = "writing.created"
	TypeWritingUpdated       Type = "writing.updated"
	TypeFeedbackRequested    Type = "feedback.requested"
	TypeCorrectionPresented  Type = "correction.presented"
	TypeCorrectionAccepted   Type = "correction.accepted"
	TypeCorrectionRejected   Type = "correction.rejected"
)
type LearningEvent struct {
	ID         string
	IdentityID learner.IdentityID
	SessionID  *session.ID           // nil for session-less events
	Type       Type
	Subject    string                // e.g. document ID, correction ID
	Evidence   map[string]any        // stored as jsonb
	OccurredAt time.Time
}

// ports/events
type Handler func(ctx context.Context, ev event.LearningEvent) error
type EventBus interface {
	Publish(ctx context.Context, ev event.LearningEvent) error // sync dispatch; joins handler errors
	Subscribe(t event.Type, h Handler)
}

// ports/storage
type LearningEventRepository interface {
	Append(ctx context.Context, ev event.LearningEvent) error
	ListRecent(ctx context.Context, identity learner.IdentityID, sid *session.ID, limit int) ([]event.LearningEvent, error)
}

// application/learning — EVERY later producer of events goes through this:
func NewRecorder(store storage.LearningEventRepository, bus events.EventBus) *Recorder
func (r *Recorder) Record(ctx context.Context, ev event.LearningEvent) error // fills ID/OccurredAt if zero; Append THEN Publish
```

- `writing.NewService` signature changes to `NewService(docs storage.DocumentRepository, rec *learning.Recorder)`; `Open` records `writing.created` (only when it created), `Autosave` records `writing.updated` with `Evidence{"rune_count": doc.RuneCount(), "version": doc.Version}`.
- Route: `GET /sessions/{id}/activity` → `partials/activity` (last 10 events, newest first); context pane includes `<div id="activity" hx-get="/sessions/{{.Session.ID}}/activity" hx-trigger="load, every 30s"></div>`.

- [ ] **Step 1: Failing bus + recorder tests.** `bus_test.go`: subscribe two handlers to `event.TypeWritingUpdated`, publish once → both called with the event; handler error surfaces from `Publish`; publishing a type with no subscribers is a no-op returning nil. `recorder_test.go` (fake store + fake bus): `Record` fills ID and OccurredAt, appends before publishing (assert ordering with a shared slice), propagates store errors without publishing. `make test` → FAIL.
- [ ] **Step 2: Implement.** `inprocbus.New() events.EventBus` — `map[event.Type][]events.Handler` + `sync.RWMutex`; `Publish` iterates a copy, `errors.Join`s handler errors. Recorder per the interface block. `make test` → PASS.
- [ ] **Step 3: Migration + repo.** `00004_learning_events.sql`:

```sql
-- +goose Up
CREATE TABLE learning_events (
    id          uuid PRIMARY KEY,
    identity_id text NOT NULL REFERENCES identities(id),
    session_id  uuid REFERENCES sessions(id),
    type        text NOT NULL,
    subject     text NOT NULL DEFAULT '',
    evidence    jsonb NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX learning_events_identity_time_idx ON learning_events (identity_id, occurred_at DESC);
CREATE INDEX learning_events_type_idx ON learning_events (identity_id, type);

-- +goose Down
DROP TABLE learning_events;
```

Queries `AppendLearningEvent :exec`, `ListRecentLearningEvents :many` (`WHERE identity_id=$1 AND ($2::uuid IS NULL OR session_id=$2) ORDER BY occurred_at DESC LIMIT $3`). `make sqlc`; integration test appends + lists scoped by identity. `make test-integration` → PASS.
- [ ] **Step 4: Wire writing events + activity partial.** Update writing service tests first (fake recorder counts events; Open-that-creates records `writing.created`; Autosave records `writing.updated` with rune_count evidence) → FAIL → implement → PASS. Activity partial renders `{{range .Events}}<li><span class="ev-type">{{.Type}}</span> <time>{{.OccurredAt.Format "15:04"}}</time></li>{{end}}`; handler uses `LearningEventRepository.ListRecent` via a small `Options.Events storage.LearningEventRepository` field. Add to `render.go` the partial helper that later tasks (13–15) also use:

```go
// RenderPartial executes one named template from web/templates/partials/*.
func RenderPartial(w http.ResponseWriter, r *http.Request, name string, data any) {
	t, err := template.ParseGlob("web/templates/partials/*.html.tmpl")
	if err != nil {
		slog.Error("partial parse", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("partial exec", "name", name, "err", err)
	}
}
```

(every partial file wraps its content in `{{define "<name>"}}…{{end}}`; the activity partial defines `"activity"`)
- [ ] **Step 5: Browser verification.** `make up && make migrate`. Open workspace, type text, wait for autosave, then wait for/force the activity refresh — the list shows `writing.updated` entries. `make db-shell`: `select type, evidence from learning_events order by occurred_at desc limit 5;` shows rune_count evidence. Console clean.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: immutable learning events with in-process bus and session activity feed"`

---

## Epic E — AI Capability & Corrections

### Task 9: AI port, prompt registry, JSON Schema registry, fake adapter, observability decorator

**Files:**
- Create: `internal/ports/ai/ai.go`, `internal/prompts/prompts.go`, `internal/prompts/templates/teacher.feedback.v1.system.md`, `internal/prompts/templates/teacher.feedback.v1.user.md`, `internal/schemas/schemas.go`, `internal/schemas/defs/correction_result.v1.json`, `internal/adapters/fakeai/fakeai.go`, `internal/observability/airequests.go`, `internal/ports/storage/airequests.go`, `internal/adapters/postgres/migrationsfs/00005_ai_requests.sql`, `db/queries/ai_requests.sql`, `internal/adapters/postgres/airequests.go`
- Test: `internal/prompts/prompts_test.go`, `internal/schemas/schemas_test.go`, `internal/adapters/fakeai/fakeai_test.go`, `internal/observability/airequests_test.go`

**Interfaces:**
- Consumes: learner/session types, Recorder conventions.
- Produces (used by Tasks 11–13):

```go
// ports/ai
type StructuredRequest struct {
	PromptName    string // "teacher.feedback"
	PromptVersion string // "v1"
	System, User  string // fully rendered prompt text
	SchemaName    string // "correction_result.v1"
	Schema        json.RawMessage
	MaxTokens     int
	IdentityID    learner.IdentityID
	SessionID     *session.ID
	Agent         string // "teacher"
}
type StructuredResponse struct {
	RequestID     string // set by the observability decorator == ai_requests.id
	JSON          json.RawMessage
	Provider      string
	Model         string
	InputTokens   int
	OutputTokens  int
	Latency       time.Duration
}
type StructuredGenerator interface {
	GenerateStructured(ctx context.Context, req StructuredRequest) (StructuredResponse, error)
}

// prompts
type Prompt struct{ System, User string }
func Render(name, version string, data any) (Prompt, error) // text/template over embedded templates/<name>.<version>.{system,user}.md

// schemas
func Get(name string) (json.RawMessage, error)          // embedded defs/<name>.json
func Validate(name string, doc []byte) error            // santhosh-tekuri/jsonschema/v6, compiled once

// adapters/fakeai
func New() ai.StructuredGenerator // deterministic; see Step 4

// observability
func NewAIObserver(inner ai.StructuredGenerator, repo storage.AIRequestRepository, pricing map[string]ModelPricing) ai.StructuredGenerator
type ModelPricing struct{ InPerMTok, OutPerMTok float64 }

// ports/storage
type AIRequestRecord struct {
	ID, Capability, Provider, Model, PromptName, PromptVersion string
	IdentityID    learner.IdentityID
	SessionID     *session.ID
	LatencyMS     int
	InputTokens   int
	OutputTokens  int
	CostUSD       float64
	Success       bool
	Error         string
	CreatedAt     time.Time
}
type AIRequestRepository interface {
	Insert(ctx context.Context, rec AIRequestRecord) error
	List(ctx context.Context, identity learner.IdentityID, limit int) ([]AIRequestRecord, error)
}
```

- [ ] **Step 1: Write the prompt templates and schema (data, then code).**

`internal/schemas/defs/correction_result.v1.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "CorrectionResult",
  "type": "object",
  "additionalProperties": false,
  "required": ["corrections"],
  "properties": {
    "corrections": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["original", "replacement", "type", "severity", "explanation"],
        "properties": {
          "original":    { "type": "string", "minLength": 1 },
          "replacement": { "type": "string" },
          "type": { "enum": ["grammar","particle","conjugation","word-order","vocabulary","collocation","register","politeness","naturalness","ambiguity","punctuation","spelling","kanji","style"] },
          "severity": { "enum": ["incorrect","unnatural","less-natural","style","optional","excellent-alternative"] },
          "explanation": {
            "type": "object",
            "additionalProperties": false,
            "required": ["ja", "en"],
            "properties": { "ja": { "type": "string" }, "en": { "type": "string" } }
          },
          "concepts": { "type": "array", "items": { "type": "string" } }
        }
      }
    }
  }
}
```

`internal/prompts/templates/teacher.feedback.v1.system.md`:

```
You are a Japanese teacher reviewing a learner's writing. Your goal is teaching, not
polishing: identify only genuine problems, classify each precisely, and explain so the
learner can generalize. Never invent errors — if the selection is natural, return zero
corrections. Distinguish carefully between severities:
incorrect (grammatically wrong), unnatural (grammatical but no native would say it),
less-natural (fine but a better option exists), style (register/tone mismatch for the
session's purpose), optional (a possible refinement), excellent-alternative (the
learner's phrasing is good; you offer an interesting variant).
Each correction's "original" MUST be an exact contiguous substring of the selection.
Keep corrections minimal and non-overlapping. Explanations: "ja" in simple Japanese
(learner level: intermediate), "en" in English. Teacher mode: {{.TeacherMode}}.
Strictness: {{.Strictness}}. Explanation language preference: {{.ExplanationLanguage}}.
```

`internal/prompts/templates/teacher.feedback.v1.user.md`:

```
Session purpose: {{.Purpose}}{{if .Audience}} / Audience: {{.Audience}}{{end}}{{if .Register}} / Register: {{.Register}}{{end}}

Surrounding context:
{{.Context}}

Selection to review:
{{.Selection}}
{{if .RecentErrors}}
The learner's recent recurring problem areas (weigh these when deciding severity):
{{range .RecentErrors}}- {{.}}
{{end}}{{end}}
```

- [ ] **Step 2: Failing tests for prompts + schemas.** `prompts_test.go`: `Render("teacher.feedback","v1", data)` with a struct containing all fields renders both parts, includes the selection string, errors on unknown name/version. `schemas_test.go`: `Validate("correction_result.v1", …)` passes on a good doc, fails on missing `severity`, fails on unknown `type` enum value; `Get` returns non-empty schema. `make test` → FAIL, implement `prompts.go` (embed + `text/template` with `template.Option("missingkey=error")`) and `schemas.go` (embed + compile cache guarded by `sync.Once`), → PASS.
- [ ] **Step 3: Failing observability test.** Fake inner generator returns a canned response after 5ms; fake repo captures the record. Assert: RequestID non-empty and equal to the inserted record's ID, latency ≥ 0, cost computed from pricing map (e.g. 1000 in / 500 out tokens at {3.0, 15.0} per MTok → 0.0105), `Success=false` + `Error` set when inner errors (record still inserted). Implement decorator: wraps call, measures latency, inserts record (log-only on insert failure — observability must not break the feature), stamps `RequestID` into the response. → PASS.
- [ ] **Step 4: Fake adapter.** Deterministic rules on `req.User` (the rendered prompt contains the selection): if it contains `面白いでした` → one correction `{"original":"面白いでした","replacement":"面白かったです","type":"conjugation","severity":"incorrect","explanation":{"ja":"い形容詞の過去形は「〜かった」を使います。「面白い」→「面白かった」。","en":"い-adjectives form the past tense with 〜かった, so 面白いでした must be 面白かったです."},"concepts":["i-adjective-past"]}`; same pattern for `楽しいでした`→`楽しかったです`; if it contains `を行きます` → particle correction `を`→`に` (`"type":"particle","severity":"incorrect"`); otherwise `{"corrections":[]}`. Provider `"fake"`, model `"fake-1"`, tokens = rune counts of prompts. Test: response JSON passes `schemas.Validate("correction_result.v1", …)` for each rule. → PASS.
- [ ] **Step 5: Migration + repo + wiring.** `00005_ai_requests.sql`:

```sql
-- +goose Up
CREATE TABLE ai_requests (
    id             uuid PRIMARY KEY,
    identity_id    text NOT NULL REFERENCES identities(id),
    session_id     uuid REFERENCES sessions(id),
    capability     text NOT NULL DEFAULT 'structured-generation',
    provider       text NOT NULL,
    model          text NOT NULL,
    prompt_name    text NOT NULL,
    prompt_version text NOT NULL,
    latency_ms     int NOT NULL,
    input_tokens   int NOT NULL DEFAULT 0,
    output_tokens  int NOT NULL DEFAULT 0,
    cost_usd       numeric(10,6) NOT NULL DEFAULT 0,
    success        boolean NOT NULL,
    error          text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_requests_identity_idx ON ai_requests (identity_id, created_at DESC);

-- +goose Down
DROP TABLE ai_requests;
```

sqlc queries `InsertAIRequest :exec`, `ListAIRequests :many`; postgres repo; integration test insert+list. In `cmd/jlp/main.go`: build `gen ai.StructuredGenerator` = fakeai (provider `fake`) — Anthropic lands in Task 11 — always wrapped in `observability.NewAIObserver(gen, aiRepo, pricing)` where pricing comes from a `config` default map (`claude-sonnet-5: {3, 15}`, `fake-1: {0, 0}`). `make test-integration` → PASS.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: AI capability port with prompt/schema registries, fake provider, observability decorator"`

---

### Task 10: Correction domain and deterministic rune diff

**Files:**
- Create: `internal/domain/correction/correction.go`, `internal/domain/diff/diff.go`
- Test: `internal/domain/correction/correction_test.go`, `internal/domain/diff/diff_test.go`

**Interfaces:**
- Consumes: nothing outside stdlib + sergi/go-diff.
- Produces:

```go
// domain/diff
type Op int
const ( OpEqual Op = iota; OpInsert; OpDelete )
type Segment struct { Op Op; Text string }
func Runes(a, b string) []Segment // diff-match-patch + semantic cleanup, rune-safe

// domain/correction
type Type string     // same enum values as the JSON schema
type Severity string // same enum values as the JSON schema
type Explanation struct{ JA, EN string }
type Correction struct {
	ID          string
	Original    string
	Replacement string
	Type        Type
	Severity    Severity
	Explanation Explanation
	Concepts    []string
}
type Result struct {
	Original    string       // the reviewed selection
	Corrected   string       // selection with all corrections applied
	Corrections []Correction // only the ones that applied; each gets a uuid ID here
}
var ErrNoMatch = errors.New("correction original not found in selection")
func NewResult(selection string, corrections []Correction) (Result, error)
```

`NewResult` semantics: walk `corrections` in the order given; find each `Original` as a substring of the *remaining* selection (search cursor advances past each applied replacement so repeated substrings bind left-to-right and never overlap); skip a correction whose `Original` isn't found (collecting it is the caller's concern — return only applied ones); assign uuid IDs; build `Corrected`. Returns `ErrNoMatch`-wrapped error only if NO corrections were provided-and-applied but some were provided (i.e. all missed — a sign of a hallucinating model).

- [ ] **Step 1: Failing diff tests.** `diff_test.go`: `Runes("とても面白いでした", "とても面白かったです")` — concatenating segments where Op≠OpDelete reproduces the second string; concatenating where Op≠OpInsert reproduces the first; segments contain no broken UTF-8 (each `Segment.Text` is `utf8.ValidString`); `Runes(x, x)` yields a single OpEqual segment. Implement with `diffmatchpatch.New()`, `DiffMain(a, b, false)`, `DiffCleanupSemantic`, map to Segments. → PASS.
- [ ] **Step 2: Failing correction tests.** Cases: (1) the PRD example — selection `昨日友達と映画を見に行って、とても面白いでした。`, one correction 面白いでした→面白かったです ⇒ `Corrected == "昨日友達と映画を見に行って、とても面白かったです。"`, 1 applied with non-empty ID; (2) repeated substring — selection `いいですいいです`, correction いいです→よかったです applied once at the FIRST occurrence, cursor advances (a second identical correction binds the second occurrence); (3) not-found correction is skipped, others still apply; (4) all-miss returns error wrapping `ErrNoMatch`; (5) empty corrections list → `Corrected == selection`, no error. Implement using `strings.Index` on the remaining substring with a byte cursor (indices stay on rune boundaries because we only cut at match boundaries of well-formed UTF-8 needles). → PASS.
- [ ] **Step 3: Commit** — `git add -A && git commit -m "feat: correction domain with deterministic apply and rune-level diff engine"`

---

### Task 11: Anthropic adapter (forced tool use)

**Files:**
- Create: `internal/adapters/anthropic/anthropic.go`
- Modify: `cmd/jlp/main.go` (provider `anthropic` wiring), `.env.example` (comment which vars matter)
- Test: `internal/adapters/anthropic/anthropic_test.go` (httptest — runs in `make test`, NO live API, NO build tag)

**Interfaces:**
- Consumes: `ai.StructuredRequest/StructuredResponse/StructuredGenerator`, `config.Anthropic{APIKey, Model, BaseURL}`.
- Produces: `anthropic.New(cfg config.Anthropic) ai.StructuredGenerator`.

Mechanism: one `messages` call with a single tool `emit_result` whose `input_schema` is `req.Schema`, `tool_choice = {"type":"tool","name":"emit_result"}`, system = `req.System`, one user message = `req.User`, `max_tokens = req.MaxTokens` (default 2048 when 0). The response's `tool_use` content block input IS the structured JSON. Map usage tokens and model into `StructuredResponse` (Provider `"anthropic"`). Error if no tool_use block is present.

- [ ] **Step 1: Failing httptest-based test.** Start `httptest.NewServer` that asserts the request path is `/v1/messages`, header `x-api-key` == `sk-test`, body JSON contains `"tool_choice"` forcing `emit_result` and the schema under `tools[0].input_schema`; respond with a canned Anthropic Messages response:

```json
{"id":"msg_test","model":"claude-sonnet-5","role":"assistant","stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"tu_1","name":"emit_result",
   "input":{"corrections":[{"original":"面白いでした","replacement":"面白かったです","type":"conjugation","severity":"incorrect","explanation":{"ja":"×","en":"x"}}]}}],
 "usage":{"input_tokens":210,"output_tokens":96}}
```

Test constructs `New(config.Anthropic{APIKey:"sk-test", Model:"claude-sonnet-5", BaseURL: srv.URL})`, calls `GenerateStructured` with the real `correction_result.v1` schema, asserts: returned `JSON` unmarshals with `corrections[0].replacement == "面白かったです"`, tokens 210/96, provider `anthropic`. Second test: a response with only a `text` block → error mentioning "tool_use". `make test` → FAIL.
- [ ] **Step 2: Implement with `github.com/anthropics/anthropic-sdk-go`** (client options `option.WithAPIKey`, `option.WithBaseURL`). Build the tool with `input_schema` set directly from `req.Schema` (the SDK accepts a schema map — `json.Unmarshal` req.Schema into `map[string]any` first). If the installed SDK's type names differ from what you expect, follow the SDK's own README examples — the wire contract above is the invariant, not the Go type names. `make test` → PASS.
- [ ] **Step 3: Wire provider selection in main:** `case "anthropic": gen = anthropic.New(cfg.AI.Anthropic)` (still wrapped by the observer). Boot check: `make up` still healthy with provider `fake` (default).
- [ ] **Step 4: Commit** — `git add -A && git commit -m "feat: Anthropic structured-generation adapter via forced tool use"`

---

### Task 12: Teacher agent and feedback pipeline (application layer)

**Files:**
- Create: `internal/agent/teacher/teacher.go`, `internal/application/feedback/service.go`, `internal/ports/storage/feedback.go`, `internal/adapters/postgres/migrationsfs/00006_feedback.sql`, `db/queries/feedback.sql`, `internal/adapters/postgres/feedback.go`
- Test: `internal/agent/teacher/teacher_test.go`, `internal/application/feedback/service_test.go`, `internal/adapters/postgres/feedback_test.go` (integration)

**Interfaces:**
- Consumes: `ai.StructuredGenerator` (+ fakeai), `prompts.Render`, `schemas.Get/Validate`, `correction.NewResult`, `diff.Runes`, `learning.Recorder`, repositories from earlier tasks.
- Produces:

```go
// agent/teacher — Rule 3: this package imports ports/ai + prompts + schemas + domain ONLY.
func New(gen ai.StructuredGenerator) *Agent
type ReviewInput struct {
	Identity     learner.IdentityID
	Session      session.Session
	Selection    string
	Context      string   // surrounding document text
	RecentErrors []string // human-readable weakness summaries (empty in MVP; Phase 2 fills it)
}
func (a *Agent) ReviewWriting(ctx context.Context, in ReviewInput) (correction.Result, ai.StructuredResponse, error)

// ports/storage/feedback.go
type FeedbackRecord struct {
	ID            string
	IdentityID    learner.IdentityID
	SessionID     session.ID
	DocumentID    writing.DocumentID
	SelectionStart, SelectionEnd int // rune offsets
	SelectionText string
	CorrectedText string
	AIRequestID   string
	CreatedAt     time.Time
}
type CorrectionRecord struct {
	ID, FeedbackID              string
	Position                    int
	Original, Replacement       string
	Type, Severity              string
	ExplanationJA, ExplanationEN string
	Status                      string // "presented" | "accepted" | "rejected"
}
type FeedbackRepository interface {
	InsertFeedback(ctx context.Context, rec FeedbackRecord, corrections []CorrectionRecord) error // one tx
	UpdateCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (CorrectionRecord, error) // identity check via join to feedback_requests
}

// application/feedback
func NewService(sessions storage.SessionRepository, docs storage.DocumentRepository,
	repo storage.FeedbackRepository, t *teacher.Agent, rec *learning.Recorder) *Service
type Request struct {
	Identity   learner.IdentityID
	SessionID  session.ID
	DocumentID writing.DocumentID
	Start, End int    // rune offsets into the document
	Text       string // the selection as the client saw it
}
type CorrectionView struct {
	correction.Correction
	Status string
	Diff   []diff.Segment // Original vs Replacement
}
type Feedback struct {
	ID          string
	Original    string
	Corrected   string
	Diff        []diff.Segment // whole selection vs corrected
	Corrections []CorrectionView
	AIRequestID string
}
func (s *Service) RequestFeedback(ctx context.Context, req Request) (Feedback, error)
func (s *Service) SetCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (CorrectionView, error)
```

`ReviewWriting` mechanism: render `teacher.feedback`/`v1` with data `{TeacherMode, Strictness, ExplanationLanguage, Purpose, Audience, Register, Context, Selection, RecentErrors}` (from `Session.Profile` + input); `schemas.Get("correction_result.v1")`; call `gen.GenerateStructured` (MaxTokens 2048, Agent `"teacher"`); then Rule 4 enforcement — `schemas.Validate` on `resp.JSON`; on failure attempt ONE constrained repair (extract the first balanced `{…}` block from the raw text and re-validate), then ONE full retry call, then fail with a wrapped error. Unmarshal to a local DTO mirroring the schema, map to `[]correction.Correction`, `correction.NewResult(in.Selection, cs)`.

`RequestFeedback` mechanism: `sessions.Get` (authz) → `docs.Get` → validate `0 ≤ Start ≤ End ≤ len([]rune(doc.Content))`; if `Start==End`, treat the WHOLE document as the selection; selection text = `req.Text` if non-empty else the doc slice; context = doc content windowed to ±200 runes around the selection → `teacher.ReviewWriting` → persist `FeedbackRecord` (AIRequestID = `resp.RequestID`, CorrectedText = `Result.Corrected`) + one `CorrectionRecord` per applied correction (Position = index, Status "presented") → record events: one `feedback.requested` (Subject = doc ID, Evidence `{"start":…,"end":…,"corrections":N}`), then one `correction.presented` per correction (Subject = correction ID, Evidence `{"type":…,"severity":…}`) → return `Feedback` with `diff.Runes` computed for the whole selection and per correction.

`SetCorrectionStatus`: only `"accepted"`/`"rejected"` allowed → `repo.UpdateCorrectionStatus` → record `correction.accepted`/`correction.rejected` (Subject = correction ID, Evidence `{"type":…,"severity":…}`) → return refreshed `CorrectionView`.

- [ ] **Step 1: Failing teacher tests.** Using `fakeai.New()`: reviewing selection `とても面白いでした` yields 1 correction with `Replacement == "面白かったです"` and `Result.Corrected == "とても面白かったです"`. Using a local `flakyGen` test double (implements `ai.StructuredGenerator`): first call returns `JSON: []byte("```json\n{\"corrections\":[]}\n```")`-style garbage wrapped as raw invalid JSON, second call valid → succeeds (repair-or-retry path); an always-invalid double → error. `make test` → FAIL.
- [ ] **Step 2: Implement teacher agent.** → `make test` PASS.
- [ ] **Step 3: Failing service tests.** In-memory fakes for the three repos + real teacher over fakeai + real Recorder over fake event store/bus. Cases: (a) full happy path on the PRD sentence — feedback persisted, 1 correction `presented`, events recorded in order `feedback.requested`, `correction.presented`; (b) `Start==End` → whole document reviewed; (c) cross-identity session → `storage.ErrNotFound`, nothing persisted; (d) `SetCorrectionStatus` accepted → status updated + `correction.accepted` event; (e) invalid status string → error. → FAIL → implement service → PASS.
- [ ] **Step 4: Migration + postgres repo.** `00006_feedback.sql`:

```sql
-- +goose Up
CREATE TABLE feedback_requests (
    id              uuid PRIMARY KEY,
    identity_id     text NOT NULL REFERENCES identities(id),
    session_id      uuid NOT NULL REFERENCES sessions(id),
    document_id     uuid NOT NULL REFERENCES documents(id),
    selection_start int NOT NULL,
    selection_end   int NOT NULL,
    selection_text  text NOT NULL,
    corrected_text  text NOT NULL DEFAULT '',
    ai_request_id   uuid,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX feedback_identity_idx ON feedback_requests (identity_id, created_at DESC);

CREATE TABLE corrections (
    id                  uuid PRIMARY KEY,
    feedback_request_id uuid NOT NULL REFERENCES feedback_requests(id),
    position            int NOT NULL,
    original            text NOT NULL,
    replacement         text NOT NULL,
    type                text NOT NULL,
    severity            text NOT NULL,
    explanation_ja      text NOT NULL DEFAULT '',
    explanation_en      text NOT NULL DEFAULT '',
    status              text NOT NULL DEFAULT 'presented',
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX corrections_feedback_idx ON corrections (feedback_request_id, position);

-- +goose Down
DROP TABLE corrections;
DROP TABLE feedback_requests;
```

Queries: `InsertFeedbackRequest :exec`, `InsertCorrection :exec`, `UpdateCorrectionStatus :one`:

```sql
-- name: UpdateCorrectionStatus :one
UPDATE corrections c SET status = $3
FROM feedback_requests f
WHERE c.id = $1 AND c.feedback_request_id = f.id AND f.identity_id = $2
RETURNING c.id, c.feedback_request_id, c.position, c.original, c.replacement,
          c.type, c.severity, c.explanation_ja, c.explanation_en, c.status;
```

`InsertFeedback` runs both inserts in one tx (same `WithTx` pattern as Task 7). Integration test: insert with 2 corrections, update one to accepted with the right identity (succeeds) and a wrong identity (→ `storage.ErrNotFound`). `make sqlc && make test-integration` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: teacher agent with schema-enforced review and feedback pipeline"`

---

### Task 13: Feedback UI — selection, diff view, correction cards, accept/reject

**Files:**
- Create: `internal/adapters/http/feedback.go`, `web/templates/partials/feedback.html.tmpl`, `web/templates/partials/correction_card.html.tmpl`
- Modify: `web/templates/workspace.html.tmpl` (feedback button), `web/static/js/app.js` (`selectionPayload`), `web/static/css/app.css` (diff + card styles), `internal/adapters/http/server.go` (routes + `Options.Feedback *feedback.Service`), `cmd/jlp/main.go` (wire teacher + feedback service)
- Test: `internal/adapters/http/feedback_test.go`

**Interfaces:**
- Consumes: `feedback.Service`, `RenderPartial`, `jlp.runeOffset`.
- Produces:
  - Routes: `POST /sessions/{sid}/feedback` (form: `document_id`, `start`, `end`, `text`) → renders partial `"feedback"`; `POST /corrections/{id}/status` (form: `status` = `accepted|rejected`) → renders partial `"correction_card"`.
  - View models in `feedback.go` (used by both partials and by Task 15's rating widget):

```go
type diffSpanView struct{ Class, Text string } // "d-eq" | "d-ins" | "d-del"
type correctionCardView struct {
	ID, Type, Severity, Original, Replacement string
	ExplanationJA, ExplanationEN, Status      string
	DiffSpans                                 []diffSpanView
}
type feedbackView struct {
	ID, Original, Corrected, AIRequestID string
	DiffSpans                            []diffSpanView
	Cards                                []correctionCardView
}
func toDiffSpans(segs []diff.Segment) []diffSpanView // OpEqual→d-eq, OpInsert→d-ins, OpDelete→d-del
```

- [ ] **Step 1: Failing handler test.** POST feedback form for a seeded in-memory setup (fake repos + fakeai as in Task 12 tests, real chi router) with text containing `面白いでした` → 200, body contains `面白かったです`, `d-ins`, and both explanation strings; POST status `accepted` on the returned correction id → 200 body contains `data-status="accepted"`. `make test` → FAIL.
- [ ] **Step 2: Templates + JS + handler.** `app.js` addition:

```js
window.jlp.selectionPayload = () => {
  const ta = document.getElementById("editor");
  return {
    document_id: ta.dataset.docId,
    start: window.jlp.runeOffset(ta.value, ta.selectionStart),
    end: window.jlp.runeOffset(ta.value, ta.selectionEnd),
    text: ta.value.slice(ta.selectionStart, ta.selectionEnd),
  };
};
```

Workspace feedback pane header:

```html
<section id="feedback-pane">
  <button id="feedback-btn" hx-post="/sessions/{{.Session.ID}}/feedback"
          hx-target="#feedback-results" hx-swap="innerHTML"
          hx-vals="js:{...window.jlp.selectionPayload()}">
    フィードバックを取得</button>
  <span class="hint">（選択なし＝全文をレビュー）</span>
  <div id="feedback-results"></div>
</section>
```

`partials/feedback.html.tmpl`:

```html
{{define "feedback"}}
<div class="feedback" data-ai-request="{{.AIRequestID}}">
  {{if not .Cards}}<p class="all-good">問題は見つかりませんでした。よく書けています！</p>{{else}}
  <div class="diff-block">
    <h3>修正案</h3>
    <p class="inline-diff">{{range .DiffSpans}}<span class="{{.Class}}">{{.Text}}</span>{{end}}</p>
  </div>
  <div class="cards">{{range .Cards}}{{template "correction_card" .}}{{end}}</div>
  {{end}}
</div>
{{end}}
```

`partials/correction_card.html.tmpl`:

```html
{{define "correction_card"}}
<article class="correction" id="corr-{{.ID}}" data-status="{{.Status}}">
  <header><span class="badge type">{{.Type}}</span>
          <span class="badge sev sev-{{.Severity}}">{{.Severity}}</span></header>
  <p class="pair"><del>{{.Original}}</del> → <ins>{{.Replacement}}</ins></p>
  <p class="inline-diff">{{range .DiffSpans}}<span class="{{.Class}}">{{.Text}}</span>{{end}}</p>
  <p class="expl ja">{{.ExplanationJA}}</p>
  <p class="expl en">{{.ExplanationEN}}</p>
  {{if eq .Status "presented"}}<footer>
    <button hx-post="/corrections/{{.ID}}/status" hx-vals='{"status":"accepted"}'
            hx-target="#corr-{{.ID}}" hx-swap="outerHTML">納得した</button>
    <button hx-post="/corrections/{{.ID}}/status" hx-vals='{"status":"rejected"}'
            hx-target="#corr-{{.ID}}" hx-swap="outerHTML">同意しない</button>
  </footer>{{else}}<footer class="resolved">{{.Status}}</footer>{{end}}
</article>
{{end}}
```

CSS: `.d-ins{background:#e6ffe6;text-decoration:none} .d-del{background:#ffe6e6;text-decoration:line-through} .sev-incorrect{background:var(--accent);color:#fff} .sev-unnatural{background:#e69500;color:#fff}` plus neutral badges for the rest; `.correction[data-status="accepted"]{opacity:.6;border-left:3px solid green}` and rejected variant. Handler parses form ints, calls the service, maps to views, `RenderPartial(w, r, "feedback", view)`. HTMX needs `HX-Request` nothing special. `make test` → PASS.
- [ ] **Step 3: Browser verification (the core loop — be thorough).** `make up && make migrate`, open the session workspace, ensure the editor contains `昨日友達と映画を見に行って、とても面白いでした。` (set + dispatch input, wait for save). Then:
  1. Select the substring `とても面白いでした` via javascript_tool (`ta.setSelectionRange` using UTF-16 indices of that substring; verify `ta.value.slice(...)` echoes it).
  2. Click フィードバックを取得.
  3. Expect a correction card: badges `conjugation`+`incorrect`, pair 面白いでした→面白かったです, inline diff with green 面白かったです segment and struck-through red segment, Japanese AND English explanations visible.
  4. Click 納得した — card turns accepted (opacity + green border), buttons disappear.
  5. Click the feedback button with NO selection (collapse via `ta.setSelectionRange(0,0)`) — whole document reviewed, same correction found.
  6. Activity feed shows `feedback.requested`, `correction.presented`, `correction.accepted`.
  7. `make db-shell`: `select status from corrections;` shows accepted; `select provider, model, success from ai_requests;` shows fake/fake-1/t.
  8. Console: zero errors.
- [ ] **Step 4: Commit** — `git add -A && git commit -m "feat: feedback UI with inline rune diff and correction cards"`

---

## Epic F — Learner Statistics & AI Observability UI

### Task 14: Basic learner model — statistics service and dashboard

**Files:**
- Create: `internal/ports/storage/analytics.go`, `db/queries/analytics.sql`, `internal/adapters/postgres/analytics.go`, `internal/application/analytics/service.go`
- Modify: `web/templates/home.html.tmpl` (becomes the dashboard), `internal/adapters/http/server.go` (home handler uses analytics + sessions; `Options.Analytics *analytics.Service`), `cmd/jlp/main.go`, `web/static/css/app.css` (stat tiles)
- Test: `internal/application/analytics/service_test.go`, `internal/adapters/postgres/analytics_test.go` (integration)

**Interfaces:**
- Consumes: repositories, event/correction tables.
- Produces:

```go
// ports/storage/analytics.go
type ErrorTypeCount struct{ Type string; Count int }
type Statistics struct {
	RunesWritten          int // sum of char_length(content) over the identity's documents
	SessionCount          int
	FeedbackRequests      int
	CorrectionsPresented  int
	CorrectionsAccepted   int
	CorrectionsRejected   int
	AcceptanceRate        float64 // accepted / (accepted+rejected), 0 when no resolutions
	CorrectionsPer1000    float64 // presented per 1000 runes written, 0 when no runes
	TopErrorTypes         []ErrorTypeCount // top 5 by presented corrections
}
type AnalyticsRepository interface {
	Statistics(ctx context.Context, identity learner.IdentityID) (Statistics, error)
}

// application/analytics
func NewService(repo storage.AnalyticsRepository) *Service
func (s *Service) Statistics(ctx context.Context, identity learner.IdentityID) (storage.Statistics, error) // computes the two derived ratios from the raw counts
```

The repository returns raw counts (ratios zeroed); the application service computes `AcceptanceRate` and `CorrectionsPer1000` — that keeps the derivation unit-testable without SQL.

- [ ] **Step 1: Failing service test.** Fake repo returns counts (runes 2000, presented 6, accepted 3, rejected 1) → service yields `AcceptanceRate == 0.75`, `CorrectionsPer1000 == 3.0`; zero-division cases return 0. → FAIL → implement → PASS.
- [ ] **Step 2: SQL + integration.** `db/queries/analytics.sql` — one query per counter, sqlc `:one` each (`CountRunesWritten` = `SELECT COALESCE(SUM(char_length(content)),0)::int FROM documents WHERE identity_id=$1` — PG `char_length` counts code points, which matches Go rune counts for Japanese text; `CountCorrectionsByStatus :many` grouped; `TopErrorTypes :many` `GROUP BY type ORDER BY count(*) DESC LIMIT 5` joined through feedback_requests for identity scoping). Postgres repo assembles `Statistics`. Integration test: seed one identity's worth of rows + a SECOND identity's rows, assert the first identity's stats exclude the second (Rule 6). `make sqlc && make test-integration` → PASS.
- [ ] **Step 3: Dashboard template.** Replace home content: a `.stat-grid` of tiles (書いた文字数 RunesWritten, セッション SessionCount, フィードバック FeedbackRequests, 納得率 AcceptanceRate as %, 修正/1000字 CorrectionsPer1000) + よくある間違い list (TopErrorTypes) + recent sessions links. Plain tiles and lists only — no chart library in MVP.
- [ ] **Step 4: Browser verification.** After the Task 13 flow data exists: open `/` — tiles show non-zero runes/feedback counts, top error types lists `conjugation`, recent sessions link opens the workspace. Console clean.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: learner statistics service and dashboard"`

---

### Task 15: AI ratings and AI requests page

**Files:**
- Create: `internal/adapters/postgres/migrationsfs/00007_ai_ratings.sql`, `db/queries/ai_ratings.sql`, `internal/adapters/postgres/airatings.go`, `internal/ports/storage/airatings.go`, `internal/adapters/http/ai.go`, `web/templates/ai.html.tmpl`
- Modify: `web/templates/partials/feedback.html.tmpl` (star widget), `internal/adapters/http/server.go` (routes: `POST /ratings`, `GET /ai`), `web/templates/layout.html.tmpl` (nav link AI), `cmd/jlp/main.go`
- Test: `internal/adapters/http/ai_test.go`, `internal/adapters/postgres/airatings_test.go` (integration)

**Interfaces:**
- Consumes: `feedbackView.AIRequestID` (Task 13), `AIRequestRepository.List` (Task 9).
- Produces:

```go
// ports/storage/airatings.go
type AIRating struct {
	ID, AIRequestID string
	IdentityID      learner.IdentityID
	Rating          int // 1..5
	CreatedAt       time.Time
}
type AIRatingRepository interface {
	Upsert(ctx context.Context, r AIRating) error // one rating per (ai_request_id, identity)
	ForRequests(ctx context.Context, identity learner.IdentityID, requestIDs []string) (map[string]int, error)
}
```

- Routes: `POST /ratings` (form: `ai_request_id`, `rating`) → 204; `GET /ai` → table of the identity's last 50 ai_requests (provider, model, prompt, latency ms, tokens, cost, success, rating).
- Migration: `ai_ratings(id uuid pk, ai_request_id uuid NOT NULL REFERENCES ai_requests(id), identity_id text NOT NULL REFERENCES identities(id), rating int NOT NULL CHECK (rating BETWEEN 1 AND 5), created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (ai_request_id, identity_id))` with `ON CONFLICT` upsert.
- Star widget (Alpine) appended inside `partials/feedback.html.tmpl`'s `.feedback` div:

```html
<div class="rating" x-data="{ score: 0 }">
  <span>この添削はどうでしたか？</span>
  {{/* 5 buttons; posts on click */}}
  <template x-for="n in 5"><button type="button" class="star"
     :class="{ on: n <= score }"
     @click="score = n; fetch('/ratings', {method:'POST',
        body: new URLSearchParams({ai_request_id: '{{.AIRequestID}}', rating: n})})">★</button></template>
</div>
```

- [ ] **Step 1: Failing tests.** Handler test: POST `/ratings` with rating 4 → 204 and fake repo captured `{AIRequestID, 4}`; rating 9 → 400. Integration test: upsert twice for same request (3 then 5) → `ForRequests` returns 5. → FAIL → implement (migration, sqlc, repo, handlers, `/ai` page rendering the joined list) → PASS (`make test`, `make test-integration`).
- [ ] **Step 2: Browser verification.** Run the feedback flow, click the 4th star — stars 1–4 light up; `/ai` page lists the fake request with rating 4, latency, zero cost; `make db-shell` confirms the `ai_ratings` row. Console clean.
- [ ] **Step 3: Commit** — `git add -A && git commit -m "feat: AI response ratings and AI requests observability page"`

---

## Epic G — JSON API & PWA

### Task 16: Versioned JSON API (/api/v1)

**Files:**
- Create: `internal/adapters/http/api.go`
- Modify: `internal/adapters/http/server.go` (mount `/api/v1` group)
- Test: `internal/adapters/http/api_test.go`

**Interfaces:**
- Consumes: the same application services the HTML handlers use — NO new application code (this proves HTMX isn't the domain boundary, PRD §38).
- Produces stable DTOs (json tags exactly as shown — clients depend on them):

```go
type sessionDTO struct {
	ID string `json:"id"`; Title string `json:"title"`; Purpose string `json:"purpose"`
	TeacherMode string `json:"teacher_mode"`; ExplanationLanguage string `json:"explanation_language"`
	Strictness string `json:"strictness"`; CreatedAt time.Time `json:"created_at"`
}
type feedbackDTO struct {
	ID string `json:"id"`; Original string `json:"original"`; Corrected string `json:"corrected"`
	AIRequestID string `json:"ai_request_id"`; Corrections []correctionDTO `json:"corrections"`
}
type correctionDTO struct {
	ID string `json:"id"`; Original string `json:"original"`; Replacement string `json:"replacement"`
	Type string `json:"type"`; Severity string `json:"severity"`
	ExplanationJA string `json:"explanation_ja"`; ExplanationEN string `json:"explanation_en"`
	Status string `json:"status"`
}
```

Routes (all under the identity middleware; errors are `{"error":"…"}` with proper status): `GET /api/v1/sessions`, `POST /api/v1/sessions` (JSON body `{title, purpose, teacher_mode, explanation_language, strictness}`), `GET /api/v1/sessions/{id}`, `POST /api/v1/sessions/{id}/feedback` (JSON `{document_id, start, end, text}` — rune offsets), `POST /api/v1/corrections/{id}/status` (`{"status":"accepted"}`), `GET /api/v1/learner/statistics`, `POST /api/v1/ratings`. Unauthenticated → `401 {"error":"unauthorized"}` (adjust `RequireIdentity` to emit JSON when the path starts with `/api/`).

- [ ] **Step 1: Failing contract test** covering: create session via JSON → 201 with `id`; feedback POST on seeded doc returns `corrections[0].replacement == "面白かったです"`; statistics returns the derived fields; malformed JSON → 400. `make test` → FAIL.
- [ ] **Step 2: Implement handlers** (thin: decode → service → encode). → PASS.
- [ ] **Step 3: Browser verification.** Via javascript_tool on an app tab: `await (await fetch('/api/v1/sessions')).json()` returns the existing session; a feedback POST via fetch returns the correction JSON. (This validates cookies/middleware work for API calls from the browser context — exactly how the Chrome extension will call it in Phase 3.)
- [ ] **Step 4: Commit** — `git add -A && git commit -m "feat: versioned JSON API sharing application services with the UI"`

---

### Task 17: PWA shell — manifest, service worker, offline shell

**Files:**
- Create: `web/static/manifest.webmanifest`, `web/static/sw.js`, `web/static/icons/icon.svg`, `web/templates/offline.html.tmpl`
- Modify: `web/templates/layout.html.tmpl` (manifest link + theme-color + SW registration), `internal/adapters/http/server.go` (`GET /offline` route, exempt from auth like `/healthz`), `web/static/js/app.js` (SW registration)
- Test: `internal/adapters/http/server_test.go` (offline route renders)

**Interfaces:** Produces: SW cache name `jlp-shell-v1` — bump the suffix whenever shell assets change (later tasks must respect this).

- [ ] **Step 1: Assets.** `manifest.webmanifest`: `{"name":"JLP 日本語","short_name":"JLP","start_url":"/","display":"standalone","background_color":"#ffffff","theme_color":"#b7282e","icons":[{"src":"/static/icons/icon.svg","sizes":"any","type":"image/svg+xml","purpose":"any"}]}`. `icon.svg`: 512×512 rounded square, accent background, white 「日」glyph (`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"><rect width="512" height="512" rx="96" fill="#b7282e"/><text x="256" y="330" font-size="280" text-anchor="middle" fill="#fff" font-family="serif">日</text></svg>`). `sw.js`: install → precache `['/offline', '/static/css/app.css', '/static/js/htmx.min.js', '/static/js/alpine.min.js', '/static/js/app.js', '/static/manifest.webmanifest', '/static/icons/icon.svg']` into `jlp-shell-v1`; fetch → network-first for navigations with `caches.match('/offline')` fallback, cache-first for `/static/*`; activate → delete old cache names. `app.js` registers `/static/sw.js` with `{scope: '/'}` — this needs the response header `Service-Worker-Allowed: /` on `/static/sw.js` (add a tiny middleware for that path in `server.go`). Offline page: "オフラインです — 接続が戻り次第、続きが書けます。"
- [ ] **Step 2: Test + run.** Offline route test asserts 200 + オフライン; `make test` PASS.
- [ ] **Step 3: Browser verification.** Hard-reload the app twice; via javascript_tool: `(await navigator.serviceWorker.getRegistration())?.active?.state === 'activated'`; `await caches.keys()` includes `jlp-shell-v1`; `(await (await caches.open('jlp-shell-v1')).keys()).length >= 6`; `document.querySelector('link[rel=manifest]')` present; manifest fetch → 200 JSON. Console clean (no SW errors).
- [ ] **Step 4: Commit** — `git add -A && git commit -m "feat: installable PWA shell with offline fallback"`

---

## Epic H — Quality Gates, Seed, Deploy

### Task 18: Architecture enforcement, lint config, seed data, README

**Files:**
- Create: `.golangci.yml`, `cmd/jlp/seed.go` (seed subcommand), `README.md`
- Modify: `Makefile` (`lint`, `fmt`, `arch-check`, `seed` targets), `cmd/jlp/main.go` (case "seed")
- Test: the gates themselves are the test

**Interfaces:** Produces Make targets `lint`, `fmt`, `arch-check`, `seed`; a seeded stack any demo/verification can rely on.

- [ ] **Step 1: `.golangci.yml`** — standard linters (govet, staticcheck, errcheck, ineffassign, unused) plus depguard encoding PRD §75:

```yaml
version: "2"
linters:
  enable: [depguard]
  settings:
    depguard:
      rules:
        domain-purity:
          files: ["**/internal/domain/**"]
          deny:
            - pkg: github.com/mikeyaustin/jlp/internal/adapters
              desc: "Rule 1/2: domain imports no adapters"
            - pkg: github.com/mikeyaustin/jlp/internal/application
              desc: "dependencies point inward"
            - pkg: github.com/mikeyaustin/jlp/internal/ports
              desc: "domain defines no dependencies on ports"
            - { pkg: net/http, desc: "Rule 2" }
            - { pkg: github.com/jackc, desc: "Rule 2" }
            - { pkg: github.com/spf13/viper, desc: "Rule 2" }
        agents-no-repos:
          files: ["**/internal/agent/**"]
          deny:
            - pkg: github.com/mikeyaustin/jlp/internal/adapters
              desc: "Rule 3: agents reach state only through tools/services"
            - pkg: github.com/mikeyaustin/jlp/internal/ports/storage
              desc: "Rule 3: agents must not touch repositories"
        application-no-adapters:
          files: ["**/internal/application/**"]
          deny:
            - pkg: github.com/mikeyaustin/jlp/internal/adapters
              desc: "application depends on ports, not adapters"
```

Makefile: `lint: $(TOOLS) golangci-lint run ./...`, `fmt: $(TOOLS) gofmt -w .`, `arch-check: $(TOOLS) golangci-lint run --enable-only depguard ./...`. Run `make lint` and FIX every finding now (expect a few). If a depguard rule fires on legitimate code, the code moves — the rule doesn't bend (e.g. if `application/feedback` imports `diff` from domain that's fine; if anything in domain grew an adapter import, refactor it out).
- [ ] **Step 2: Seed.** `jlp seed`: upsert identity (`dev`/`Dev Learner`), create session 「旅行について書く」 (purpose Blog post, defaults), document containing `昨日友達と映画を見に行って、とても面白いでした。京都はとてもきれいでした。` — idempotent (skip when a session with that title exists). Makefile `seed: $(TOOLS) go run ./cmd/jlp seed`.
- [ ] **Step 3: README.md** — quickstart (`make init && make build && make up && make migrate && make seed`, open http://localhost:8080), the full `make help` table, auth modes (static vs `make up-auth`), AI providers (fake default; set `ANTHROPIC_API_KEY` + `APP_AI_PROVIDER=anthropic` in `.env` for live), architecture summary + pointer to PRD/spec/plans, testing commands.
- [ ] **Step 4: Full clean-slate verification (browser).** `make clean && make build && make up && make migrate && make seed`, then in Chrome: dashboard shows the seeded session → open workspace → seeded text present → run the whole Task 13 feedback flow again. `make test && make test-integration && make lint && make arch-check` all pass.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: architecture gates, seed data, and quickstart README"`

---

### Task 19: Production build and `make deploy`

**Files:**
- Create: `deploy/compose.prod.yml`, `deploy/caddy/Caddyfile.prod`, `deploy/.env.prod.example`, `scripts/gen-prod-secrets.sh`, `scripts/wait-healthy.sh`
- Modify: `Makefile` (`deploy`, `deploy-local`, `deploy-logs` targets), `.gitignore` (`deploy/.env.prod`)
- Test: `make deploy-local` is the test

**Interfaces:** Produces: `make deploy` (remote via `DOCKER_HOST=ssh://$(DEPLOY_HOST)`), `make deploy-local` (prod stack locally on https://localhost:8444 for verification).

- [ ] **Step 1: `deploy/compose.prod.yml`** — services: `app` (build target `prod`, `restart: unless-stopped`, env from `deploy/.env.prod`: `APP_DATABASE_URL` pointing at the prod postgres with a strong password, `APP_AUTH_MODE=authelia`, `APP_AI_PROVIDER` + key; NO source mounts, NO published app port), `postgres` (volume `pgdata_prod`, password from env file), `caddy` (ports `8444:8444`, `Caddyfile.prod` — same forward_auth block as dev but site `{$JLP_DOMAIN}:8444` with `tls internal`, domain from env), `authelia` (config mounted from `deploy/authelia/configuration.prod.yml` — copy of dev config with the three secrets replaced by `{{ secret "/secrets/…" }}` file references; `scripts/gen-prod-secrets.sh` writes three random 64-char secrets into `deploy/authelia/secrets/` [gitignored] with `openssl rand -hex 32`, plus a prod users.yml generated the same way as dev). `deploy/.env.prod.example` documents every variable.
- [ ] **Step 2: Makefile targets.**

```make
PROD_COMPOSE := docker compose -f deploy/compose.prod.yml --env-file deploy/.env.prod

deploy-local: ## Run the production stack locally (https://localhost:8444)
	$(PROD_COMPOSE) build
	$(PROD_COMPOSE) up -d
	$(PROD_COMPOSE) run --rm app migrate
	sh scripts/wait-healthy.sh https://localhost:8444/healthz

deploy: ## Deploy to $(DEPLOY_HOST) over SSH (set in .env)
	@test -n "$(DEPLOY_HOST)" || (echo "set DEPLOY_HOST in .env"; exit 1)
	DOCKER_HOST=ssh://$(DEPLOY_HOST) $(PROD_COMPOSE) build
	DOCKER_HOST=ssh://$(DEPLOY_HOST) $(PROD_COMPOSE) up -d
	DOCKER_HOST=ssh://$(DEPLOY_HOST) $(PROD_COMPOSE) run --rm app migrate
	@echo "deployed to $(DEPLOY_HOST)"

deploy-logs: ## Tail remote app logs
	DOCKER_HOST=ssh://$(DEPLOY_HOST) $(PROD_COMPOSE) logs -f app
```

`scripts/wait-healthy.sh`: curl `-k` the URL up to 30× with 2s sleeps, exit 1 with logs hint on failure. (Note: `/healthz` must be excluded from forward_auth in `Caddyfile.prod` — add a `handle /healthz` block that proxies straight to app.)
- [ ] **Step 3: Verification (browser, prod stack).** `sh scripts/gen-prod-secrets.sh && cp deploy/.env.prod.example deploy/.env.prod` (fill dev-ish values, `JLP_DOMAIN=localhost`), `make deploy-local`. In Chrome: `https://localhost:8444` → cert warning → proceed → Authelia login → dashboard loads → run one feedback round (fake provider) → confirm NO hot-reload (this is the distroless binary: `docker compose -f deploy/compose.prod.yml ps` shows the prod image). Then `$(PROD_COMPOSE) down`. Remote `make deploy` is exercised for real when the user configures `DEPLOY_HOST` — the make path is identical, only `DOCKER_HOST` differs.
- [ ] **Step 4: Commit** — `git add -A && git commit -m "feat: production compose stack and make deploy over SSH docker context"`

---

## Done — MVP exit criteria

All true, each verified by the final task's clean-slate run:

1. `make init build up migrate seed` from a fresh clone yields a working app at http://localhost:8080.
2. The PRD §57 loop works in the browser end-to-end (write → select → feedback → diff + explanations → accept/reject → events → stats).
3. `make up-auth` serves the same app behind a real Authelia login; direct unauthenticated access is rejected.
4. `make test`, `make test-integration`, `make lint`, `make arch-check` all pass; no test needs a live AI key.
5. `make deploy-local` runs the production images end-to-end; `make deploy` targets `DEPLOY_HOST` with the identical path.
6. Every UI-facing task was browser-verified by its implementing subagent via claude-in-chrome.
