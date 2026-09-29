# 読解 Article Images and Designed Cover — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Kindle study editions carry the article's own figures (in place, captioned) and open with a designed cover drawn from the lead photo.

**Architecture:** The extension captures figures from the learner's tab, fetches and downscales them itself, and uploads them multipart with the article. The server validates them in the domain (`reading.NewFigures`), stores them in Postgres beside the article, and interleaves them with the text (`reading.Layout`) for both `/reading/{id}` and the EPUB. A new `cover` adapter behind a `publishing.CoverDesigner` port draws a 1600×2560 JPEG with an embedded Japanese font. The lesson (model input, content hash, dedupe) is untouched.

**Tech Stack:** Go 1.25, pgx/v5 + sqlc + goose, chi, `golang.org/x/image` (opentype, draw), EPUB 3, Chrome MV3 (OffscreenCanvas, `chrome.permissions`).

**Spec:** `docs/superpowers/specs/2026-09-29-reading-figures-and-cover-design.md`

## Global Constraints

- The server never makes an outbound request to an article's host; image bytes only ever arrive from the extension.
- Images are optional: no image failure may fail a submission, an edition, a download or a delivery.
- The model input, `ContentHash` and dedupe are unchanged; captions are never sent to the model.
- Accepted image types: JPEG, PNG, GIF, recognised from their bytes by `image.DecodeConfig` (the domain may not import `net/http`). Per image ≤ 2 MB (`2 << 20`), ≤ 25,000,000 pixels. ≤ 12 figures per article, at most one lead.
- Caption ≤ 500 runes, alt ≤ 300 runes, both trimmed.
- JSON submit body limit stays 1 MiB; multipart submit limit is 15 MiB (`15 << 20`).
- Client downscale: long side ≤ 1200 px; PNG kept when source is PNG and result < 500 KB; else JPEG at 0.82 → 0.72 → 0.62 until < 2 MB; 10 s fetch timeout per image.
- Cover: 1600×2560 JPEG; top 1536 px photo (crop to fill) or accent band `#b7282e`; title ≤ 4 lines then `…`.
- Font: M PLUS 1p Bold/Regular (OFL), pinned to google/fonts commit `d714b17ce2379f06daf6295617f961df605dccb5`, embedded via `//go:embed` with `OFL.txt`. (Replaces the spec's Noto Sans JP: verified 2026-09-29 to render through `x/image/font/opentype`, static weights, 1.7 MB each.)
- EPUB must pass EPUBCheck 5.2.1 with 0 errors and 0 warnings; output stays deterministic.
- All dev commands go through `make` / docker compose (`make test`, `make test-integration`, `make lint`, `make sqlc`, `make migrate`). App on http://localhost:28080 on this machine.
- Tests that touch the dev database clean up their rows (`t.Cleanup`); fakes must be no looser than production.
- Commits end with the session's `Co-Authored-By` / `Claude-Session` lines.

## Review Focus

1. **Server paragraphs differ from the client's blocks** (a `<p>` with blank lines splits in two): a figure must still land after the paragraph it followed — anchored by `after_text`, index only as fallback. Pinned in Task 1.
2. **Duplicate submit racing another with images:** exactly one set attaches, never a mix. Pinned in Task 2.
3. **Another learner's figure URL** (`/reading/articles/{id}/figures/0` with a guessed id) and **a soft-deleted article's figure**: 404, never bytes. Pinned in Tasks 2 and 6.
4. **Extension without the image permission:** the import still succeeds text-only and the popup says why. Pinned in Task 8.
5. **A lead image that is corrupt or not an image at render time:** the cover falls back to the no-photo design and the EPUB still builds. Pinned in Task 3.

---

## File map

| File | Responsibility |
|---|---|
| `internal/domain/reading/figure.go` (new) | `FigureDraft`, `Figure`, `NewFigures`, limits, errors |
| `internal/domain/reading/layout.go` (new) | `Block`, `Layout` — paragraphs and figures in reading order |
| `internal/domain/reading/article.go` | `Draft.Figures` |
| `internal/adapters/postgres/migrationsfs/00031_reading_figures.sql` (new) | table |
| `db/queries/reading.sql` | three queries |
| `internal/ports/storage/reading.go` | three port methods |
| `internal/adapters/postgres/reading.go` | implementations |
| `internal/ports/publishing/publishing.go` | `Ebook.Figures/Cover`, `CoverDesigner`, `CoverInput` |
| `internal/application/reading/service.go` | attach on submit, figures in detail, render with figures + cover, `Figure` lookup |
| `internal/adapters/cover/` (new) | `Designer`, fonts, wrapping |
| `internal/adapters/epub/` | figures, cover, CSS |
| `internal/adapters/http/reading.go` | multipart submit, figure route, detail blocks, DTO fields |
| `web/templates/reading_detail.html.tmpl`, `web/static/css/app.css` | figures on the page |
| `cmd/jlp/main.go` | wire the cover designer |
| `chrome-extension/article.js`, `popup.js`, `options.*`, `shim/chrome-shim.js`, `README.md` | capture, fetch, downscale, upload, permission |
| `docs/api/reading.md` | multipart contract |
| `Makefile` | `vendor-cover-fonts`, `epubcheck-sample` |

---

### Task 1: Domain — figures and layout

**Files:**
- Create: `internal/domain/reading/figure.go`, `internal/domain/reading/layout.go`, `internal/domain/reading/figure_test.go`
- Modify: `internal/domain/reading/article.go` (Draft)

**Interfaces:**
- Produces:
  - `type FigureDraft struct { Caption, Alt, AfterText string; AfterParagraph int; Lead, InText bool; Data []byte }`
  - `type Figure struct { ArticleID string; Ordinal, AfterParagraph int; Caption, Alt string; Lead, InText bool; MediaType string; Width, Height int; SHA256 string; Data []byte }`
  - `func NewFigures(drafts []FigureDraft, paragraphs []string) (figs []Figure, rejected []error)`
  - `const MaxFigures = 12; MaxFigureBytes = 2 << 20; MaxFigurePixels = 25_000_000`
  - `var ErrFigureType, ErrFigureSize, ErrFigurePixels, ErrFigureDecode error`
  - `type Block struct { Paragraph []Segment; Figure *Figure; Caption []Segment }`
  - `func Layout(paragraphs []string, figs []Figure, vocab []VocabularyItem) []Block`
  - `Draft.Figures []FigureDraft` (ignored by `NewArticle` and `ContentHash`)

- [ ] **Step 1: Write the failing tests** — `internal/domain/reading/figure_test.go`:

```go
package reading

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"
)

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var paras = []string{"第一段落です。", "第二段落です。", "第三段落です。"}

func TestNewFiguresSniffsTypeAndMeasures(t *testing.T) {
	var g bytes.Buffer
	_ = gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 300, 200), []color.Color{color.Black, color.White}), nil)
	figs, rejected := NewFigures([]FigureDraft{
		{Data: testJPEG(t, 640, 480), AfterParagraph: 0, InText: true},
		{Data: testPNG(t, 300, 300), AfterParagraph: 1, InText: true},
		{Data: g.Bytes(), AfterParagraph: 2, InText: true},
	}, paras)
	if len(rejected) != 0 {
		t.Fatalf("rejected: %v", rejected)
	}
	want := []string{"image/jpeg", "image/png", "image/gif"}
	for i, f := range figs {
		if f.MediaType != want[i] || f.Ordinal != i || f.SHA256 == "" {
			t.Fatalf("fig %d = %+v", i, f)
		}
	}
	if figs[0].Width != 640 || figs[0].Height != 480 {
		t.Fatalf("dimensions = %dx%d", figs[0].Width, figs[0].Height)
	}
}

func TestNewFiguresRejectsBadImagesIndividually(t *testing.T) {
	huge := make([]byte, MaxFigureBytes+1)
	copy(huge, testJPEG(t, 10, 10))
	// A PNG header claiming 6000×6000 = 36 MP, over the pixel cap; the
	// body is never decoded, so it need not be valid.
	bomb := testPNG(t, 1, 1)
	bomb[16], bomb[17], bomb[18], bomb[19] = 0, 0, 0x17, 0x70 // width 6000
	bomb[20], bomb[21], bomb[22], bomb[23] = 0, 0, 0x17, 0x70 // height 6000
	figs, rejected := NewFigures([]FigureDraft{
		{Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
		{Data: huge},
		{Data: bomb},
		{Data: append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{0}, 64)...)}, // JPEG magic, no image
		{Data: testJPEG(t, 400, 300), InText: true},
	}, paras)
	if len(figs) != 1 || figs[0].Ordinal != 0 {
		t.Fatalf("kept = %+v; the good one must survive, renumbered from 0", figs)
	}
	wantErrs := []error{ErrFigureType, ErrFigureSize, ErrFigurePixels, ErrFigureDecode}
	if len(rejected) != len(wantErrs) {
		t.Fatalf("rejected = %v", rejected)
	}
	for i, e := range wantErrs {
		if !errors.Is(rejected[i], e) {
			t.Fatalf("rejected[%d] = %v, want %v", i, rejected[i], e)
		}
	}
}

func TestNewFiguresIgnoresClaimedTypeAndCapsCountAndLead(t *testing.T) {
	var drafts []FigureDraft
	for i := 0; i < MaxFigures+3; i++ {
		drafts = append(drafts, FigureDraft{Data: testJPEG(t, 300, 300), Lead: true, InText: true,
			Caption: "  " + strings.Repeat("字", 600) + "  ", Alt: strings.Repeat("a", 400)})
	}
	figs, _ := NewFigures(drafts, paras)
	if len(figs) != MaxFigures {
		t.Fatalf("kept %d, want %d", len(figs), MaxFigures)
	}
	leads := 0
	for _, f := range figs {
		if f.Lead {
			leads++
		}
	}
	if leads != 1 || !figs[0].Lead {
		t.Fatalf("leads = %d (first lead wins)", leads)
	}
	if n := len([]rune(figs[0].Caption)); n != 500 {
		t.Fatalf("caption runes = %d", n)
	}
	if n := len([]rune(figs[0].Alt)); n != 300 {
		t.Fatalf("alt runes = %d", n)
	}
}

// The client counts its own blocks; the server may split one of them in
// two (a <p> holding a blank line). after_text anchors the figure to the
// paragraph it really followed; the index is only a fallback.
func TestNewFiguresAnchorsByTextThenClampsIndex(t *testing.T) {
	server := []string{"見出し", "前半です。", "後半です。", "結びです。"}
	figs, _ := NewFigures([]FigureDraft{
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: 1, AfterText: "後半です"},
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: 99},
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: -7},
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: 0, AfterText: "どこにもない"},
	}, server)
	got := []int{figs[0].AfterParagraph, figs[1].AfterParagraph, figs[2].AfterParagraph, figs[3].AfterParagraph}
	want := []int{2, 3, -1, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("positions = %v, want %v", got, want)
		}
	}
}

func TestContentHashIgnoresFigures(t *testing.T) {
	d := Draft{Title: "t", Content: "本文です。日本語の記事。"}
	a1, err := NewArticle("a", "me", d, 0, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	d.Figures = []FigureDraft{{Data: testJPEG(t, 300, 300)}}
	a2, _ := NewArticle("b", "me", d, 0, time.Unix(0, 0))
	if a1.ContentHash != a2.ContentHash {
		t.Fatal("figures must not change the content hash (dedupe)")
	}
}

func TestLayoutInterleavesInTextFiguresOnly(t *testing.T) {
	figs := []Figure{
		{Ordinal: 0, AfterParagraph: -1, InText: true, Caption: "冒頭の写真"},
		{Ordinal: 1, AfterParagraph: 1, InText: true, Caption: "金融政策の図"},
		{Ordinal: 2, AfterParagraph: 1, InText: true},
		{Ordinal: 3, AfterParagraph: 0, InText: false, Lead: true}, // og:image: cover only
	}
	vocab := []VocabularyItem{{Expression: "金融", Reading: "きんゆう", MeaningEN: "finance"}}
	blocks := Layout(paras, figs, vocab)
	var shape []string
	for _, b := range blocks {
		if b.Figure != nil {
			shape = append(shape, "F"+string(rune('0'+b.Figure.Ordinal)))
		} else {
			shape = append(shape, "P")
		}
	}
	if got := strings.Join(shape, " "); got != "F0 P P F1 F2 P" {
		t.Fatalf("layout = %s", got)
	}
	var ruby bool
	for _, s := range blocks[3].Caption {
		if s.Vocab && s.Reading == "きんゆう" {
			ruby = true
		}
	}
	if !ruby {
		t.Fatalf("caption not annotated: %+v", blocks[3].Caption)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `make test 2>&1 | grep -A5 'domain/reading'`
Expected: build failure — `undefined: FigureDraft`, `NewFigures`, `Layout`, `Draft.Figures`.

- [ ] **Step 3: Implement** — `internal/domain/reading/figure.go`:

```go
package reading

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // registers the format for DecodeConfig
	_ "image/jpeg" // registers the format for DecodeConfig
	_ "image/png"  // registers the format for DecodeConfig
	"strings"
	"unicode/utf8"
)

