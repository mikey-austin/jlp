# 読解 (reading) API

The JSON surface the Chrome extension's 「JLPでKindle版を作成」 drives. The
same pipeline backs the `/reading` pages; see the root README's 読解
section for how it works.

## Authentication

Every call takes either the learner's browser session or
`Authorization: Bearer <token>` for a token minted under 設定 → APIトークン
with the **`reading:write`** scope. That scope covers exactly the routes below and nothing else — it cannot read correction history,
sessions or statistics. It is separate from `sessions:write` because
every submission queues a model call.

## `POST /api/v1/reading/articles`

Submit an article. Returns immediately; the study edition is written in
the background.

```json
{
  "url": "https://jp.wsj.com/articles/…",
  "title": "政府、新たな経済対策",
  "source": "WSJ日本版",
  "author": "山田太郎",
  "published_at": "2026-09-29T08:00:00+09:00",
  "content": "本文…",
  "selection": "",
  "deliver": false
}
```

| field | |
|---|---|
| `content` | the article body; paragraphs separated by blank lines |
| `selection` | optional; when non-empty it is studied **instead of** `content` — a passage the learner highlighted wins over the extension's guess at the article body |
| `url` | optional; absolute `http(s)` only. The fragment is dropped |
| `title`, `source`, `author` | optional metadata; a missing title falls back to the start of the text |
| `published_at` | optional; RFC 3339 or `YYYY-MM-DD`. Anything else is ignored, not rejected |
| `deliver` | optional; send the edition to the Kindle as soon as it is ready. Ignored when the server has no Kindle delivery configured |

The body is limited to `APP_READING_MAXARTICLERUNES` characters (default
20 000) after normalisation, and must look like Japanese (at least a fifth
of its letters kana or kanji).

**Idempotent.** The same text (after whitespace normalisation) from the
same learner is the same article. A repeat returns the existing article
and its current edition — no second model call — unless that edition
failed, in which case a fresh one is queued.

| status | meaning |
|---|---|
| `202` | new edition queued |
| `200` | duplicate: the existing edition is returned (`"duplicate": true`); with `deliver` and a ready edition, a delivery is queued too |
| `400` | `{"error": "…"}` — empty, not Japanese, too long, bad URL, or not JSON |

```json
{
  "article": {"id": "…", "title": "…", "source": "WSJ日本版", "url": "…", "chars": 1834},
  "edition": { …see below… },
  "duplicate": false,
  "delivery": null
}
```

## `GET /api/v1/reading/editions/{id}`

An edition's progress — what the extension polls.

```json
{
  "id": "…",
  "article_id": "…",
  "status": "ready",
  "status_label": "完成",
  "terminal": true,
  "attempts": 1,
  "last_error": "",
  "vocabulary": 14,
  "page_url": "/reading/…",
  "epub_url": "/reading/…/epub",
  "delivery_enabled": true,
  "deliveries": [
    {"id": "…", "status": "sent", "status_label": "Kindleに送信済み", "attempts": 1, "created_at": "…", "sent_at": "…"}
  ],
  "created_at": "…",
  "updated_at": "…"
}
```

`status` is `pending` → `analysing` → `ready`, or `failed` after three
attempts (`last_error` says why; a failed attempt before that goes back to
`pending` with a backoff). `terminal` is true for `ready` and `failed`.
`page_url`/`epub_url` are relative — join them to the configured base URL;
`epub_url` is only present once the edition is ready. Deliveries are newest
first. `404` for an id that does not exist or belongs to someone else.

## `POST /api/v1/reading/editions/{id}/deliver`

Queue a Send-to-Kindle delivery of a ready edition.

| status | meaning |
|---|---|
| `202` | queued |
| `200` | this edition is already on its way to this address — the in-flight delivery is returned, nothing is sent twice |
| `404` | no such edition for this learner |
| `409` | the edition is not ready yet |
| `503` | Kindle delivery is not configured on this server |

## Drafts: one article from several pages

A draft collects an article page by page, server-side, so it can be
reviewed before one send. A learner has at most one active draft, of up to
10 pages; drafts untouched for 7 days are swept.

### `POST /api/v1/reading/drafts/active/parts`

Append one captured page. The body is exactly the article body above (JSON,
or multipart with images); `url` identifies the page, so sending the same
`url` again **replaces** that page rather than adding a second copy, and
`selection` wins over `content`.

```json
{"draft_id": "…", "pages": 2, "paragraphs": 14, "kept_paragraphs": 14,
 "images": 3, "kept_images": 3, "chars": 5210, "figures_rejected": 0,
 "review_url": "/reading/drafts/…"}
```

The counts are totals for the whole draft. `400` with `{"error": …}`:
`下書きは10ページまでです` for an 11th distinct page, or the usual message
for an empty body. `review_url` is a page route (session cookie), where
blocks are dropped and the draft is sent.

### `GET /api/v1/reading/drafts/active`

The same counts for the current draft; `404` when there is none.

### `DELETE /api/v1/reading/drafts/active`

Discard the current draft. `204`, or `404` when there is none.

## Submitting with images

`POST /api/v1/reading/articles` also accepts `multipart/form-data`: a
`metadata` part holding the usual JSON body plus `figures[]`, and one
`image-N` file part per figure, matched to `figures[N]` by index. Each
figure takes `caption`, `alt`, `after_paragraph`, `after_text`, `lead`
(the cover photo) and `in_text` (default `true`; send `false` for an image
used only as the cover). A `selection` keeps only the lead figure, as
cover-only, because the other positions refer to the whole article.

The multipart body is limited to 15 MiB (the JSON form stays at 1 MiB).
Images must be JPEG, PNG or GIF, at most 2 MB and 25 megapixels each, and
at most 12 per article. An image that fails those checks (or is not an
image) is dropped, not fatal: the response reports `figures` (attached) and
`figures_rejected`, and the edition has `figure_count`.

Each image is served at `GET /reading/articles/{id}/figures/{n}` — a page
route, so it needs the learner's session cookie, not an API token; another
learner's figure is a `404`.

## Example

```sh
curl -sS -X POST "$JLP/api/v1/reading/articles" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"title":"テスト","content":"中央銀行は金融引き締めを続けている。"}'

curl -sS "$JLP/api/v1/reading/editions/$EDITION" -H "Authorization: Bearer $TOKEN"
```
