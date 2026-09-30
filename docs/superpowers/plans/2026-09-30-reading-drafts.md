# 読解 Multi-page Drafts and Deletes That Stay Deleted — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collect an article from several pages into one server-side draft, review and drop junk blocks, then send it as one Kindle edition; and make a re-import of a deleted article start fresh.

**Architecture:** A per-learner active draft (`reading_drafts` + ordered `reading_draft_blocks`) is appended to through `POST /api/v1/reading/drafts/active/parts` from the extension and the phone pages, reviewed on a server-rendered page with htmx toggles, and sent by building an ordinary `reading.Draft` from the kept blocks and calling the existing `Service.Submit`. The article upsert purges a soft-deleted match instead of restoring it.

**Tech Stack:** Go (pgx/sqlc/goose, chi, html/template, htmx), vanilla JS (extension popup/background, JLP phone pages).

**Spec:** `docs/superpowers/specs/2026-09-30-reading-drafts-design.md`

## Global Constraints

- One active draft per identity (unique index on `reading_drafts.identity_id`); ≤ 10 pages per draft, message 「下書きは10ページまでです」; re-adding a `page_url` already in the draft replaces that page's blocks; each add is one transaction.
- A page's paragraphs come from `selection` when non-empty, else `content`, via `reading.NormaliseParagraphs`; its images are validated with `reading.NewFigures` (same limits as articles) and placed after the paragraph each anchors to.
- Send: kept paragraphs in order joined with "\n\n"; each kept image → `FigureDraft{InText: true, AfterText: first 40 runes of the previous kept paragraph (or AfterParagraph -1 if none)}`, first kept image `Lead: true`; draft title and metadata; `Service.Submit` with `Deliver` from the form (default on); then delete the draft; redirect `/reading/{edition}`.
- Review warnings: over the service's max article runes → 「長すぎます（上限 {N}字）— 段落を除外してください」 and send disabled; > 12 kept images → 「画像は最初の12枚だけ使われます」. Summary line 「{pages}ページ・段落 {kept}/{total}・画像 {kept}/{total}・{chars}字」. Gone draft: 「この下書きはありません」.
- Drafts not updated for 7 days are swept by the reading worker (once per poll tick).
- Re-import of a soft-deleted article purges its deliveries, editions, figures and row in one transaction, then inserts fresh (`created = true`). Undo and `jlp restore reading` keep working until then.
- Every draft/block/image route is identity-scoped (404 otherwise). API: session or a `reading:write` token. Review page and its actions: session only.
- The one-shot capture stays the default everywhere; the phone's add-to-draft still requires a tap.
- Dev: app http://localhost:28080, Mailpit http://localhost:8025; make/compose only; repo tmp/ and dist/ are root-owned.

## Review Focus

1. **Double tap on "add this page"** → replaced, not duplicated. Pinned in Task 2.
2. **The reported bug: delete then re-import** → fresh edition, old one gone. Pinned in Task 1.
3. **Another learner's draft id / block seq / image** → 404. Pinned in Tasks 2 and 4.
4. **Send with everything excluded / over the cap** → message, draft kept. Pinned in Task 3.
5. **Popup opened with a draft already active** → shows the draft state, not the one-shot flow. Pinned in Task 5.

---

### Task 1: Deletes that stay deleted

**Files:** `internal/adapters/postgres/reading.go` (UpsertArticle), `db/queries/reading.sql` (if the query changes), `internal/adapters/postgres/reading_test.go`, the two in-memory fakes (`memRepo` in internal/application/reading/service_test.go, `fakeReadingRepo` in internal/adapters/http/reading_test.go), `internal/application/reading/service_test.go`.

- [ ] **Step 1: Failing tests.**
  - Integration (`-tags integration`): upsert article A (identity me), queue an edition, attach figures, queue a delivery; soft-delete A; upsert the same content again → `created == true`, a NEW article id; `GetEdition(old edition)` → ErrNotFound; `ListFigures(old article)` empty; no rows remain for the old article id in reading_editions/reading_deliveries/reading_article_figures/reading_articles (query directly). Also: upsert of a NOT-deleted duplicate still returns the existing row with `created == false`. Clean up in t.Cleanup.
  - Service (memRepo): Submit text → Drain (edition ready) → Delete → Submit same text again → `res.Duplicate == false`, `res.Edition.ID` differs, `res.Article.ID` differs, and `List` shows exactly one article.