// Figure limits (spec: 読解 article images). The client downscales well
// inside them; these are the server's word, whatever a client sends.
const (
	MaxFigures      = 12
	MaxFigureBytes  = 2 << 20
	MaxFigurePixels = 25_000_000
	maxCaptionRunes = 500
	maxAltRunes     = 300
)

var (
	ErrFigureType   = errors.New("reading: figure is not a JPEG, PNG or GIF")
	ErrFigureSize   = errors.New("reading: figure is over 2 MB")
	ErrFigurePixels = errors.New("reading: figure has too many pixels")
	ErrFigureDecode = errors.New("reading: figure could not be read")
)

// FigureDraft is one image as the extension sent it. AfterText is the
// start of the paragraph the image followed; it anchors the figure when
// the server's paragraphs differ from the client's blocks. InText false
// marks a cover-only image (the page's og:image when the article body
// had none).
type FigureDraft struct {
	Caption, Alt, AfterText string
	AfterParagraph          int
	Lead, InText            bool
	Data                    []byte
}

// Figure is a validated image belonging to an article.
type Figure struct {
	ArticleID      string
	Ordinal        int
	AfterParagraph int // -1: before the first paragraph
	Caption, Alt   string
	Lead, InText   bool
	MediaType      string
	Width, Height  int
	SHA256         string
	Data           []byte // nil when listed without bytes
}

// NewFigures validates drafts against the article's paragraphs. A bad
// draft is rejected on its own — reported in rejected, in draft order —
// and never fails the rest. Kept figures are renumbered from 0, capped
// at MaxFigures, and carry at most one lead (the first).
func NewFigures(drafts []FigureDraft, paragraphs []string) (figs []Figure, rejected []error) {
	lead := false
	for i, d := range drafts {
		if len(figs) == MaxFigures {
			break
		}
		f, err := newFigure(d, paragraphs)
		if err != nil {
			rejected = append(rejected, fmt.Errorf("figure %d: %w", i, err))
			continue
		}
		f.Ordinal = len(figs)
		if f.Lead {
			f.Lead, lead = !lead, true
		}
		figs = append(figs, f)
	}
	return figs, rejected
}

func newFigure(d FigureDraft, paragraphs []string) (Figure, error) {
	if len(d.Data) > MaxFigureBytes {
		return Figure{}, ErrFigureSize
	}
	// DecodeConfig recognises the format by its magic bytes, whatever the
	// client claimed; only the three registered above are known to it.
	// (net/http's DetectContentType would do the same, but the domain
	// may not import net/http — .golangci.yml domain-purity.)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(d.Data))
	if errors.Is(err, image.ErrFormat) {
		return Figure{}, ErrFigureType
	}
	if err != nil {
		return Figure{}, fmt.Errorf("%w: %v", ErrFigureDecode, err)
	}
	mt := "image/" + format // jpeg, png or gif
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Figure{}, ErrFigureDecode
	}
	if cfg.Width*cfg.Height > MaxFigurePixels {
		return Figure{}, ErrFigurePixels
	}
	sum := sha256.Sum256(d.Data)
	return Figure{
		AfterParagraph: anchor(d, paragraphs),
		Caption:        clipRunes(oneLine(d.Caption), maxCaptionRunes),
		Alt:            clipRunes(oneLine(d.Alt), maxAltRunes),
		Lead:           d.Lead,
		InText:         d.InText,
		MediaType:      mt,
		Width:          cfg.Width,
		Height:         cfg.Height,
		SHA256:         hex.EncodeToString(sum[:]),
		Data:           d.Data,
	}, nil
}

