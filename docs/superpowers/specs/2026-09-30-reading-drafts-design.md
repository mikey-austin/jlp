# 読解 multi-page drafts, and deletes that stay deleted — design

**Goal:** build one study edition from several captures — an article
spread over pages (National Geographic 日本版's `?P=2`), with the junk
around it left out — sent as a single Kindle edition. The one-shot
capture stays the default. And: an article the learner deleted must not
come back when they import it again.

**Status:** approved in conversation 2026-09-30.

## Why

- Paged articles (NatGeo 日本版: page 2 via a 「次ページ：…」 link to
  `?P=2`, no `rel=next`, no `<article>`/`<main>`) arrive as one page.
- Capture heuristics cannot reliably tell subscription pitches, login
  prompts and ad captions from prose (NatGeo page 1 captured 「ここから先
  は…定期購読者のみ」「ログイン」「詳細はこちら」 as paragraphs). The fix is
  a review step where the learner sees every captured block and drops
  junk — a phone-friendly alternative to precise text selection.
- `UpsertReadingArticle` answers a re-import of a soft-deleted article
  by clearing `deleted_at`, restoring the OLD article and its OLD
  edition; `Submit` then reports 「この記事の学習版はすでにあります」 and
  the deleted edition reappears.

## 1. Model and API

**One active draft per learner.** A learner has at most one open draft
— their active collection. The server tracks it, so neither the
extension nor the phone holds a draft id, and pages can be added from
either device.

**Storage** (migration `00032_reading_drafts`):

- `reading_drafts` — `id uuid`, `identity_id`, `title`, `source_name`,
  `source_url`, `author`, `published_at` (from the first page added),
  `created_at`, `updated_at`. At most one row per identity (unique
  index on `identity_id`).
- `reading_draft_blocks` — `draft_id`, `seq` (reading order across
  pages), `page` (1-based), `page_url`, `kind` (`paragraph` | `image`),
  `text` (paragraph), image fields (`caption`, `alt`, `media_type`,
  `width`, `height`, `sha256`, `data bytea`), `excluded boolean`.
  Primary key `(draft_id, seq)`.

Images are blocks in sequence, so an image keeps its place between
paragraphs. Image blocks are validated exactly as article figures are
(`reading.NewFigures`' rules: type recognised from bytes, ≤ 2 MB,
≤ 25 MP).

**API** (session cookie or a `reading:write` token, like
`POST /api/v1/reading/articles`):

- `POST /api/v1/reading/drafts/active/parts` — the same shapes as the
  article submit (JSON, or multipart `metadata` + `image-N`). Creates
  the active draft if none exists; appends the page's paragraphs (from
  `selection` when non-empty, else `content`, split by
  `reading.NormaliseParagraphs`) and its in-text images as blocks, in
  order. Returns `{draft_id, pages, paragraphs, images, chars,
  review_url}`.
- `GET /api/v1/reading/drafts/active` — the same counts, or 404.
- Guard rails: at most 10 pages per draft (「下書きは10ページまでです」);
  adding a page whose `page_url` is already in the draft **replaces**
  that page's blocks (a double tap does not duplicate). Each add is one
  transaction — nothing is half-added.

**Review and send** (JLP pages, session only):

- `GET /reading/drafts/{id}` — the review page (section 3).
- `POST /reading/drafts/{id}/blocks/{seq}/toggle` — flip `excluded`.
- `POST /reading/drafts/{id}/title` — set the title.
- `POST /reading/drafts/{id}/send` — build a `reading.Draft` from the
  kept blocks (paragraphs in order, joined with blank lines; each kept
  image a `FigureDraft` anchored by `AfterText` to the kept paragraph
  before it, `InText: true`, the first kept image the lead) plus the
  draft's metadata and title, and call the existing `Service.Submit`
  with `Deliver` from the form's checkbox (default on). Then delete the
  draft and redirect to `/reading/{edition}`. Validation, dedupe, the
  lesson, EPUB and Kindle are unchanged.
- `POST /reading/drafts/{id}/discard` — delete the draft and its blocks.
- The worker's tick sweeps drafts not updated for 7 days.