- [ ] **Step 2: Implement.** In `UpsertArticle`, in one pgx transaction (the repository has `pool`): look up `(identity_id, content_hash)`; if a row exists with `deleted_at IS NOT NULL`, delete its `reading_deliveries` (via editions), `reading_editions`, `reading_article_figures`, then the article row; then run the existing insert. The `ON CONFLICT … DO UPDATE SET deleted_at = NULL` clause becomes unreachable for deleted rows — change it to `DO UPDATE SET identity_id = EXCLUDED.identity_id` (a no-op update so RETURNING still yields the existing live row) and update the query comment to say a deleted match is purged by the repository first. Mirror the semantics in both fakes (a deleted match is removed with its editions/deliveries/figures, then a new article is inserted). Regenerate sqlc if the query changed.
- [ ] **Step 3:** `make test`, `make test-integration`, `make lint`. Commit `fix(reading): a deleted article stays deleted — re-importing it starts fresh`.

### Task 2: Draft storage

**Files:** create `internal/adapters/postgres/migrationsfs/00032_reading_drafts.sql`, `internal/domain/reading/draft.go` (+ test); modify `db/queries/reading.sql`, `internal/ports/storage/reading.go`, `internal/adapters/postgres/reading.go` (+ integration tests), both fakes.

**Interfaces — Produces:**

```go
// internal/domain/reading/draft.go
type BlockKind string
const (BlockParagraph BlockKind = "paragraph"; BlockImage BlockKind = "image")
type DraftBlock struct {
	Seq, Page int; PageURL string; Kind BlockKind
	Text string            // paragraph
	Figure Figure          // image (Data nil when listed)
	Excluded bool
}
type DraftMeta struct { Title, SourceName, SourceURL, Author string; PublishedAt *time.Time }
type DraftInfo struct { ID string; Identity learner.IdentityID; Meta DraftMeta; CreatedAt, UpdatedAt time.Time }
// PageBlocks turns one captured page into ordered blocks: paragraphs from
// NormaliseParagraphs(text), each followed by the figures NewFigures
// anchored after it (figures anchored -1 first). Seq/Page are left 0 for
// the repository to assign. rejected is NewFigures' per-figure errors.
func PageBlocks(pageURL, text string, figs []FigureDraft) (blocks []DraftBlock, rejected []error)
// Summary counts kept/total paragraphs and images and kept runes.
type DraftSummary struct { Pages, Paragraphs, KeptParagraphs, Images, KeptImages, Chars int }
func Summarise(blocks []DraftBlock) DraftSummary
// Submission builds the article Draft from the kept blocks (see Global Constraints).
func Submission(meta DraftMeta, blocks []DraftBlock) Draft
const MaxDraftPages = 10
var ErrDraftFull = errors.New("reading: a draft holds at most 10 pages")
```

`storage.ReadingRepository` gains:

```go
// AddDraftPage appends (or, when pageURL is already in the draft, replaces)
// one page of blocks in identity's active draft, creating it with meta if
// none exists, in one transaction. ErrDraftFull past MaxDraftPages new pages.
AddDraftPage(ctx context.Context, identity learner.IdentityID, meta reading.DraftMeta, pageURL string, blocks []reading.DraftBlock, now time.Time) (reading.DraftInfo, error)
ActiveDraft(ctx context.Context, identity learner.IdentityID) (reading.DraftInfo, error) // ErrNotFound
GetDraft(ctx context.Context, identity learner.IdentityID, id string) (reading.DraftInfo, error)
ListDraftBlocks(ctx context.Context, identity learner.IdentityID, id string, withData bool) ([]reading.DraftBlock, error)
DraftImage(ctx context.Context, identity learner.IdentityID, id string, seq int) (reading.Figure, error)
SetDraftBlockExcluded(ctx context.Context, identity learner.IdentityID, id string, seq int, excluded bool, now time.Time) error
SetDraftTitle(ctx context.Context, identity learner.IdentityID, id, title string, now time.Time) error
DeleteDraft(ctx context.Context, identity learner.IdentityID, id string) error
SweepDrafts(ctx context.Context, before time.Time) (int, error)
```