// anchor finds the paragraph the figure followed: the first whose text
// begins with AfterText, else AfterParagraph clamped to the article.
func anchor(d FigureDraft, paragraphs []string) int {
	if t := oneLine(d.AfterText); t != "" {
		for i, p := range paragraphs {
			if strings.HasPrefix(p, t) {
				return i
			}
		}
	}
	return min(max(d.AfterParagraph, -1), len(paragraphs)-1)
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
```

`internal/domain/reading/layout.go`:

```go
package reading

// Block is one piece of an article body in reading order: a paragraph,
// or a figure with its annotated caption.
type Block struct {
	Paragraph []Segment
	Figure    *Figure
	Caption   []Segment
}

// Layout interleaves the in-text figures with the annotated paragraphs:
// figures anchored before the first paragraph lead, the rest follow
// their paragraph in ordinal order. Cover-only figures are left out.
// Both /reading/{id} and the EPUB render from it, so they agree.
func Layout(paragraphs []string, figs []Figure, vocab []VocabularyItem) []Block {
	body := Annotate(paragraphs, vocab)
	after := map[int][]Figure{}
	for _, f := range figs {
		if f.InText {
			after[f.AfterParagraph] = append(after[f.AfterParagraph], f)
		}
	}
	var out []Block
	emit := func(i int) {
		for _, f := range after[i] {
			f := f
			b := Block{Figure: &f}
			if f.Caption != "" {
				b.Caption = Annotate([]string{f.Caption}, vocab)[0]
			}
			out = append(out, b)
		}
	}
	emit(-1)
	for i, p := range body {
		out = append(out, Block{Paragraph: p})
		emit(i)
	}
	return out
}
```

In `article.go`, add to `Draft` (after `Content`):

```go
	// Figures are the article's images. They never reach NewArticle's
	// text or ContentHash: an article is the same article with or
	// without its pictures.
	Figures []FigureDraft
```

- [ ] **Step 4: Run to verify pass**

Run: `make test 2>&1 | grep -E 'domain/reading|FAIL'`
Expected: `ok  github.com/mikeyaustin/jlp/internal/domain/reading`.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/reading
git commit -m "feat(reading): figures in the domain — validated, anchored and laid out"
```

---

### Task 2: Storage — table, queries, repository

**Files:**
- Create: `internal/adapters/postgres/migrationsfs/00031_reading_figures.sql`
- Modify: `db/queries/reading.sql`, `internal/ports/storage/reading.go`, `internal/adapters/postgres/reading.go`, `internal/adapters/postgres/reading_test.go`, `internal/application/reading/service_test.go` (memRepo), `internal/adapters/http/reading_test.go` (fakeReadingRepo)
- Generated: `internal/adapters/postgres/sqlcgen/*` via `make sqlc`

**Interfaces:**
- Consumes: `reading.Figure` (Task 1)
- Produces (on `storage.ReadingRepository`):
  - `AttachFigures(ctx context.Context, identity learner.IdentityID, articleID string, figs []reading.Figure) (attached bool, err error)`
  - `ListFigures(ctx context.Context, identity learner.IdentityID, articleID string) ([]reading.Figure, error)` — `Data` nil
  - `FigureData(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error)` — `storage.ErrNotFound` when absent, foreign or soft-deleted

- [ ] **Step 1: Migration** — `00031_reading_figures.sql`:

```sql
-- +goose Up
-- An article's images, captured by the extension from the learner's own
-- tab and uploaded with the text (the server never fetches a page).
-- Bytes live here rather than on disk so the existing restic backup of
-- this database covers them; an article carries at most 12, each at
-- most 2 MB after the client's downscale.
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
-- +goose Down
DROP TABLE reading_article_figures;
```

- [ ] **Step 2: Queries** — append to `db/queries/reading.sql`:

```sql
-- name: AttachReadingFigures :execrows
-- One statement, so a set of figures lands whole or not at all. NOT
-- EXISTS makes a resubmission a no-op once an article has figures; two
-- racing attaches that both pass it collide on the primary key, and the
-- loser's whole statement fails — the repository reads that as "already
-- attached". Scoped to a visible article of this identity.
INSERT INTO reading_article_figures (article_id, ordinal, after_paragraph, caption, alt, is_lead, in_text, media_type, width, height, sha256, data)
SELECT a.id, f.ordinal, f.after_paragraph, f.caption, f.alt, f.is_lead, f.in_text, f.media_type, f.width, f.height, f.sha256, f.data
FROM reading_articles a,
     unnest(sqlc.arg(ordinals)::int[], sqlc.arg(after_paragraphs)::int[], sqlc.arg(captions)::text[], sqlc.arg(alts)::text[],
            sqlc.arg(leads)::boolean[], sqlc.arg(in_texts)::boolean[], sqlc.arg(media_types)::text[],
            sqlc.arg(widths)::int[], sqlc.arg(heights)::int[], sqlc.arg(sha256s)::text[], sqlc.arg(datas)::bytea[])
       AS f(ordinal, after_paragraph, caption, alt, is_lead, in_text, media_type, width, height, sha256, data)
WHERE a.id = sqlc.arg(article_id) AND a.identity_id = sqlc.arg(identity_id) AND a.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM reading_article_figures x WHERE x.article_id = a.id);

-- name: ListReadingFigures :many
SELECT f.ordinal, f.after_paragraph, f.caption, f.alt, f.is_lead, f.in_text, f.media_type, f.width, f.height, f.sha256
FROM reading_article_figures f
JOIN reading_articles a ON a.id = f.article_id
WHERE f.article_id = $1 AND a.identity_id = $2 AND a.deleted_at IS NULL
ORDER BY f.ordinal;

-- name: GetReadingFigure :one
SELECT f.ordinal, f.after_paragraph, f.caption, f.alt, f.is_lead, f.in_text, f.media_type, f.width, f.height, f.sha256, f.data
FROM reading_article_figures f
JOIN reading_articles a ON a.id = f.article_id
WHERE f.article_id = $1 AND a.identity_id = $2 AND a.deleted_at IS NULL AND f.ordinal = $3;
```

Run: `make sqlc && git status --short internal/adapters/postgres/sqlcgen`
Expected: `reading.sql.go` modified with `AttachReadingFigures`, `ListReadingFigures`, `GetReadingFigure`. If sqlc cannot type the multi-array `unnest` (error mentioning `unnest`), keep the two `SELECT` queries in sqlc and implement `AttachFigures` directly with the same SQL through the pool: give `ReadingRepository` a `pool *pgxpool.Pool` field set in `NewReadingRepository`, and call `pool.Exec` with `$1…$13` in the argument order above.

- [ ] **Step 3: Port** — add to `storage.ReadingRepository` after `RestoreArticle`:

```go
	// AttachFigures stores figs as articleID's figures unless it already
	// has some; attached reports which. All or nothing, and safe against
	// a racing attach. ErrNotFound-free: a foreign or deleted article
	// simply attaches nothing.
	AttachFigures(ctx context.Context, identity learner.IdentityID, articleID string, figs []reading.Figure) (attached bool, err error)
	// ListFigures returns a visible article's figures in order, without
	// their bytes.
	ListFigures(ctx context.Context, identity learner.IdentityID, articleID string) ([]reading.Figure, error)
	// FigureData returns one figure with its bytes, or ErrNotFound.
	FigureData(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error)
```

- [ ] **Step 4: Failing integration tests** — append to `internal/adapters/postgres/reading_test.go` (uses `readingTestSetup`, whose cleanup must also delete figures: add `"reading_article_figures"` handling — figures have no `identity_id`, so delete them first with `DELETE FROM reading_article_figures WHERE article_id IN (SELECT id FROM reading_articles WHERE identity_id = ANY($1))`):

```go
func testFigures(n int) []reading.Figure {
	var out []reading.Figure
	for i := 0; i < n; i++ {
		out = append(out, reading.Figure{Ordinal: i, AfterParagraph: i - 1, Caption: "図" + strconv.Itoa(i), InText: true, Lead: i == 0,
			MediaType: "image/jpeg", Width: 400, Height: 300, SHA256: strconv.Itoa(i), Data: []byte{0xFF, 0xD8, byte(i)}})
	}
	return out
}

func TestReadingFiguresAttachOnceScopedAndHidden(t *testing.T) {
	r, me, them := readingTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a, _, err := r.UpsertArticle(ctx, testArticle(t, me, "図のある記事です。日本語の本文。", now))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.AttachFigures(ctx, them, a.ID, testFigures(2)); err != nil || ok {
		t.Fatalf("foreign attach = %v %v, want false nil", ok, err)
	}
	if ok, err := r.AttachFigures(ctx, me, a.ID, testFigures(2)); err != nil || !ok {
		t.Fatalf("attach = %v %v", ok, err)
	}
	if ok, err := r.AttachFigures(ctx, me, a.ID, testFigures(3)); err != nil || ok {
		t.Fatalf("second attach = %v %v, want false nil", ok, err)
	}
	list, err := r.ListFigures(ctx, me, a.ID)
	if err != nil || len(list) != 2 || list[1].Caption != "図1" || list[0].Data != nil || !list[0].Lead {
		t.Fatalf("list = %+v %v", list, err)
	}
	f, err := r.FigureData(ctx, me, a.ID, 1)
	if err != nil || !bytes.Equal(f.Data, []byte{0xFF, 0xD8, 1}) {
		t.Fatalf("data = %+v %v", f, err)
	}
	if _, err := r.FigureData(ctx, them, a.ID, 1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("foreign data err = %v", err)
	}
	if err := r.SoftDeleteArticle(ctx, me, a.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FigureData(ctx, me, a.ID, 1); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted article's figure err = %v", err)
	}
	if l, _ := r.ListFigures(ctx, me, a.ID); len(l) != 0 {
		t.Fatalf("deleted article lists %d figures", len(l))
	}
	if err := r.RestoreArticle(ctx, me, a.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := r.ListFigures(ctx, me, a.ID); len(l) != 2 {
		t.Fatalf("restored article lists %d figures", len(l))
	}
}

func TestReadingFiguresRacingAttachLandsOneSet(t *testing.T) {
	r, me, _ := readingTestSetup(t)
	ctx := context.Background()
	a, _, err := r.UpsertArticle(ctx, testArticle(t, me, "競合する記事です。日本語の本文。", time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := r.AttachFigures(ctx, me, a.ID, testFigures(2+i%3))
			if err != nil {
				t.Errorf("attach %d: %v", i, err)
			}
			results[i] = ok
		}(i)
	}
	wg.Wait()
	won := 0
	for _, ok := range results {
		if ok {
			won++
		}
	}
	list, _ := r.ListFigures(ctx, me, a.ID)
	if won != 1 {
		t.Fatalf("%d attaches reported success, want exactly 1", won)
	}
	for i, f := range list {
		if f.Ordinal != i {
			t.Fatalf("figures are a mix of sets: %+v", list)
		}
	}
}
```

Run: `make test-integration 2>&1 | grep -E 'postgres|FAIL'`
Expected: FAIL (methods undefined).

- [ ] **Step 5: Implement** in `internal/adapters/postgres/reading.go` (field names follow what `make sqlc` generated — check `sqlcgen/reading.sql.go`):

```go
func (r *ReadingRepository) AttachFigures(ctx context.Context, identity learner.IdentityID, articleID string, figs []reading.Figure) (bool, error) {
	if len(figs) == 0 {
		return false, nil
	}
	id, err := parseUUID(articleID)
	if err != nil {
		return false, nil
	}
	p := sqlcgen.AttachReadingFiguresParams{ArticleID: id, IdentityID: string(identity)}
	for _, f := range figs {
		p.Ordinals = append(p.Ordinals, int32(f.Ordinal))
		p.AfterParagraphs = append(p.AfterParagraphs, int32(f.AfterParagraph))
		p.Captions = append(p.Captions, f.Caption)
		p.Alts = append(p.Alts, f.Alt)
		p.Leads = append(p.Leads, f.Lead)
		p.InTexts = append(p.InTexts, f.InText)
		p.MediaTypes = append(p.MediaTypes, f.MediaType)
		p.Widths = append(p.Widths, int32(f.Width))
		p.Heights = append(p.Heights, int32(f.Height))
		p.Sha256s = append(p.Sha256s, f.SHA256)
		p.Datas = append(p.Datas, f.Data)
	}
	n, err := r.q.AttachReadingFigures(ctx, p)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, nil // a racing attach won
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *ReadingRepository) ListFigures(ctx context.Context, identity learner.IdentityID, articleID string) ([]reading.Figure, error) {
	id, err := parseUUID(articleID)
	if err != nil {
		return nil, nil
	}
	rows, err := r.q.ListReadingFigures(ctx, sqlcgen.ListReadingFiguresParams{ArticleID: id, IdentityID: string(identity)})
	if err != nil {
		return nil, err
	}
	out := make([]reading.Figure, 0, len(rows))
	for _, x := range rows {
		out = append(out, reading.Figure{ArticleID: articleID, Ordinal: int(x.Ordinal), AfterParagraph: int(x.AfterParagraph),
			Caption: x.Caption, Alt: x.Alt, Lead: x.IsLead, InText: x.InText, MediaType: x.MediaType,
			Width: int(x.Width), Height: int(x.Height), SHA256: x.Sha256})
	}
	return out, nil
}

func (r *ReadingRepository) FigureData(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error) {
	id, err := parseUUID(articleID)
	if err != nil {
		return reading.Figure{}, storage.ErrNotFound
	}
	x, err := r.q.GetReadingFigure(ctx, sqlcgen.GetReadingFigureParams{ArticleID: id, IdentityID: string(identity), Ordinal: int32(ordinal)})
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.Figure{}, storage.ErrNotFound
	}
	if err != nil {
		return reading.Figure{}, err
	}
	return reading.Figure{ArticleID: articleID, Ordinal: int(x.Ordinal), AfterParagraph: int(x.AfterParagraph),
		Caption: x.Caption, Alt: x.Alt, Lead: x.IsLead, InText: x.InText, MediaType: x.MediaType,
		Width: int(x.Width), Height: int(x.Height), SHA256: x.Sha256, Data: x.Data}, nil
}
```

Add `"github.com/jackc/pgx/v5/pgconn"` to the imports.

- [ ] **Step 6: Fakes** — both in-memory repos (`memRepo` in `internal/application/reading/service_test.go`, `fakeReadingRepo` in `internal/adapters/http/reading_test.go`) get a `figures map[string][]reading.Figure` (initialise it in their constructors) and these methods, honouring identity and soft delete like their `GetArticle`:

```go
func (m *memRepo) AttachFigures(_ context.Context, id learner.IdentityID, aid string, figs []reading.Figure) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] || len(m.figures[aid]) > 0 || len(figs) == 0 {
		return false, nil
	}
	m.figures[aid] = append([]reading.Figure(nil), figs...)
	return true, nil
}

func (m *memRepo) ListFigures(_ context.Context, id learner.IdentityID, aid string) ([]reading.Figure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] {
		return nil, nil
	}
	out := make([]reading.Figure, 0, len(m.figures[aid]))
	for _, f := range m.figures[aid] {
		f.Data = nil
		out = append(out, f)
	}
	return out, nil
}

func (m *memRepo) FigureData(_ context.Context, id learner.IdentityID, aid string, ordinal int) (reading.Figure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] {
		return reading.Figure{}, storage.ErrNotFound
	}
	for _, f := range m.figures[aid] {
		if f.Ordinal == ordinal {
			return f, nil
		}
	}
	return reading.Figure{}, storage.ErrNotFound
}
```

(Use the receiver/field names each fake already has; if `memRepo` keys `deleted` differently, follow its `GetArticle`.)

- [ ] **Step 7: Verify**

Run: `make migrate && make test && make test-integration 2>&1 | grep -E 'FAIL|ok .*postgres'`
Expected: all `ok`, no `FAIL`.

- [ ] **Step 8: Commit**

```bash
git add internal/adapters/postgres db/queries internal/ports/storage internal/application/reading/service_test.go internal/adapters/http/reading_test.go
git commit -m "feat(reading): store article figures beside the article"
```

---

### Task 3: Publishing port and application service

**Files:**
- Modify: `internal/ports/publishing/publishing.go`, `internal/application/reading/service.go`, `internal/application/reading/service_test.go`

**Interfaces:**
- Consumes: Task 1 types; Task 2 repository methods.
- Produces:
  - `publishing.Ebook` gains `Figures []reading.Figure` (with `Data`) and `Cover []byte`
  - `type CoverInput struct { Title, Source, Date string; Photo []byte }`
  - `type CoverDesigner interface { Design(ctx context.Context, in CoverInput) ([]byte, error) }`
  - `appreading.Deps.Cover publishing.CoverDesigner` (nil: no cover)
  - `SubmitResult.Figures int` (figures the article has after submit), `SubmitResult.FiguresRejected int`
  - `EditionDetail.Figures []reading.Figure` (no data)
  - `func (s *Service) Figure(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error)`

- [ ] **Step 1: Port** — in `publishing.go`, extend `Ebook` and add the cover port:

```go
	// Figures are the article's images with their bytes, in order. The
	// renderer places the in-text ones (see reading.Layout).
	Figures []reading.Figure
	// Cover is a JPEG to use as the book's cover; nil for none.
	Cover []byte
}

// CoverInput is what a cover is drawn from.
type CoverInput struct {
	Title, Source, Date string
	// Photo is the lead image (JPEG, PNG or GIF); nil draws the
	// no-photo design.
	Photo []byte
}

// CoverDesigner draws a book cover. Separate from Renderer so the EPUB
// adapter never deals in fonts, and a failed cover can fall back without
// failing the book.
type CoverDesigner interface {
	Design(ctx context.Context, in CoverInput) ([]byte, error)
}
```

- [ ] **Step 2: Failing tests** — append to `service_test.go`. If `harness` does not already keep its `*fakeRenderer`, add a `renderer *fakeRenderer` field and pass it as `Renderer` in `newHarness`. Add a `fakeCover` and make `fakeRenderer` record the last `publishing.Ebook` it got (add `last publishing.Ebook` to it and set it in `Render`). Wire `Cover: h.cover` in `newHarness` (new `cover *fakeCover` field, created with `&fakeCover{}`).

```go
type fakeCover struct {
	calls []publishing.CoverInput
	fail  func(in publishing.CoverInput) bool
}

func (c *fakeCover) Design(_ context.Context, in publishing.CoverInput) ([]byte, error) {
	c.calls = append(c.calls, in)
	if c.fail != nil && c.fail(in) {
		return nil, errors.New("cover: bad photo")
	}
	return []byte("COVER"), nil
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSubmitAttachesFiguresAndResubmitAddsThemOnce(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	d := reading.Draft{Title: "経済対策", Content: "政府は新たな経済対策をまとめた。\n\n物価高への対応が柱となる。"}
	res, err := h.svc.Submit(ctx, "me", d, SubmitOptions{})
	if err != nil || res.Figures != 0 {
		t.Fatalf("text-only submit = %+v %v", res, err)
	}
	d.Figures = []reading.FigureDraft{
		{Data: jpegBytes(t), InText: true, Lead: true, Caption: "記者会見"},
		{Data: []byte("not an image")},
	}
	res2, err := h.svc.Submit(ctx, "me", d, SubmitOptions{})
	if err != nil || !res2.Duplicate || res2.Figures != 1 || res2.FiguresRejected != 1 {
		t.Fatalf("resubmit with figures = %+v %v", res2, err)
	}
	d.Figures = []reading.FigureDraft{{Data: jpegBytes(t), InText: true}, {Data: jpegBytes(t), InText: true}}
	res3, _ := h.svc.Submit(ctx, "me", d, SubmitOptions{})
	if res3.Figures != 1 {
		t.Fatalf("an article that has figures keeps them: %+v", res3)
	}
}

func TestRenderPassesFiguresAndCover(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	res, _ := h.svc.Submit(ctx, "me", reading.Draft{Title: "経済対策", SourceName: "NHK", Content: "政府は新たな経済対策をまとめた。",
		Figures: []reading.FigureDraft{{Data: jpegBytes(t), InText: true, Lead: true}}}, SubmitOptions{})
	h.svc.Drain(ctx)
	if _, err := h.svc.RenderEbook(ctx, "me", res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	got := h.renderer.last
	if len(got.Figures) != 1 || len(got.Figures[0].Data) == 0 || string(got.Cover) != "COVER" {
		t.Fatalf("ebook figures=%d cover=%q", len(got.Figures), got.Cover)
	}
	if in := h.cover.calls[0]; in.Title != "経済対策" || in.Source != "NHK" || len(in.Photo) == 0 {
		t.Fatalf("cover input = %+v", in)
	}
}

func TestRenderFallsBackToNoPhotoCoverThenNoCover(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	res, _ := h.svc.Submit(ctx, "me", reading.Draft{Title: "t", Content: "政府は新たな経済対策をまとめた。",
		Figures: []reading.FigureDraft{{Data: jpegBytes(t), InText: true, Lead: true}}}, SubmitOptions{})
	h.svc.Drain(ctx)
	h.cover.fail = func(in publishing.CoverInput) bool { return in.Photo != nil }
	if _, err := h.svc.RenderEbook(ctx, "me", res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.cover.calls); n != 2 || h.cover.calls[1].Photo != nil || string(h.renderer.last.Cover) != "COVER" {
		t.Fatalf("calls=%d cover=%q: want a photo attempt then a no-photo one", n, h.renderer.last.Cover)
	}
	h.cover.fail = func(publishing.CoverInput) bool { return true }
	if _, err := h.svc.RenderEbook(ctx, "me", res.Edition.ID); err != nil {
		t.Fatalf("a failed cover must not fail the book: %v", err)
	}
	if h.renderer.last.Cover != nil {
		t.Fatal("no cover expected after both attempts failed")
	}
}

func TestFigureIsIdentityScoped(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	res, _ := h.svc.Submit(ctx, "me", reading.Draft{Title: "t", Content: "政府は新たな経済対策をまとめた。",
		Figures: []reading.FigureDraft{{Data: jpegBytes(t), InText: true}}}, SubmitOptions{})
	if f, err := h.svc.Figure(ctx, "me", res.Article.ID, 0); err != nil || len(f.Data) == 0 {
		t.Fatalf("own figure = %v", err)
	}
	if _, err := h.svc.Figure(ctx, "them", res.Article.ID, 0); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("foreign figure err = %v", err)
	}
	d, _ := h.svc.Edition(ctx, "me", res.Edition.ID)
	if len(d.Figures) != 1 || d.Figures[0].Data != nil {
		t.Fatalf("detail figures = %+v", d.Figures)
	}
}
```

Run: `make test 2>&1 | grep -A5 'application/reading'` — Expected: build failure (`res.Figures`, `h.cover`, `Figure` undefined).

- [ ] **Step 3: Implement** in `service.go`:

Add `Cover publishing.CoverDesigner // nil: no cover` to `Deps`; add to `SubmitResult`:

```go
	// Figures is how many figures the article has after this submit;
	// FiguresRejected how many of the submitted ones failed validation.
	Figures, FiguresRejected int
```

In `Submit`, right after `res := SubmitResult{Article: stored}`:

```go
	res.Figures, res.FiguresRejected = s.attachFigures(ctx, identity, stored, d.Figures)
```

and add:

```go
// attachFigures validates and stores an article's figures. Images are
// optional: every failure here is logged and swallowed.
func (s *Service) attachFigures(ctx context.Context, identity learner.IdentityID, a reading.Article, drafts []reading.FigureDraft) (have, rejected int) {
	log := slog.With("identity", identity, "article", a.ID)
	if len(drafts) > 0 {
		figs, bad := reading.NewFigures(drafts, a.Paragraphs)
		for _, err := range bad {
			log.Warn("reading: figure rejected", "err", err)
		}
		rejected = len(bad)
		if len(figs) > 0 {
			if _, err := s.d.Repo.AttachFigures(ctx, identity, a.ID, figs); err != nil {
				log.Warn("reading: attach figures", "err", err)
			}
		}
	}
	list, err := s.d.Repo.ListFigures(ctx, identity, a.ID)
	if err != nil {
		log.Warn("reading: list figures", "err", err)
	}
	return len(list), rejected
}

// Figure returns one of an article's figures with its bytes.
func (s *Service) Figure(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error) {
	return s.d.Repo.FigureData(ctx, identity, articleID, ordinal)
}
```

In `EditionDetail` add `Figures []reading.Figure`; in `Edition`, after loading deliveries:

```go
	figs, err := s.d.Repo.ListFigures(ctx, identity, a.ID)
	if err != nil {
		slog.Warn("reading: list figures", "identity", identity, "article", a.ID, "err", err)
	}
	out.Figures = figs
```

(assign after `out` is built). Replace `render`:

```go
func (s *Service) render(ctx context.Context, a reading.Article, e reading.StudyEdition) (Ebook, error) {
	if e.Status != reading.EditionReady || e.Lesson == nil {
		return Ebook{}, ErrNotReady
	}
	figs := s.figuresWithData(ctx, a)
	book := publishing.Ebook{ID: e.ID, Article: a, Lesson: *e.Lesson, GeneratedAt: e.UpdatedAt, Figures: figs}
	book.Cover = s.cover(ctx, a, figs)
	data, err := s.d.Renderer.Render(ctx, book)
	if err != nil {
		return Ebook{}, fmt.Errorf("reading: render: %w", err)
	}
	return Ebook{Filename: Filename(a, e) + s.d.Renderer.Extension(), MediaType: s.d.Renderer.MediaType(), Data: data}, nil
}

// figuresWithData loads an article's figures with their bytes; one that
// cannot be loaded is left out of the book.
func (s *Service) figuresWithData(ctx context.Context, a reading.Article) []reading.Figure {
	list, err := s.d.Repo.ListFigures(ctx, a.IdentityID, a.ID)
	if err != nil {
		slog.Warn("reading: list figures for render", "article", a.ID, "err", err)
		return nil
	}
	out := make([]reading.Figure, 0, len(list))
	for _, f := range list {
		full, err := s.d.Repo.FigureData(ctx, a.IdentityID, a.ID, f.Ordinal)
		if err != nil {
			slog.Warn("reading: load figure for render", "article", a.ID, "ordinal", f.Ordinal, "err", err)
			continue
		}
		out = append(out, full)
	}
	return out
}

// cover draws the book's cover from the lead figure, falling back to the
// no-photo design and then to no cover: a cover never fails a book.
func (s *Service) cover(ctx context.Context, a reading.Article, figs []reading.Figure) []byte {
	if s.d.Cover == nil {
		return nil
	}
	in := publishing.CoverInput{Title: a.Title, Source: a.SourceName}
	if a.PublishedAt != nil {
		in.Date = a.PublishedAt.Format("2006年1月2日")
	}
	for _, f := range figs {
		if f.Lead {
			in.Photo = f.Data
			break
		}
	}
	if in.Photo == nil && len(figs) > 0 {
		in.Photo = figs[0].Data
	}
	img, err := s.d.Cover.Design(ctx, in)
	if err != nil && in.Photo != nil {
		slog.Warn("reading: cover with photo", "article", a.ID, "err", err)
		in.Photo = nil
		img, err = s.d.Cover.Design(ctx, in)
	}
	if err != nil {
		slog.Warn("reading: cover", "article", a.ID, "err", err)
		return nil
	}
	return img
}
```

- [ ] **Step 4: Verify** — Run: `make test 2>&1 | grep -E 'application/reading|FAIL'` — Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/ports/publishing internal/application/reading
git commit -m "feat(reading): figures and a cover reach the renderer"
```

---

### Task 4: Cover adapter

**Files:**
- Create: `internal/adapters/cover/cover.go`, `internal/adapters/cover/wrap.go`, `internal/adapters/cover/cover_test.go`, `internal/adapters/cover/fonts/{MPLUS1p-Bold.ttf,MPLUS1p-Regular.ttf,OFL.txt}`
- Modify: `Makefile` (`vendor-cover-fonts`), `go.mod`/`go.sum` (`golang.org/x/image`)

**Interfaces:**
- Consumes: `publishing.CoverInput`, `publishing.CoverDesigner` (Task 3)
- Produces: `func cover.New() (*cover.Designer, error)`; `(*Designer).Design(ctx, publishing.CoverInput) ([]byte, error)`; `func wrapLines(face font.Face, s string, width fixed.Int26_6, maxLines int) []string`

- [ ] **Step 1: Vendor the fonts** — add to `Makefile` (and to `.PHONY`):

```make
COVER_FONT_REV := d714b17ce2379f06daf6295617f961df605dccb5
vendor-cover-fonts: ## Vendor pinned M PLUS 1p (OFL) into internal/adapters/cover/fonts for the 読解 cover
	$(TOOLS) sh -c "mkdir -p internal/adapters/cover/fonts && for f in MPLUS1p-Bold.ttf MPLUS1p-Regular.ttf OFL.txt; do \
	  curl -fsSL https://cdn.jsdelivr.net/gh/google/fonts@$(COVER_FONT_REV)/ofl/mplus1p/\$$f -o internal/adapters/cover/fonts/\$$f || exit 1; done"
```

Run: `make vendor-cover-fonts && ls -la internal/adapters/cover/fonts` — Expected: two ~1.7 MB TTFs and `OFL.txt`.
Run: `make tidy` after adding the import in Step 3 (`golang.org/x/image` v0.46.0 or later).

- [ ] **Step 2: Failing tests** — `internal/adapters/cover/cover_test.go`:

```go
package cover

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/math/fixed"

	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

func solid(t *testing.T, c color.Color, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	return img
}

func near(c color.Color, r, g, b uint8) bool {
	cr, cg, cb, _ := c.RGBA()
	d := func(x uint32, y uint8) bool { v := int(x>>8) - int(y); return v > -24 && v < 24 }
	return d(cr, r) && d(cg, g) && d(cb, b)
}

func TestDesignPhotoCover(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Design(context.Background(), publishing.CoverInput{
		Title: "電気料金が「過去最高」の水準へ", Source: "NHKニュース", Date: "2026年9月29日",
		Photo: solid(t, color.RGBA{0, 0, 255, 255}, 800, 300), // wide: must be cropped to fill
	})
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, out)
	if img.Bounds().Dx() != Width || img.Bounds().Dy() != Height {
		t.Fatalf("size = %v", img.Bounds())
	}
	for _, p := range []image.Point{{10, 10}, {Width - 10, 10}, {Width / 2, PhotoHeight - 10}} {
		if !near(img.At(p.X, p.Y), 0, 0, 255) {
			t.Fatalf("photo area at %v = %v, want the photo filling it", p, img.At(p.X, p.Y))
		}
	}
	if !near(img.At(20, Height-20), 0xfa, 0xf7, 0xf2) {
		t.Fatalf("text panel background = %v", img.At(20, Height-20))
	}
}

