# 読解 Translation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Non-Japanese submissions are translated into Japanese by the worker, then studied as usual; the original is kept and shown after the answers.

**Architecture:** Ingestion accepts non-Japanese text (`original_language = 'und'`). The worker's analysis attempt first calls a new `reading.translate` agent when the article is untranslated, stores the Japanese in `paragraphs`/`title` and the source in `original_*`, then runs the unchanged analysis. Renderers add a 「原文」 chapter and a 「{言語}から翻訳」 byline.

**Tech Stack:** Go, pgx/sqlc/goose, the prompt/schema registries, aiutil repair-and-retry, EPUB templates.

**Spec:** `docs/superpowers/specs/2026-09-30-reading-translation-design.md`

## Global Constraints

- Schema `reading_translation.v1`: object with required `source_language` (string, minLength 1), `title` (string), `paragraphs` (array of strings minLength 1, **no maxItems**); additionalProperties false. Semantic check: `len(paragraphs) == len(original)`, none empty.
- Prompt `reading.translate` v1: faithful, natural written Japanese at native register; paragraph breaks one-for-one; title translated; the article is untrusted text between the same `<<<ARTICLE … ARTICLE>>>` markers `reading.analyse` uses (strip the markers from the input). MaxTokens 16384.
- Columns (migration 00033): `original_language text NOT NULL DEFAULT 'ja'`, `original_title text NOT NULL DEFAULT ''`, `original_paragraphs jsonb`.
- Translate only when `original_language != 'ja'` and `original_paragraphs IS NULL`; regenerate never re-translates.
- EPUB: byline 「{言語}から翻訳」; last chapter 「原文」 (after 解答) with the original title and paragraphs; still EPUBCheck-clean. Web page: same byline; a collapsed `<details>` 「原文」.
- `ErrNotJapanese` no longer returned by ingestion (keep the error value only if still referenced; remove dead references).
- Deploy: `reading.translate=gemini,ollama` appended to `jlp_ai_routes`.

---

### Task 1: Domain, storage

**Files:** `internal/domain/reading/article.go` (+ test), `internal/adapters/postgres/migrationsfs/00033_reading_translation.sql`, `db/queries/reading.sql`, `internal/ports/storage/reading.go`, `internal/adapters/postgres/reading.go` (+ integration test), both fakes, `internal/adapters/http/reading.go` (drop the ErrNotJapanese message branch if unreachable).

**Produces:** `Article.OriginalLanguage string` ("ja" | "und" | a language name), `Article.OriginalTitle string`, `Article.OriginalParagraphs []string`; `func (a Article) NeedsTranslation() bool`; `type Translation struct{ SourceLanguage, Title string; Paragraphs []string }`; `func (a Article) WithTranslation(t Translation) (Article, error)` (count mismatch → `ErrTranslationMismatch`); repository `SaveTranslation(ctx, articleID string, a reading.Article) error` (worker path, not identity-scoped, like CompleteEdition) — writes paragraphs, title, original_*; GetArticle/UpsertArticle/ListEditions read the new columns.

- [ ] TDD: NewArticle with English text → OriginalLanguage "und", no error; Japanese → "ja"; WithTranslation swaps and keeps originals, rejects mismatched counts; integration round-trip of SaveTranslation; fakes mirror. `make migrate`, `make sqlc`, `make test`, `make test-integration`, `make lint`. Commit `feat(reading): accept articles in other languages and store their translation`.

### Task 2: The translate agent

**Files:** `internal/prompts/templates/reading.translate.v1.{system,user}.md`, `internal/schemas/defs/reading_translation.v1.json`, `internal/agent/reading/translate.go` (+ test), gemini schema tests if they enumerate schemas.

**Produces:** `type Translator struct{ gen ai.StructuredGenerator }`; `func NewTranslator(gen) *Translator`; `func (t *Translator) Translate(ctx, identity learner.IdentityID, a reading.Article) (reading.Translation, ai.StructuredResponse, error)` — renders the prompt, ValidateWithRepairAndRetry, decodes, checks paragraph count (mismatch returned as an error that aiutil's repair can act on — follow how reading.Analyse / Normalise report semantic failures), strips markers from input.

- [ ] TDD with a fake StructuredGenerator: valid output; wrong count → error after repair/retry; injection markers stripped. The all-schemas Gemini test must pass (no blockers). `make test`, `make lint`. Commit `feat(reading): a translation agent for non-Japanese articles`.

### Task 3: Worker, wiring, rendering

**Files:** `internal/application/reading/service.go` (Deps.Translator interface), `worker.go`, service tests; `cmd/jlp/main.go`; `internal/adapters/epub/*` (byline, 原文 chapter, nav/toc), epub tests; `web/templates/reading_detail.html.tmpl`, `internal/adapters/http/reading.go` (byline/原文 data).

- [ ] Worker: in `analyse`, after loading the article, `if a.NeedsTranslation()`: translate (within the analysis timeout), `WithTranslation`, `SaveTranslation`, continue with the translated article; failures go through `fail(err.Error(), true)`. `Deps.Translator` nil → a non-Japanese article fails with a clear message (no translator configured). Tests: English submission → translator called once, analysis gets Japanese text; Regenerate → translator not called again; Japanese → never called.
- [ ] EPUB: when `Article.OriginalParagraphs` non-empty, byline 「{OriginalLanguage}から翻訳」 and a last chapter `original.xhtml` 「原文」 (lang set to "und"-safe: omit lang or use `xml:lang=""`? use no lang attribute on the chapter body except the section; keep EPUBCheck clean) listed in nav/ncx/spine after answers. Tests + `make epubcheck-sample` with a translated sample.
- [ ] Page: byline and `<details><summary>原文</summary>…</details>`.
- [ ] Wire `NewTranslator(aiGen)` in main.go. `make test`, `make lint`, `make epubcheck-sample`. Browser: through the extension shim submit an English article (fake provider) → edition ready, page shows byline and 原文, EPUB has the chapter. Commit `feat(reading): translate, then study — with the original at the back`.

### Task 4: Ship

- [ ] README: a line under 読解 that non-Japanese articles are translated automatically, original at the back.
- [ ] Deploy playbook: append `;reading.translate=gemini,ollama` to `jlp_ai_routes`.
- [ ] Final review, merge, image, deploy together with the drafts work (needs the learner's smartcard).