- [ ] **Step 1: Migration** `00032_reading_drafts.sql` (goose Up/Down like 00031): `reading_drafts(id uuid PK, identity_id text NOT NULL REFERENCES identities(id), title text NOT NULL DEFAULT '', source_name text NOT NULL DEFAULT '', source_url text NOT NULL DEFAULT '', author text NOT NULL DEFAULT '', published_at timestamptz, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL)` with `UNIQUE (identity_id)`; `reading_draft_blocks(draft_id uuid NOT NULL REFERENCES reading_drafts(id) ON DELETE CASCADE, seq int NOT NULL, page int NOT NULL, page_url text NOT NULL DEFAULT '', kind text NOT NULL, text text NOT NULL DEFAULT '', caption text NOT NULL DEFAULT '', alt text NOT NULL DEFAULT '', media_type text NOT NULL DEFAULT '', width int NOT NULL DEFAULT 0, height int NOT NULL DEFAULT 0, sha256 text NOT NULL DEFAULT '', data bytea, excluded boolean NOT NULL DEFAULT false, PRIMARY KEY (draft_id, seq))`.
- [ ] **Step 2: Domain tests first** (`draft_test.go`): PageBlocks interleaves figures after their anchor paragraph and puts -1 figures first; Summarise counts; Submission joins kept paragraphs, anchors each kept image to the previous kept paragraph's first 40 runes (or -1 when none precedes), marks the first kept image lead, skips excluded blocks, carries meta/title. Implement.
- [ ] **Step 3: Repository with integration tests first:** identity scoping on every method (another identity → ErrNotFound / nothing); one active draft (second AddDraftPage appends to the same draft); same page_url replaces that page's blocks and keeps its page number; the 11th distinct page → ErrDraftFull and nothing changed; seq is global reading order across pages (a replaced page keeps its position — implement by renumbering: delete that page's blocks, insert new ones, then renumber all blocks by (page, original order) — or store seq as page*10000+i; pick one, document it); toggle/title/delete; DraftImage returns bytes only for image blocks; SweepDrafts deletes drafts with updated_at < before (cascade). AddDraftPage and the page replace run in one pgx transaction (use `r.pool`); simple reads can be sqlc queries. Tests clean up their identities' drafts. Mirror everything in both fakes (no looser).
- [ ] **Step 4:** `make sqlc` (if queries added), `make migrate`, `make test`, `make test-integration`, `make lint`. Commit `feat(reading): drafts — pages of blocks collected on the server`.

### Task 3: Application service

**Files:** `internal/application/reading/drafts.go` (+ `drafts_test.go`), `internal/application/reading/worker.go` (sweep).

**Produces:**

```go
type DraftPage struct { Meta reading.DraftMeta; PageURL, Content, Selection string; Figures []reading.FigureDraft }
type DraftStatus struct { Draft reading.DraftInfo; Summary reading.DraftSummary; Rejected int }
func (s *Service) AddDraftPage(ctx, identity, p DraftPage) (DraftStatus, error)      // selection wins; empty text and no images → reading.ErrEmptyContent
func (s *Service) ActiveDraft(ctx, identity) (DraftStatus, error)                    // storage.ErrNotFound when none
type DraftReview struct { Draft reading.DraftInfo; Blocks []reading.DraftBlock; Summary reading.DraftSummary; MaxRunes int; TooLong, TooManyImages bool }
func (s *Service) Draft(ctx, identity, id string) (DraftReview, error)
func (s *Service) DraftImage(ctx, identity, id string, seq int) (reading.Figure, error)
func (s *Service) SetDraftBlockExcluded(ctx, identity, id string, seq int, excluded bool) (DraftReview, error)
func (s *Service) SetDraftTitle(ctx, identity, id, title string) error
func (s *Service) SendDraft(ctx, identity, id string, deliver bool) (SubmitResult, error) // Submission → Submit → DeleteDraft; draft kept on any Submit error
func (s *Service) DiscardDraft(ctx, identity, id string) error
```

- [ ] **Step 1: Tests first** (memRepo harness): add two pages → counts; selection wins; the same page twice → counts unchanged; send builds the article (paragraph order, figure anchors via the stored figures on the new article) and deletes the draft; send with all excluded → error (ErrEmptyContent) and draft kept; over MaxArticleRunes → review TooLong and SendDraft returns reading.ErrArticleTooLarge with draft kept; discard; sweep removes a draft whose updated_at is 8 days old (inject `s.now`).
- [ ] **Step 2: Implement.** MaxRunes = `s.cfg.MaxArticleRunes` or `reading.DefaultMaxArticleRunes`. The worker's `Run` sweeps once per ticker fire (`SweepDrafts(now-7d)`), logging the count when non-zero; errors are logged, never fatal.
- [ ] **Step 3:** `make test`, `make lint`. Commit `feat(reading): collect, review and send a draft through the normal pipeline`.

### Task 4: HTTP — API, review page, image route

**Files:** `internal/adapters/http/drafts.go` (+ `drafts_test.go`), `internal/adapters/http/server.go` (routes), `web/templates/reading_draft.html.tmpl` (+ a partial for one block row), `web/static/css/app.css`, `docs/api/reading.md`.