func TestDesignWithoutPhotoUsesAccentBand(t *testing.T) {
	d, _ := New()
	out, err := d.Design(context.Background(), publishing.CoverInput{Title: "見出し"})
	if err != nil {
		t.Fatal(err)
	}
	if img := decode(t, out); !near(img.At(Width/2, PhotoHeight/2), 0xb7, 0x28, 0x2e) {
		t.Fatalf("band = %v", img.At(Width/2, PhotoHeight/2))
	}
}

func TestDesignRejectsCorruptPhoto(t *testing.T) {
	d, _ := New()
	if _, err := d.Design(context.Background(), publishing.CoverInput{Title: "t", Photo: []byte("nope")}); err == nil {
		t.Fatal("a corrupt photo must be an error so the caller can fall back")
	}
}

func TestDesignIsDeterministic(t *testing.T) {
	d, _ := New()
	in := publishing.CoverInput{Title: "同じ表紙", Source: "S", Date: "D"}
	a, _ := d.Design(context.Background(), in)
	b, _ := d.Design(context.Background(), in)
	if !bytes.Equal(a, b) {
		t.Fatal("same input must give the same bytes (the EPUB is deterministic)")
	}
}

func TestWrapLinesEllipsisAndKinsoku(t *testing.T) {
	d, _ := New()
	face := d.titleFace
	width := fixed.I(600)
	long := strings.Repeat("日本語の見出しがとても長い場合、", 20)
	lines := wrapLines(face, long, width, 4)
	if len(lines) != 4 || !strings.HasSuffix(lines[3], "…") {
		t.Fatalf("lines = %q", lines)
	}
	for _, l := range lines {
		if textWidth(face, l) > width {
			t.Fatalf("line overflows: %q", l)
		}
		if strings.ContainsRune("、。」』）！？ー", []rune(l)[0]) {
			t.Fatalf("line starts with a no-break-before character: %q", l)
		}
	}
	if got := wrapLines(face, "短い", width, 4); len(got) != 1 || got[0] != "短い" {
		t.Fatalf("short = %q", got)
	}
}
```

Run: `make test 2>&1 | grep -A3 adapters/cover` — Expected: build failure.

- [ ] **Step 3: Implement** — `internal/adapters/cover/wrap.go`:

```go
package cover