## 2. Deletes that stay deleted

- Re-importing an article whose matching row is soft-deleted **purges**
  it first, in one transaction — its deliveries, editions, figures and
  the article row — then inserts the import as a brand-new article, so
  it gets a fresh edition (and delivery if asked). Implemented in the
  repository's `UpsertArticle`: the `ON CONFLICT` path no longer
  restores a deleted row.
- Undo 「元に戻す」 after a delete keeps working until the article is
  re-imported; `jlp restore reading` likewise.
- Deleting stays where it is (/reading list and the edition page, soft
  delete with undo). A deleted edition never reappears in the list or at
  its URL, and a re-import never answers 「すでにあります」 for it.
- Drafts: discard is a hard delete (a confirming tap on the page, no
  undo); a sent draft is deleted; stale drafts expire after 7 days.

## 3. Clients

**Extension popup.**
- No draft open: 「このページのKindle版を作成」 stays the one-shot default;
  a checkbox 「複数ページ（下書きに集める）」 switches the button to
  「このページを下書きに追加」.
- A draft open (`GET …/drafts/active` on popup open): a status line
  「下書き：2ページ・段落34・画像5」 and 「このページを追加」,
  「確認して送信」 (opens the review page in a tab), 「破棄」.
- Context menu: while a draft is open, 「JLPでKindle版を作成」 reads
  「JLPの下書きにこのページを追加」 and adds without opening the popup's
  main flow (the popup opens to show the result, as today).
- What is added: the page's capture as today (a selection wins over
  the page); images fetched, downscaled and uploaded exactly as now,
  under the same permission.

**Phone.** The `/reading/capture` card gains 「下書きに追加（複数ページ）」
beside 「作成してKindleに送る」; with a draft open it shows 「下書き：2ページ」
and 「確認して送信」. The share card offers the same. The tap is still
required, so a hostile page cannot add to a draft.

## 4. The review page

Phone-first, server-rendered:

- Editable title (from the first page); source; a live summary
  「3ページ・段落 34/41・画像 5/7・12,480字」 (kept/total).
- Blocks in order under 「1ページ目」「2ページ目」…: a paragraph shows two
  lines (tap to expand) and a toggle 「除外」/「戻す」; an image shows a
  thumbnail (`GET /reading/drafts/{id}/images/{seq}`, identity-scoped)
  with its caption and the same toggle. Excluded blocks are greyed and
  struck through. Toggles post without a full reload (the app's existing
  htmx pattern).
- Warnings: over the article cap → 「長すぎます（上限 20,000字）— 段落を
  除外してください」 and send disabled; more than 12 kept images →
  「画像は最初の12枚だけ使われます」.
- Footer: 「作成してKindleに送る」 (Kindle checkbox, default on) and
  「下書きを破棄」 (confirming tap).

## Failure handling

| Situation | Result |
|---|---|
| Add fails (network, 5xx) | error in popup/card; nothing half-added; retry safe |
| Same page added twice | that page's blocks replaced |
| 11th page | 「下書きは10ページまでです」 |
| Send with nothing kept / not Japanese / too long | the server's usual message on the review page; draft kept |
| Another learner's draft or block | 404 |
| Draft gone (sent, discarded, expired) while a tab shows it | 「この下書きはありません」 |

## Testing

- Postgres integration: drafts identity-scoped; one active draft;
  same-page replace; 10-page cap; toggle; send builds the expected
  paragraphs and figure anchors; 7-day sweep; re-import of a
  soft-deleted article purges editions, deliveries and figures and
  yields a fresh edition (the reported bug). All tests clean up.
- HTTP: part API via JSON and multipart; token scope; review-page
  actions; 404 across identities; send disabled over the cap.
- Browser: a two-page collection through the extension shim with junk
  excluded on the review page, edition checked in Mailpit; the phone
  card's 「下書きに追加」; delete then re-import shows a fresh lesson.
- NatGeo's members-only page 2 is checked by the learner after deploy.

## Out of scope

Auto-following 「次ページ」 links (a later add-on: "add the next page
too"); translation of non-Japanese articles (its own design, next).
