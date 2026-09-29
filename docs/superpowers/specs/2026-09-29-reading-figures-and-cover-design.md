# 読解: article images and a designed cover — design

**Goal:** a Kindle study edition carries the article's own photos,
charts and diagrams, where they appear in the text with their captions,
and opens with a designed cover built from the lead photo.

**Status:** approved in conversation 2026-09-29; awaiting review of this
written spec. First of three follow-ups to the 読解 → Kindle feature
(PR #1). The other two, agreed and ordered after this one, are out of
scope here: grouping articles from one site into a series (a spike
first — whether Send to Kindle keeps series metadata is unknown), and a
typography pass judged on a real Kindle.

## Principles carried over

- **The server never fetches the page.** Text comes from the learner's
  own logged-in tab so a subscription site works and nothing scrapes
  around a paywall. Images follow the same rule: the extension fetches
  them with the learner's session and uploads the bytes. The server
  never makes an outbound request to an article's host, which also
  keeps it free of a server-side request forgery surface.
- **Images are optional everywhere.** Nothing about a picture may stop
  an article being studied or delivered.
- **The lesson is unchanged.** Captions and images stay out of the
  article text the model sees, so the content hash, dedupe and the
  analysis are exactly as today. Captions are displayed (with furigana
  for lesson vocabulary) but no vocabulary is drawn from them.

## 1. Capture and upload

### Capture (`chrome-extension/article.js`)

`article.js` stops skipping `figure`/`figcaption` **inside the chosen
article root**. Everything outside the root — sidebars, related-story
cards — stays excluded by the existing SKIP selector and link-density
rules.

For each `<img>` in the root it records:

| Field | Source |
|---|---|
| `src` | the largest candidate: `currentSrc`, else the widest `srcset` entry, else `src`, else a lazy-load attribute (`data-src`, `data-original`) |
| `alt` | `alt` |
| `caption` | the enclosing `<figure>`'s `<figcaption>` text |
| `after_paragraph` | index of the last paragraph emitted before it, `-1` if before the first |
| `lead` | true for the first kept figure |
| `after_text` | the first 40 characters of that paragraph, so the server can anchor the figure even when its paragraph split differs from the client's |

Images whose rendered or natural size is under 200 px on either side
are skipped (tracking pixels, icons, logos, avatars). When the root has
no figure, `og:image` becomes the lead image with `in_text: false`: used
for the cover only, not placed in the text. With a selection, only the
lead image is sent, as cover-only — in-text positions refer to the whole
article.

### Fetch and downscale (`chrome-extension/popup.js`)

The popup fetches each image with the extension's host permission —
the learner's cookies apply, so paywalled and signed CDN URLs work —
with a 10 s timeout per image. Each is decoded into an
`OffscreenCanvas`, scaled to at most 1200 px on the long side, and
re-encoded:

- PNG when the source is PNG and the re-encoded PNG is under 500 KB
  (charts and diagrams stay sharp);
- otherwise JPEG, starting at quality 0.82 and stepping down (0.72,
  0.62) until it is under 2 MB.

At most 12 images, in document order. A figure that fails to fetch,
decode or fit is dropped and counted; the popup reports 「画像 3/5枚」.
A failed image never fails the import.

### Upload (`POST /api/v1/reading/articles`)

The endpoint also accepts `multipart/form-data`:

- `metadata` — today's JSON body plus a `figures` array of
  `{caption, alt, after_paragraph, lead}` in order;
- `image-0` … `image-N` — one part per `figures` entry.

The JSON body keeps working unchanged for the paste form and older
extension versions. The body limit rises to 15 MB for multipart only;
JSON stays at 1 MB. If the multipart request as a whole is rejected
(too large, malformed), the popup retries once as text-only JSON so the
lesson still happens.

`docs/api/reading.md` documents the multipart form.

### Resubmission

Submitting the same text still dedupes to the existing article (200,
no second analysis). If that article has no figures yet, the new
figures are attached to it, so re-importing a page adds its pictures
without paying for another analysis. An article that already has
figures keeps them.

## 2. Domain and storage

### Domain (`internal/domain/reading`)

```go
type Figure struct {
    Ordinal        int
    AfterParagraph int    // -1: before the first paragraph
    Caption, Alt   string
    Lead           bool
    MediaType      string // image/jpeg | image/png | image/gif
    Width, Height  int
    Data           []byte
}
```

`NewFigure` validates the way `NewArticle` validates text:

- the media type is **recognised from the bytes** (`image.DecodeConfig`),
  whatever the client claims; only JPEG, PNG and GIF are accepted;
- at most 2 MB of data;
- dimensions come from `image.DecodeConfig` before anything is decoded,
  and over 25 megapixels is refused (decompression bombs);
- caption and alt are trimmed and capped (caption 500 runes, alt 300);
- `AfterParagraph` is clamped to `[-1, len(paragraphs)-1]`;
- at most 12 figures per article, at most one lead (the first wins).

An invalid figure is an error for that figure only.

### Storage

Migration `00031_reading_figures.sql`:

```sql
CREATE TABLE reading_article_figures (
    article_id      uuid NOT NULL REFERENCES reading_articles(id),
    ordinal         int  NOT NULL,
    after_paragraph int  NOT NULL,
    caption         text NOT NULL DEFAULT '',
    alt             text NOT NULL DEFAULT '',
    is_lead         boolean NOT NULL DEFAULT false,
    in_text         boolean NOT NULL DEFAULT true,
    media_type      text NOT NULL,
    width           int  NOT NULL,
    height          int  NOT NULL,
    sha256          text NOT NULL,
    data            bytea NOT NULL,
    PRIMARY KEY (article_id, ordinal)
);
```

Images live in Postgres, so the existing restic backups cover them.
Worst case is about 2–3 MB per article.

### Port (`internal/ports/storage`, `ReadingRepository`)

- `AttachFigures(ctx, identity, articleID, figs) (attached bool, err)` —
  inserts only if the article has none, in one transaction
  (`INSERT … SELECT … WHERE NOT EXISTS`), so two racing resubmissions
  cannot both attach.
- `ListFigures(ctx, identity, articleID) ([]reading.Figure, error)` —
  everything but `Data`, for pages and polling.
- `FigureData(ctx, identity, articleID, ordinal) (reading.Figure, error)`
  — with `Data`, for serving and rendering.

All three are identity-scoped and treat a soft-deleted article's
figures as absent. Figures follow their article through soft delete and
`jlp restore reading` with no change there.

### Application (`internal/application/reading`)

`Submit` takes the figures with the draft. Figures are validated before
anything is written; invalid ones are dropped and logged with the
reason, the rest are attached after the article upsert. The analysis
worker does not touch figures. The delivery stage and the EPUB download
load them to render.

## 3. Rendering

### Port (`internal/ports/publishing`)

`Ebook` gains `Figures []reading.Figure` (with data, in order) and
`Cover []byte` (JPEG, optional). A new `CoverDesigner` port —
`Design(ctx, CoverInput) ([]byte, error)` with title, source, date and
an optional lead image — is called by the application layer before
`Render`. Nothing new is stored for the cover: like the EPUB, it is
derived each time.

### EPUB (`internal/adapters/epub`)

- `article.xhtml` places each figure after its paragraph:
  `<figure class="figure"><img src="images/fig-N.jpg" alt="…"/><figcaption>…</figcaption></figure>`.
  Captions are annotated with furigana for lesson vocabulary, as the
  body is.
- Images go in `OEBPS/images/`, each with a manifest item.
- The cover goes in as `OEBPS/images/cover.jpg`, declared with EPUB 3
  `properties="cover-image"` **and** `<meta name="cover" content="…"/>`,
  which Kindle's conversion reads. No separate cover page in the spine,
  so Kindle does not show the cover twice.
- `style.css`: images at most the page width, `page-break-inside: avoid`
  on figures, captions smaller and centred.
- Output stays deterministic and must pass EPUBCheck with 0 errors and 0
  warnings (alt text is required).

### Cover (`internal/adapters/cover`)

A 1600×2560 JPEG (Amazon's recommended 1:1.6):

- top ~60%: the lead photo, cropped to fill;
- below, on a quiet background: kicker 日本語読解, then the title in
  large type wrapped to fit (up to four lines, then ellipsised), then
  source and date;
- no lead photo: the same layout with a solid colour band instead of
  the photo.

Font: **M PLUS 1p** Bold and Regular, embedded in the binary because
titles can contain any kanji (about 3.5 MB for both). Chosen over Noto
Sans JP, which Google Fonts ships only as a variable font that
`golang.org/x/image`'s `opentype` renders at its default weight; M PLUS
1p's static TrueType weights were verified to render through it
(2026-09-29), rare kanji included. Its licence (OFL) ships alongside it.

### Web page

`/reading/{id}` shows the same figures inline, served from
`GET /reading/articles/{id}/figures/{n}`. The route is identity-scoped,
returns 404 for another identity's or a soft-deleted article, and sends
`Cache-Control: private, max-age=31536000, immutable` with an ETag of
the sha256, since the bytes never change.

The edition JSON (`GET /api/v1/reading/editions/{id}`) gains
`figure_count`, which the popup shows.

## 4. Error handling

| Where | Failure | Result |
|---|---|---|
| Extension | fetch fails (403, CORS, no host permission, 10 s timeout) | image skipped, counted in 「画像 n/m枚」 |
| Extension | decode or downscale fails, or still over 2 MB at quality 0.62 | image skipped, counted |
| Server | bad type, too large, too many pixels, bad caption data | that figure dropped and logged; article accepted |
| Server | multipart request too large or malformed | 400 with a clear message; popup retries once as JSON |
| Rendering | cover cannot be drawn (e.g. corrupt lead image) | no-photo cover |
| Rendering | a figure fails while building the EPUB | figure left out of the book, warning logged |
| Delivery | EPUB size | cannot approach Send to Kindle's 50 MB (12 × 2 MB bound) |

## 5. Testing

Test-first throughout; fakes must be no looser than production (the
lesson of the KPN `AUTH` bug).

- **Domain:** type sniffing ignores the claimed type; size, pixel and
  count limits; position clamping; caption and alt trimming; one lead;
  the content hash ignores figures.
- **Postgres integration** (`-tags integration`): `AttachFigures` runs
  once, including two racing calls; identity scoping; hidden on soft
  delete and back on restore; tests clean up their rows (they share the
  dev database with a live worker).
- **HTTP:** multipart and JSON both accepted; one bad image drops only
  that image; body limits (15 MB multipart, 1 MB JSON); the figure route
  404s for another identity and after soft delete; cache headers.
- **EPUB:** figures in order and in the right places; manifest items;
  cover declared both ways and absent from the spine; EPUBCheck in a
  container against a golden book with figures and a cover.
- **Cover:** golden-image tests for the photo and no-photo layouts, and
  a long title that wraps and ellipsises.
- **Extension, in the browser:** the real `article.js` and popup, per
  `chrome-extension/README.md`, against a live NHK article and a
  synthetic page covering lazy `srcset`, a tracking pixel, a sidebar
  image, `<figure>` with caption, and a CORS failure; then submit to the
  local stack, check `/reading/{id}` and the EPUB, and deliver to
  Mailpit.
- **One real delivery:** an edition with images sent to the learner's
  Kindle, to judge on the device. The typography pass starts from it.

## Out of scope

Series grouping and typography (agreed follow-ups); video; animated
GIFs beyond their first frame; images in the vocabulary, grammar,
close-reading or review sections; studying captions.