import (
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// noBreakBefore are characters a Japanese line must not start with
// (kinsoku shori, the common subset).
const noBreakBefore = "、。，．」』）】〕！？ー…・：；"

func textWidth(face font.Face, s string) fixed.Int26_6 { return font.MeasureString(face, s) }

// wrapLines breaks s into lines no wider than width, at any character
// (Japanese has no spaces to wait for) but never starting a line with a
// no-break-before character, and ellipsises the last of maxLines.
func wrapLines(face font.Face, s string, width fixed.Int26_6, maxLines int) []string {
	var lines []string
	var cur []rune
	for _, r := range []rune(strings.TrimSpace(s)) {
		next := append(append([]rune{}, cur...), r)
		if len(cur) > 0 && textWidth(face, string(next)) > width {
			if strings.ContainsRune(noBreakBefore, r) {
				// Pull the last character down with this one.
				last := cur[len(cur)-1]
				lines = append(lines, string(cur[:len(cur)-1]))
				cur = []rune{last, r}
			} else {
				lines = append(lines, string(cur))
				cur = []rune{r}
			}
			continue
		}
		cur = next
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	if len(lines) <= maxLines {
		return lines
	}
	lines = lines[:maxLines]
	last := []rune(lines[maxLines-1])
	for len(last) > 0 && textWidth(face, string(last)+"…") > width {
		last = last[:len(last)-1]
	}
	lines[maxLines-1] = string(last) + "…"
	return lines
}
```

`internal/adapters/cover/cover.go`:

```go
// Package cover draws a 読解 edition's cover: the article's lead photo
// over a typeset title, as one JPEG, so the title shows even in a
// Kindle library thumbnail. Fonts are embedded (M PLUS 1p, OFL — see
// fonts/OFL.txt); nothing is fetched.
package cover

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

//go:embed fonts/MPLUS1p-Bold.ttf fonts/MPLUS1p-Regular.ttf
var fonts embed.FS

// Geometry: Amazon's recommended 1:1.6, photo on the top 60%.
const (
	Width       = 1600
	Height      = 2560
	PhotoHeight = 1536
	margin      = 120
)

var (
	accent = color.RGBA{0xb7, 0x28, 0x2e, 0xff} // the app's accent
	paper  = color.RGBA{0xfa, 0xf7, 0xf2, 0xff}
	ink    = color.RGBA{0x1f, 0x1b, 0x16, 0xff}
	muted  = color.RGBA{0x6b, 0x63, 0x5a, 0xff}
)

// Designer implements publishing.CoverDesigner.
type Designer struct {
	titleFace, kickerFace, metaFace font.Face
}

var _ publishing.CoverDesigner = (*Designer)(nil)

// New parses the embedded fonts once.
func New() (*Designer, error) {
	load := func(name string, size float64) (font.Face, error) {
		b, err := fonts.ReadFile("fonts/" + name)
		if err != nil {
			return nil, err
		}
		f, err := opentype.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("cover: parse %s: %w", name, err)
		}
		return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	var d Designer
	var err error
	if d.titleFace, err = load("MPLUS1p-Bold.ttf", 108); err != nil {
		return nil, err
	}
	if d.kickerFace, err = load("MPLUS1p-Bold.ttf", 52); err != nil {
		return nil, err
	}
	if d.metaFace, err = load("MPLUS1p-Regular.ttf", 50); err != nil {
		return nil, err
	}
	return &d, nil
}

// Design draws the cover. A photo that cannot be decoded is an error, so
// the caller can retry without it.
func (d *Designer) Design(_ context.Context, in publishing.CoverInput) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	xdraw.Draw(img, img.Bounds(), image.NewUniform(paper), image.Point{}, xdraw.Src)
	top := image.Rect(0, 0, Width, PhotoHeight)
	if in.Photo != nil {
		photo, _, err := image.Decode(bytes.NewReader(in.Photo))
		if err != nil {
			return nil, fmt.Errorf("cover: decode photo: %w", err)
		}
		xdraw.CatmullRom.Scale(img, top, photo, cropToFill(photo.Bounds(), top.Dx(), top.Dy()), xdraw.Src, nil)
	} else {
		xdraw.Draw(img, top, image.NewUniform(accent), image.Point{}, xdraw.Src)
	}

	text := func(face font.Face, c color.Color, s string, y int) {
		(&font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face, Dot: fixed.P(margin, y)}).DrawString(s)
	}
	y := PhotoHeight + 150
	text(d.kickerFace, accent, "日本語読解", y)
	y += 70
	lh := int(float64(d.titleFace.Metrics().Height.Ceil()) * 1.3)
	for _, line := range wrapLines(d.titleFace, in.Title, fixed.I(Width-2*margin), 4) {
		y += lh
		text(d.titleFace, ink, line, y)
	}
	meta := in.Source
	if in.Date != "" {
		if meta != "" {
			meta += "・"
		}
		meta += in.Date
	}
	if meta != "" {
		text(d.metaFace, muted, meta, Height-150)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// cropToFill returns the centred part of src with the target's aspect.
func cropToFill(src image.Rectangle, w, h int) image.Rectangle {
	sw, sh := src.Dx(), src.Dy()
	if sw*h > sh*w { // too wide
		cw := sh * w / h
		x := src.Min.X + (sw-cw)/2
		return image.Rect(x, src.Min.Y, x+cw, src.Max.Y)
	}
	ch := sw * h / w
	y := src.Min.Y + (sh-ch)/2
	return image.Rect(src.Min.X, y, src.Max.X, y+ch)
}
```

- [ ] **Step 4: Verify** — Run: `make tidy && make test 2>&1 | grep -E 'adapters/cover|FAIL'` — Expected: `ok`. Then look at one: add a temporary `t.Logf` or write `out` to `tmp/cover-sample.jpg` in a scratch test run and open it with the Read tool; check the title wraps inside the margins and nothing overlaps. Remove any scratch output.

- [ ] **Step 5: Commit**

```bash
git add Makefile go.mod go.sum internal/adapters/cover
git commit -m "feat(reading): a designed cover from the lead photo, M PLUS 1p embedded"
```

---

### Task 5: EPUB — figures and cover

**Files:**
- Modify: `internal/adapters/epub/epub.go`, `templates/article.xhtml.tmpl`, `templates/content.opf.tmpl`, `style.css`, `epub_test.go`, `Makefile` (`epubcheck-sample`)

**Interfaces:**
- Consumes: `publishing.Ebook.Figures/Cover`, `reading.Layout`, `reading.Block`
- Produces: EPUB entries `OEBPS/images/fig-<ordinal>.<jpg|png|gif>`, `OEBPS/images/cover.jpg`

- [ ] **Step 1: Failing tests** — append to `epub_test.go` (reuse its existing helpers for opening the zip; if none, add):

```go
func unzipBook(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = data
	}
	return out
}

func bookWithFigures(t *testing.T) publishing.Ebook {
	t.Helper()
	var jb, pb bytes.Buffer
	_ = jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil)
	_ = png.Encode(&pb, image.NewRGBA(image.Rect(0, 0, 300, 300)))
	b := sampleBook() // the existing test fixture; three paragraphs
	b.Figures = []reading.Figure{
		{Ordinal: 0, AfterParagraph: -1, InText: true, Lead: true, Caption: "冒頭", Alt: "会見の様子", MediaType: "image/jpeg", Data: jb.Bytes()},
		{Ordinal: 1, AfterParagraph: 1, InText: true, Caption: "グラフ", MediaType: "image/png", Data: pb.Bytes()},
		{Ordinal: 2, AfterParagraph: 0, InText: false, MediaType: "image/jpeg", Data: jb.Bytes()},
	}
	b.Cover = jb.Bytes()
	return b
}