- [ ] **Step 1: Tests first:** `POST /api/v1/reading/drafts/active/parts` accepts JSON and multipart (reuse `decodeSubmit` from reading.go; the part's `url` is the page URL) → 200 `{draft_id, pages, paragraphs, images, chars, review_url}`; an 11th page → 400 with 「下書きは10ページまでです」; `GET /api/v1/reading/drafts/active` → counts or 404; `DELETE /api/v1/reading/drafts/active` → 204 (discards the active draft; 404 when none); token without `reading:write` → 403 (scope enforcement as for articles — put the routes in the same scope group); review page `GET /reading/drafts/{id}` renders blocks grouped by page with toggles, the summary line and warnings; toggle `POST /reading/drafts/{id}/blocks/{seq}/toggle` (htmx: returns the re-rendered block row plus an out-of-band summary/warnings/send-button fragment); `POST /reading/drafts/{id}/title`; `POST /reading/drafts/{id}/send` (form field `deliver=on`) → 303 to /reading/{edition}; on a Submit validation error re-render the review with the message (400) and keep the draft; `POST /reading/drafts/{id}/discard` → 303 /reading; `GET /reading/drafts/{id}/images/{seq}` → bytes, private cache, nosniff; all of them 404 for another identity and after discard (「この下書きはありません」 page for the review GET).
- [ ] **Step 2: Implement** following reading.go's patterns (s.render, readingErrorMessage for the Japanese error text, ErrDraftFull → 「下書きは10ページまでです」). Routes for /reading/drafts/* are registered with the other /reading page routes (before /reading/{id}); the API routes in the /api/v1 reading:write group. Template is phone-first: title input (saves on change via hx-post), summary line, 「1ページ目」 headings, rows with two-line clamp (CSS `-webkit-line-clamp: 2`, tap toggles a class to expand), image thumbnails, excluded rows greyed + line-through, footer with send (checkbox 「Kindleに送る」 checked) and discard (a `<details>`/second-tap confirm — no JS dialog). Document the two API endpoints in docs/api/reading.md.
- [ ] **Step 3:** `make test`, `make lint`; `make restart`; browser: create a draft with two JSON parts via fetch from a logged-in page, open the review page, toggle a block (summary updates without reload), edit title, send → edition page. Commit `feat(reading): the draft API and review page`.

### Task 5: Clients — extension, phone capture and share pages

**Files:** `chrome-extension/popup.html`, `popup.js`, `background.js`, `popup.css`, `README.md`; `web/templates/reading_capture.html.tmpl`, `web/static/js/reading-capture.js`, `web/templates/reading_share.html.tmpl`, `web/static/js/reading-share.js`; manifest version → 1.3.0.

- [ ] **Step 1: Extension.** On popup open, `GET /api/v1/reading/drafts/active`: if 200, show the draft view (status line 「下書き：{pages}ページ・段落{paragraphs}・画像{images}」, buttons 「このページを追加」, 「確認して送信」 → `chrome.tabs.create({url: base + review_url})`, 「破棄」 → a second tap to confirm, then `DELETE /api/v1/reading/drafts/active` (Task 4). If 404, the empty state gets a checkbox 「複数ページ（下書きに集める）」 (remembered in chrome.storage.sync `collectMode`) that relabels 「このページのKindle版を作成」 to 「このページを下書きに追加」. Adding a page posts the captured article (same multipart/JSON shape and image preparation as submitArticle, via JLPImaging, same retry-as-JSON on non-auth failure) to `/api/v1/reading/drafts/active/parts`, then shows the new status. background.js: when the active draft exists (check on menu click via the API) the reading menu item's title is 「JLPの下書きにこのページを追加」 and the popup is opened with `mode=draft` so it adds instead of submitting (update the title with `chrome.contextMenus.update` whenever the popup changes draft state and on startup).
- [ ] **Step 2: Phone.** `/reading/capture` card: second button 「下書きに追加（複数ページ）」; on load fetch `/api/v1/reading/drafts/active` (same origin, session) and, when a draft exists, show 「下書き：{pages}ページ」 and relabel to 「下書きに追加」 + a link 「確認して送信」 → review_url. The add posts to the parts endpoint (same image preparation), then shows the status and the review link — no navigation away. `/reading/share` card: the same second button (text only).
- [ ] **Step 3: Browser verification:** extension via the shim protocol (see chrome-extension/README.md; images from a local CORS server): collect two pages (the second with a selection), open the review page, exclude a junk paragraph and an image, send → edition with the expected text and images, Mailpit receives it. Phone capture page via the bookmarklet (injected button for user activation): add to draft, status shows, review link works. Delete then re-import an article through the extension → a fresh lesson, not 「すでにあります」. Close tabs; stop servers.
- [ ] **Step 4:** Commit `feat(reading): collect multi-page articles from the extension and the phone`.

### Task 6: Docs and ship

- [ ] README 読解 section: 「複数ページの記事」 paragraph (collect mode, review page, 10-page cap, 7-day expiry) and a line that deleting an article and importing it again starts fresh. chrome-extension/README.md: the draft flow and API endpoints.
- [ ] `make test && make test-race && make test-integration && make lint`.
- [ ] After the final review: merge to main, image, deploy (needs the learner's smartcard for the vault and git push).