func TestRenderPlacesFiguresAndDeclaresCover(t *testing.T) {
	out, err := New().Render(context.Background(), bookWithFigures(t))
	if err != nil {
		t.Fatal(err)
	}
	files := unzipBook(t, out)
	for _, name := range []string{"OEBPS/images/fig-0.jpg", "OEBPS/images/fig-1.png", "OEBPS/images/cover.jpg"} {
		if len(files[name]) == 0 {
			t.Fatalf("missing %s", name)
		}
	}
	if _, ok := files["OEBPS/images/fig-2.jpg"]; ok {
		t.Fatal("a cover-only figure must not be in the book's images")
	}
	art := string(files["OEBPS/article.xhtml"])
	i0, i1 := strings.Index(art, `src="images/fig-0.jpg"`), strings.Index(art, `src="images/fig-1.png"`)
	p2 := strings.Index(art, sampleBook().Article.Paragraphs[1][:9])
	if i0 < 0 || i1 < 0 || !(i0 < p2 && p2 < i1) {
		t.Fatalf("figure order wrong: fig0=%d para2=%d fig1=%d", i0, p2, i1)
	}
	if !strings.Contains(art, `alt="会見の様子"`) || !strings.Contains(art, "<figcaption>") {
		t.Fatal("alt/caption missing")
	}
	opf := string(files["OEBPS/content.opf"])
	for _, want := range []string{
		`<meta name="cover" content="cover-image"/>`,
		`id="cover-image" href="images/cover.jpg" media-type="image/jpeg" properties="cover-image"`,
		`href="images/fig-1.png" media-type="image/png"`,
	} {
		if !strings.Contains(opf, want) {
			t.Fatalf("content.opf missing %s", want)
		}
	}
	if strings.Contains(opf[strings.Index(opf, "<spine"):], "cover") {
		t.Fatal("the cover must not be in the spine (Kindle would show it twice)")
	}
}

func TestRenderWithFiguresIsDeterministic(t *testing.T) {
	a, _ := New().Render(context.Background(), bookWithFigures(t))
	b, _ := New().Render(context.Background(), bookWithFigures(t))
	if !bytes.Equal(a, b) {
		t.Fatal("not deterministic")
	}
	if p := os.Getenv("EPUB_SAMPLE_OUT"); p != "" {
		if err := os.WriteFile(p, a, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
```

(If the existing fixture has a different name than `sampleBook`, use it; it must have ≥ 3 paragraphs.)

Run: `make test 2>&1 | grep -A5 adapters/epub` — Expected: FAIL.

- [ ] **Step 2: Implement** in `epub.go`:

Replace `Article [][]reading.Segment` in `bookData` with `Blocks []blockView`, and add `Images []imageItem; HasCover bool`:

```go
type figureView struct {
	Href, Alt string
	Caption   []reading.Segment
}

type blockView struct {
	Paragraph []reading.Segment
	Figure    *figureView
}

type imageItem struct{ ID, Href, MediaType string }

func imageExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	}
	return ".jpg"
}
```

In `Render`, build them (before `d := bookData{…}`), and set `Blocks: blocks, Images: images, HasCover: len(b.Cover) > 0` in `d`:

```go
	var blocks []blockView
	var images []imageItem
	for _, bl := range reading.Layout(b.Article.Paragraphs, b.Figures, l.Vocabulary) {
		if bl.Figure == nil {
			blocks = append(blocks, blockView{Paragraph: bl.Paragraph})
			continue
		}
		href := fmt.Sprintf("images/fig-%d%s", bl.Figure.Ordinal, imageExt(bl.Figure.MediaType))
		alt := bl.Figure.Alt
		if alt == "" {
			alt = bl.Figure.Caption
		}
		blocks = append(blocks, blockView{Figure: &figureView{Href: href, Alt: alt, Caption: bl.Caption}})
		images = append(images, imageItem{ID: fmt.Sprintf("fig-%d", bl.Figure.Ordinal), Href: href, MediaType: bl.Figure.MediaType})
	}
```

After the chapters loop and before `zw.Close()`, write the bytes (stored, not deflated — they are already compressed):

```go
	byOrdinal := map[int]reading.Figure{}
	for _, f := range b.Figures {
		byOrdinal[f.Ordinal] = f
	}
	for _, im := range images {
		var ord int
		fmt.Sscanf(im.ID, "fig-%d", &ord)
		if err := add(zw, "OEBPS/"+im.Href, byOrdinal[ord].Data, zip.Store); err != nil {
			return nil, err
		}
	}
	if len(b.Cover) > 0 {
		if err := add(zw, "OEBPS/images/cover.jpg", b.Cover, zip.Store); err != nil {
			return nil, err
		}
	}
```

`templates/article.xhtml.tmpl` — replace the `{{range .Article}}…{{end}}` line with:

```
{{range .Blocks}}{{if .Figure}}<figure class="figure"><img src="{{.Figure.Href}}" alt="{{.Figure.Alt}}"/>{{if .Figure.Caption}}<figcaption>{{range .Figure.Caption}}{{if .Vocab}}<span class="vocab">{{template "ruby" .}}</span>{{else}}{{.Text}}{{end}}{{end}}</figcaption>{{end}}</figure>
{{else}}<p class="body">{{range .Paragraph}}{{if .Vocab}}<span class="vocab">{{template "ruby" .}}</span>{{else}}{{.Text}}{{end}}{{end}}</p>
{{end}}{{end}}
```

`templates/content.opf.tmpl` — inside `<metadata>` after `dcterms:modified`:

```
    {{if .HasCover}}<meta name="cover" content="cover-image"/>
    {{end}}
```

and in `<manifest>` after the css item:

```
    {{if .HasCover}}<item id="cover-image" href="images/cover.jpg" media-type="image/jpeg" properties="cover-image"/>
    {{end}}{{range .Images}}<item id="{{.ID}}" href="{{.Href}}" media-type="{{.MediaType}}"/>
    {{end}}
```

`style.css` — append:

```css
figure.figure { margin: 1.2em 0; text-align: center; page-break-inside: avoid; break-inside: avoid; }
figure.figure img { max-width: 100%; height: auto; }
figure.figure figcaption { font-size: 0.8em; line-height: 1.5; margin-top: 0.4em; color: #555; text-align: center; }
```

Any other template or code that read `.Article` (grep `\.Article` in `templates/`) must switch to `.Blocks`.

- [ ] **Step 3: EPUBCheck target** — add to `Makefile` (and `.PHONY`):

```make
EPUBCHECK_VERSION := 5.2.1
epubcheck-sample: ## Render a sample edition with figures + cover and run EPUBCheck on it (needs docker)
	$(TOOLS) sh -c "EPUB_SAMPLE_OUT=/src/tmp/sample.epub go test ./internal/adapters/epub -run TestRenderWithFiguresIsDeterministic -count=1"
	@test -f tmp/epubcheck-$(EPUBCHECK_VERSION)/epubcheck.jar || (mkdir -p tmp && curl -fsSL https://github.com/w3c/epubcheck/releases/download/v$(EPUBCHECK_VERSION)/epubcheck-$(EPUBCHECK_VERSION).zip -o tmp/epubcheck.zip && cd tmp && unzip -qo epubcheck.zip)
	docker run --rm -v "$(CURDIR)/tmp:/w" -w /w eclipse-temurin:21-jre java -jar epubcheck-$(EPUBCHECK_VERSION)/epubcheck.jar sample.epub
```

(Check the tools container's mount path with `grep -n 'TOOLS\s*:=' Makefile` and adjust `/src` to it.)

- [ ] **Step 4: Verify**

Run: `make test 2>&1 | grep -E 'adapters/epub|FAIL'` — Expected: `ok`.
Run: `make epubcheck-sample 2>&1 | tail -5` — Expected: `No errors or warnings detected.`

- [ ] **Step 5: Commit**

```bash
git add internal/adapters/epub Makefile
git commit -m "feat(reading): figures in the EPUB, and the cover declared for Kindle"
```

---

### Task 6: HTTP — multipart submit, figure route, page

**Files:**
- Modify: `internal/adapters/http/reading.go`, `internal/adapters/http/server.go` (route), `internal/adapters/http/reading_test.go`, `web/templates/reading_detail.html.tmpl`, `web/static/css/app.css`, `docs/api/reading.md`

**Interfaces:**
- Consumes: `Service.Submit` with `Draft.Figures`, `SubmitResult.Figures/FiguresRejected`, `EditionDetail.Figures`, `Service.Figure`, `reading.Layout`
- Produces: `POST /api/v1/reading/articles` multipart; response fields `figures`, `figures_rejected`; edition DTO `figure_count`; `GET /reading/articles/{id}/figures/{n}`

- [ ] **Step 1: Failing tests** — append to `internal/adapters/http/reading_test.go` (use the file's existing server/test-token helpers — find how existing API tests post with a `reading:write` token and reuse that helper; below it is called `apiDo(t, srv, req)` and the page client `pageGet(t, srv, path, identity)`; rename to match):

```go
func multipartSubmit(t *testing.T, meta string, images ...[]byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("metadata", meta)
	for i, img := range images {
		w, _ := mw.CreateFormFile(fmt.Sprintf("image-%d", i), fmt.Sprintf("f%d", i))
		_, _ = w.Write(img)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reading/articles", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func smallJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	_ = jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil)
	return b.Bytes()
}

func TestAPIReadingSubmitMultipartAttachesFigures(t *testing.T) {
	srv := newReadingTestServer(t)
	meta := `{"title":"経済対策","content":"政府は新たな経済対策をまとめた。\n\n物価高への対応が柱。",
	  "figures":[{"caption":"会見","after_paragraph":0,"after_text":"政府は","lead":true},{"caption":"壊れた"}]}`
	res := apiDo(t, srv, multipartSubmit(t, meta, smallJPEG(t), []byte("broken")))
	if res.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", res.Code, res.Body)
	}
	var out struct {
		Figures         int `json:"figures"`
		FiguresRejected int `json:"figures_rejected"`
		Edition         struct {
			FigureCount int    `json:"figure_count"`
			ArticleID   string `json:"article_id"`
		} `json:"edition"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &out)
	if out.Figures != 1 || out.FiguresRejected != 1 || out.Edition.FigureCount != 1 {
		t.Fatalf("response = %s", res.Body)
	}
}

func TestAPIReadingSubmitJSONStillWorks(t *testing.T) {
	srv := newReadingTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reading/articles", strings.NewReader(`{"title":"t","content":"政府は新たな経済対策をまとめた。"}`))
	req.Header.Set("Content-Type", "application/json")
	if res := apiDo(t, srv, req); res.Code != http.StatusAccepted {
		t.Fatalf("status %d", res.Code)
	}
}

func TestAPIReadingSubmitMultipartTooLargeIs400(t *testing.T) {
	srv := newReadingTestServer(t)
	res := apiDo(t, srv, multipartSubmit(t, `{"title":"t","content":"本文です。"}`, make([]byte, 16<<20)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.Code)
	}
}

func TestReadingFigureRoute(t *testing.T) {
	srv := newReadingTestServer(t)
	meta := `{"title":"t","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0}]}`
	res := apiDo(t, srv, multipartSubmit(t, meta, smallJPEG(t)))
	var out struct {
		Edition struct {
			ArticleID string `json:"article_id"`
		} `json:"edition"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &out)
	path := "/reading/articles/" + out.Edition.ArticleID + "/figures/0"

	got := pageGet(t, srv, path, "dev") // the identity apiDo's token belongs to
	if got.Code != 200 || got.Header().Get("Content-Type") != "image/jpeg" ||
		!strings.Contains(got.Header().Get("Cache-Control"), "immutable") || got.Header().Get("ETag") == "" {
		t.Fatalf("own figure: %d %v", got.Code, got.Header())
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("If-None-Match", got.Header().Get("ETag"))
	if r := pageDo(t, srv, req, "dev"); r.Code != http.StatusNotModified {
		t.Fatalf("revalidation = %d", r.Code)
	}
	if r := pageGet(t, srv, path, "someone-else"); r.Code != http.StatusNotFound {
		t.Fatalf("foreign figure = %d, want 404", r.Code)
	}
	if r := pageGet(t, srv, "/reading/articles/"+out.Edition.ArticleID+"/figures/x", "dev"); r.Code != http.StatusNotFound {
		t.Fatalf("bad ordinal = %d", r.Code)
	}
}

func TestReadingDetailShowsFigures(t *testing.T) {
	srv := newReadingTestServer(t)
	meta := `{"title":"t","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0,"caption":"会見の様子"}]}`
	res := apiDo(t, srv, multipartSubmit(t, meta, smallJPEG(t)))
	var out struct {
		Edition struct {
			ID        string `json:"id"`
			ArticleID string `json:"article_id"`
		} `json:"edition"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &out)
	page := pageGet(t, srv, "/reading/"+out.Edition.ID, "dev").Body.String()
	if !strings.Contains(page, `/figures/0"`) || !strings.Contains(page, "会見の様子") {
		t.Fatalf("figure not on the page")
	}
}
```

Run: `make test 2>&1 | grep -A5 adapters/http` — Expected: FAIL.

- [ ] **Step 2: Implement** in `reading.go`:

DTO additions:

```go
type figureMetaDTO struct {
	Caption        string `json:"caption"`
	Alt            string `json:"alt"`
	AfterParagraph int    `json:"after_paragraph"`
	AfterText      string `json:"after_text"`
	Lead           bool   `json:"lead"`
	// InText defaults to true; the extension sends false for a page's
	// og:image used only as the cover.
	InText *bool `json:"in_text"`
}
```

Add `Figures []figureMetaDTO \`json:"figures"\`` to `submitArticleRequestDTO`; add `Figures int \`json:"figures"\`` and `FiguresRejected int \`json:"figures_rejected"\`` to `submitArticleResponseDTO`; add `FigureCount int \`json:"figure_count"\`` to `readingEditionDTO`, and change `toReadingEditionDTO(e, dls)` to `toReadingEditionDTO(e, dls, figureCount int)` setting it (update both callers: submit passes `res.Figures`, `apiReadingEdition` passes `len(d.Figures)`).

Multipart decoding — new constant and function:

```go
// maxMultipartSubmitBytes bounds POST /api/v1/reading/articles when it
// carries images: 12 figures at the client's 1200 px downscale are well
// under this; the JSON form keeps maxRequestBodyBytes.
const maxMultipartSubmitBytes = 15 << 20

// decodeSubmit reads the submit request in either form. Images are
// matched to metadata.figures by index (image-0 is figures[0]); a
// figure whose image part is missing is dropped.
func decodeSubmit(w http.ResponseWriter, r *http.Request) (submitArticleRequestDTO, []reading.FigureDraft, error) {
	var req submitArticleRequestDTO
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		return req, nil, decodeJSON(w, r, &req)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartSubmitBytes)
	if err := r.ParseMultipartForm(maxMultipartSubmitBytes); err != nil {
		return req, nil, err
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	if err := json.Unmarshal([]byte(r.FormValue("metadata")), &req); err != nil {
		return req, nil, err
	}
	var figs []reading.FigureDraft
	for i, m := range req.Figures {
		fhs := r.MultipartForm.File[fmt.Sprintf("image-%d", i)]
		if len(fhs) == 0 {
			continue
		}
		f, err := fhs[0].Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, reading.MaxFigureBytes+1))
		_ = f.Close()
		if err != nil {
			continue
		}
		inText := m.InText == nil || *m.InText
		figs = append(figs, reading.FigureDraft{Caption: m.Caption, Alt: m.Alt, AfterParagraph: m.AfterParagraph,
			AfterText: m.AfterText, Lead: m.Lead, InText: inText, Data: data})
	}
	return req, figs, nil
}
```

In `apiReadingSubmit`, replace the `decodeJSON` block with:

```go
	req, figs, err := decodeSubmit(w, r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
```

pass `Figures: figs` in the `reading.Draft`, and set `Figures: res.Figures, FiguresRejected: res.FiguresRejected` on the response. When `req.Selection` is used, drop in-text figures (positions refer to the whole article): before building the draft,

```go
	if strings.TrimSpace(req.Selection) != "" {
		content = req.Selection
		kept := figs[:0]
		for _, f := range figs {
			if f.Lead { // the cover still makes sense
				f.InText = false
				kept = append(kept, f)
			}
		}
		figs = kept
	}
```

(merge with the existing selection `if`.)

Figure route handler:

```go
// readingFigure handles GET /reading/articles/{id}/figures/{n}: one of
// the learner's article images. The bytes never change for a given
// (article, ordinal), so they are cached for a year and revalidated by
// their sha256.
func (s *Server) readingFigure(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n < 0 {
		http.NotFound(w, r)
		return
	}
	f, err := s.opts.Reading.Figure(r.Context(), ident.ID, chi.URLParam(r, "id"), n)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load image", http.StatusInternalServerError)
		return
	}
	etag := `"` + f.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(f.Data)
}
```

Route in `server.go`, next to the other `/reading/articles/{id}/…` routes: `r.Get("/reading/articles/{id}/figures/{n}", s.readingFigure)`.

Detail page: in `readingDetail`, replace both `data["Body"] = reading.Annotate(…)` lines with one after the lesson block:

```go
	var vocabForBody []reading.VocabularyItem
	if d.Edition.Lesson != nil {
		vocabForBody = d.Edition.Lesson.Vocabulary
	}
	data["Blocks"] = reading.Layout(d.Article.Paragraphs, d.Figures, vocabForBody)
```

Template `reading_detail.html.tmpl` line 61 — replace `{{range .Body}}…{{end}}` with:

```
      {{range .Blocks}}{{if .Figure}}<figure class="reading-figure"><img src="/reading/articles/{{$.Article.ID}}/figures/{{.Figure.Ordinal}}" alt="{{if .Figure.Alt}}{{.Figure.Alt}}{{else}}{{.Figure.Caption}}{{end}}" width="{{.Figure.Width}}" height="{{.Figure.Height}}" loading="lazy">{{if .Caption}}<figcaption lang="ja">{{range .Caption}}{{template "ruby_segment" .}}{{end}}</figcaption>{{end}}</figure>{{else}}<p>{{range .Paragraph}}{{template "ruby_segment" .}}{{end}}</p>{{end}}{{end}}
```

`web/static/css/app.css` — append (use existing tokens; check the file for the muted-text variable name):

```css
.reading-figure { margin: 1.25rem 0; }
.reading-figure img { display: block; max-width: 100%; height: auto; margin: 0 auto; border-radius: var(--radius-sm, 4px); }
.reading-figure figcaption { font-size: .85rem; color: var(--muted); text-align: center; margin-top: .4rem; }
```

`docs/api/reading.md` — add a "Submitting with images" section documenting: `multipart/form-data` with a `metadata` part (the JSON body plus `figures[]` of `caption`, `alt`, `after_paragraph`, `after_text`, `lead`, `in_text`) and `image-N` parts matched by index; 15 MiB limit (JSON stays 1 MiB); JPEG/PNG/GIF ≤ 2 MB, ≤ 25 MP, ≤ 12; a bad image is dropped and counted in `figures_rejected`; response `figures`; edition `figure_count`; `GET /reading/articles/{id}/figures/{n}` (page route, cookie auth).

- [ ] **Step 3: Verify** — Run: `make test 2>&1 | grep -E 'adapters/http|FAIL'` — Expected: `ok`.

- [ ] **Step 4: Commit**

```bash
git add internal/adapters/http web docs/api/reading.md
git commit -m "feat(reading): images over the API, on the page, and at their own URL"
```

---

### Task 7: Wiring

**Files:** Modify `cmd/jlp/main.go`

- [ ] **Step 1: Implement** — before `appreading.NewService(…)`:

```go
		coverDesigner, err := coveradapter.New()
		if err != nil {
			// The fonts are embedded, so this is a build defect, not a
			// runtime condition — but a missing cover must never stop
			// the app: books go out without one.
			slog.Error("reading: cover designer unavailable", "err", err)
		}
```

add `Cover: coverDesignerOrNil(coverDesigner),` to `appreading.Deps` with:

```go
// coverDesignerOrNil keeps a nil *cover.Designer from becoming a non-nil
// interface holding nil.
func coverDesignerOrNil(d *coveradapter.Designer) publishing.CoverDesigner {
	if d == nil {
		return nil
	}
	return d
}
```

and import `coveradapter "github.com/mikeyaustin/jlp/internal/adapters/cover"` (and `publishing` if not already imported). Keep `err` shadowing consistent with the surrounding block.

- [ ] **Step 2: Verify** — Run: `make test && make restart && sleep 5 && make logs s=app 2>&1 | tail -5` — Expected: tests `ok`; app logs `listening`, no cover error.

- [ ] **Step 3: Commit**

```bash
git add cmd/jlp/main.go
git commit -m "feat(reading): wire the cover designer"
```

---

### Task 8: Extension — capture, fetch, downscale, upload

**Files:** Modify `chrome-extension/article.js`, `popup.js`, `popup.html`, `options.html`, `options.js`, `shim/chrome-shim.js`, `manifest.json` (version bump to 1.2.0), `README.md`

**Interfaces:**
- Consumes: multipart API (Task 6)
- Produces: `article.js` result gains `figures: [{src, caption, alt, after_paragraph, after_text, lead, in_text}]`; `popup.js` exports `prepareImages`, `submitArticle` (multipart when images)

- [ ] **Step 1: `article.js` — capture figures.** In `paragraphs(root)`, collect figures in the same document-order walk. Change the selector to `"h2, h3, p, blockquote, li, img"` and handle `img` before the existing checks:

```js
    const blocks = [];
    const figs = [];
    for (const el of root.querySelectorAll("h2, h3, p, blockquote, li, img")) {
      if (el.tagName === "IMG") {
        const f = figureOf(el);
        if (f) figs.push({ ...f, blockIndex: blocks.length - 1 });
        continue;
      }
      // ... existing furniture / li / blockquote handling, pushing {t, heading}
    }
```

After the heading filter, map each figure's `blockIndex` to the kept-paragraph index and the start of that paragraph's text:

```js
    const keptIdx = []; // block index -> paragraph index in `out`, or -1
    let n = -1;
    blocks.forEach((b, i) => {
      const kept = !b.heading || (blocks[i + 1] && !blocks[i + 1].heading);
      if (kept) n++;
      keptIdx[i] = n;
    });
    const figures = figs.slice(0, 12).map((f, i) => {
      const after = f.blockIndex < 0 ? -1 : keptIdx[f.blockIndex];
      return { src: f.src, caption: f.caption, alt: f.alt, after_paragraph: after,
               after_text: after >= 0 ? out[after].slice(0, 40) : "", lead: i === 0, in_text: true };
    });
    return { paragraphs: out.length ? out : [text(root)], figures };
```

Update the caller: `const body = paragraphs(root);` then `content: body.paragraphs.join("\n\n")`, and add `figures` to the returned object with the og:image fallback:

```js
  let figures = body.figures;
  if (!figures.length) {
    const og = meta("meta[property='og:image']");
    if (og) figures = [{ src: new URL(og, location.href).href, caption: "", alt: "", after_paragraph: -1, after_text: "", lead: true, in_text: false }];
  }
```

and `figureOf`:

```js
  // figureOf reads one <img> as a figure, or null for page furniture:
  // anything inside SKIP, links (a thumbnail to another story), SVG, and
  // images under 200 px either way (pixels, icons, logos, avatars).
  function figureOf(img) {
    if (img.closest(SKIP) || img.closest("a")) return null;
    const w = img.naturalWidth || parseInt(img.getAttribute("width"), 10) || img.getBoundingClientRect().width;
    const h = img.naturalHeight || parseInt(img.getAttribute("height"), 10) || img.getBoundingClientRect().height;
    if ((w && w < 200) || (h && h < 200)) return null;
    const src = bestSrc(img);
    if (!src || /\.svg(\?|$)/i.test(src) || src.startsWith("data:image/svg")) return null;
    const fig = img.closest("figure");
    const cap = fig && fig.querySelector("figcaption");
    return { src, alt: (img.getAttribute("alt") || "").trim(), caption: cap ? text(cap) : "" };
  }

  // bestSrc picks the largest candidate: the widest srcset entry, then
  // what the browser chose, then src, then the lazy-loading attributes
  // sites keep the real URL in until the image scrolls into view.
  function bestSrc(img) {
    const set = img.getAttribute("srcset") || img.getAttribute("data-srcset") || "";
    let best = "", bestW = 0;
    for (const part of set.split(",")) {
      const [u, d] = part.trim().split(/\s+/);
      const w = parseInt(d, 10) || 1;
      if (u && w >= bestW) { best = u; bestW = w; }
    }
    const raw = best || img.currentSrc || img.getAttribute("data-src") || img.getAttribute("data-original") ||
      img.getAttribute("data-lazy-src") || img.getAttribute("src") || "";
    try { return raw ? new URL(raw, location.href).href : ""; } catch (_) { return ""; }
  }
```

Note: `figure`/`figcaption` stay in `SKIP` for *text* (captions are not article prose) — `figureOf` must therefore not use `SKIP` against the `<figure>` wrapper itself. Define `const SKIP_FOR_IMAGES = SKIP.replace("figure, figcaption, ", "")` and use it in `figureOf`.

- [ ] **Step 2: `popup.js` — fetch, downscale, upload.** Add:

```js
const IMAGE_ORIGINS = ["https://*/*", "http://*/*"];
const MAX_EDGE = 1200;
const MAX_IMAGE_BYTES = 2 * 1024 * 1024;
const PNG_KEEP_BYTES = 500 * 1024;

// imagesAllowed: image capture needs host access to every image CDN,
// granted once from the options page. Without it the import is text-only.
async function imagesAllowed() {
  if (!chrome.permissions || !chrome.permissions.contains) return false;
  return chrome.permissions.contains({ origins: IMAGE_ORIGINS });
}

async function fetchWithTimeout(url, ms) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), ms);
  try {
    const res = await fetch(url, { credentials: "include", signal: ctl.signal });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return await res.blob();
  } finally {
    clearTimeout(timer);
  }
}

// downscale re-encodes one image: long side ≤ 1200 px; PNG kept when it
// stays small (charts), else JPEG stepping down in quality until ≤ 2 MB.
async function downscale(blob) {
  const bmp = await createImageBitmap(blob);
  const scale = Math.min(1, MAX_EDGE / Math.max(bmp.width, bmp.height));
  const canvas = new OffscreenCanvas(Math.round(bmp.width * scale), Math.round(bmp.height * scale));
  const ctx = canvas.getContext("2d");
  ctx.fillStyle = "#fff"; // JPEG has no alpha
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  ctx.drawImage(bmp, 0, 0, canvas.width, canvas.height);
  bmp.close();
  if (blob.type === "image/png") {
    const png = await canvas.convertToBlob({ type: "image/png" });
    if (png.size < PNG_KEEP_BYTES) return png;
  }
  for (const quality of [0.82, 0.72, 0.62]) {
    const jpg = await canvas.convertToBlob({ type: "image/jpeg", quality });
    if (jpg.size <= MAX_IMAGE_BYTES) return jpg;
  }
  throw new Error("too large");
}

// prepareImages fetches and downscales the captured figures. A figure
// that fails is dropped, never the import.
async function prepareImages(figures) {
  const ok = [];
  let failed = 0;
  for (const f of figures || []) {
    try {
      ok.push({ meta: f, blob: await downscale(await fetchWithTimeout(f.src, 10000)) });
    } catch (err) {
      failed++;
      console.warn("[JLP] image skipped", f.src, String(err));
    }
  }
  return { ok, failed };
}
```

In `submitArticle(article)`, replace the single JSON `jlpFetch` with:

```js
    const fields = {
      url: article.url || "", title: article.title || "", source: article.source || "",
      author: article.author || "", published_at: article.published_at || "",
      content: article.content || "", selection: article.selection || "", deliver: !!cfg.autoDeliver,
    };
    let images = { ok: [], failed: 0 };
    let imageNote = "";
    if ((article.figures || []).length) {
      if (await imagesAllowed()) {
        statusEl.textContent = "画像を準備中…";
        images = await prepareImages(article.figures);
      } else {
        imageNote = "画像は取り込まれません（オプションで許可してください）";
      }
    }
    const postJSON = () => jlpFetch(cfg, `/api/v1/reading/articles`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(fields),
    });
    let res;
    if (images.ok.length) {
      const form = new FormData();
      form.append("metadata", JSON.stringify({ ...fields, figures: images.ok.map((x) => x.meta) }));
      images.ok.forEach((x, i) => form.append(`image-${i}`, x.blob, `image-${i}`));
      res = await jlpFetch(cfg, `/api/v1/reading/articles`, { method: "POST", body: form });
      if (res.status === 400 || res.status === 413) {
        imageNote = "画像を送れなかったため、本文のみで作成しました";
        res = await postJSON();
      }
    } else {
      res = await postJSON();
    }
```

(keep the existing `!res.ok` handling after it). After `const out = await res.json();`, record the image line for `renderEdition` to append:

```js
    const total = (article.figures || []).length;
    readingImageNote = imageNote || (total ? `画像 ${out.figures ?? 0}/${total}枚` : "");
```

with a module-level `let readingImageNote = "";`, and in `renderEdition`, before `statusEl.textContent = line;`:

```js
  if (readingImageNote) line += `　${readingImageNote}`;
```

Set `readingImageNote = ""` at the start of `submitArticle`.

- [ ] **Step 3: Options — the permission.** `options.html`: add under the reading settings

```html
<label><input type="checkbox" id="images-allowed"> 記事の画像も取り込む（すべてのサイトへのアクセスを許可）</label>
<p class="hint">画像はこのブラウザが記事のページと同じように読み込み、縮小してからJLPに送ります。JLPのサーバーが記事のサイトにアクセスすることはありません。</p>
```

`options.js`:

```js
async function syncImagesCheckbox() {
  const box = document.getElementById("images-allowed");
  box.checked = await chrome.permissions.contains({ origins: ["https://*/*", "http://*/*"] });
}
document.getElementById("images-allowed").addEventListener("change", async (e) => {
  const origins = ["https://*/*", "http://*/*"];
  // Requested from the click itself: permissions.request needs the gesture.
  if (e.target.checked) e.target.checked = await chrome.permissions.request({ origins });
  else await chrome.permissions.remove({ origins });
});
syncImagesCheckbox();
```

- [ ] **Step 4: Shim** — in `shim/chrome-shim.js`, add a permissions stub the verification protocol can toggle:

```js
  let granted = true;
  window.chrome.permissions = {
    contains: async () => granted,
    request: async () => (granted = true),
    remove: async () => { granted = false; return true; },
  };
```

- [ ] **Step 5: README + manifest** — `manifest.json` `"version": "1.2.0"`. README: under 「JLPでKindle版を作成」 document image capture (what counts as a figure, the 12/1200 px/2 MB limits, the one-time permission in options, text-only fallback and the 「画像 n/m枚」 line), and in the verification protocol how to drive `prepareImages` from an app-origin tab.

- [ ] **Step 6: Browser verification** (per `chrome-extension/README.md`; the stack running with `make up-mail` and Kindle pointed at Mailpit as in the jlp-dev-environment notes):
  1. Synthetic page served on a local static server with: an article `<p>` sequence; a `<figure>` with `<img srcset="a.jpg 600w, b.jpg 1400w">` + `<figcaption>`; a lazy `<img data-src>`; a 1×1 tracking pixel; an `<aside>` image; a linked thumbnail; an `<img>` on a host without CORS. Run `article.js` there; assert `figures` holds exactly the figure and the lazy image, with `b.jpg`, captions and `after_text`.
  2. Live NHK article: run `article.js`; assert the captured figures are the article's own photos (compare with a screenshot), none from 「あわせて読みたい」.
  3. In an app-origin tab (localhost:28080) with the shim: seed the token, call `submitArticle` with the NHK capture. Assert the popup shows 「画像 n/n枚」, `/reading/{id}` shows the images in place, the EPUB download contains `images/fig-*.jpg` and `images/cover.jpg`, and Mailpit receives it. Open the cover JPEG with the Read tool and look at it.
  4. With `chrome.permissions.remove` via the shim: the import succeeds text-only with the options hint.
  5. Zero console errors throughout.

- [ ] **Step 7: Commit**

```bash
git add chrome-extension
git commit -m "feat(extension): capture, shrink and upload an article's images"
```

---

### Task 9: Ship

- [ ] **Step 1: Full suite** — `make test && make test-race && make test-integration && make lint && make epubcheck-sample`. Expected: all green; lint shows only the 3 known `assets.go` findings.
- [ ] **Step 2: Push** — `git push origin main`.
- [ ] **Step 3: Image** — `make image-push IMAGE_TAG=20260929-reading-figures`.
- [ ] **Step 4: Deploy** — in `../platform-v2/home/ansible/jlp-playbook.yaml` set `jlp_image: registry.nas.jackiemclean.net/jlp:20260929-reading-figures`; run `ansible-playbook -i inventory.yaml jlp-playbook.yaml --vault-password-file ./vault_pass.sh` from that directory. Expected: `failed=0`; the playbook applies migration 00031.
- [ ] **Step 5: Commit platform-v2** — `git -C ../platform-v2 commit -am "jlp: deploy 20260929-reading-figures (article images and cover)" && git -C ../platform-v2 push`.
- [ ] **Step 6: Real delivery** — ask the user to reload the extension (1.2.0), tick 「記事の画像も取り込む」 in options, and import an article with photos. Read the production logs (`ansible venus.lan -i inventory.yaml --vault-password-file ./vault_pass.sh -b --become-method su -m shell -a 'docker logs --since 15m jlp-app-1 2>&1 | grep -iE "reading|figure|cover"'`) and confirm `reading: delivered` with no figure/cover warnings. The user judges the result on the Kindle; that feeds the typography pass.
