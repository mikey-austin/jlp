// Package epub renders a 読解 study edition as a reflowable EPUB 3 —
// the format Send to Kindle converts natively — built for reading
// Japanese on an e-ink screen:
//
//   - reflowable XHTML, never fixed layout, so the reader's own font
//     size, line spacing, margins and Japanese font settings all work;
//   - the article first, with furigana (<ruby>) on the first occurrence
//     of each 重要語彙 word, then 重要語彙 / 表現・文法 / 精読 / 復習,
//     then the review answers on their own page so reading a question
//     does not show you its answer;
//   - an EPUB 3 nav document AND an NCX, because older Kindle firmware
//     still builds its "Go To" menu from the NCX;
//   - lang="ja" throughout, so the device picks a Japanese font and
//     Japanese line breaking, with English glosses marked lang="en".
//
// Output is deterministic: the same edition renders to the same bytes
// (fixed zip timestamps, stable ordering), so a re-sent book is the
// same book.
package epub

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

//go:embed templates/*.tmpl style.css
var files embed.FS

var tmpl = template.Must(template.New("epub").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).ParseFS(files, "templates/*.tmpl"))

// zipTime is the fallback generation time, and dosDate its MS-DOS form
// stamped on every zip entry: fixed, so identical input gives identical
// bytes.
var zipTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const dosDate = uint16((2026-1980)<<9 | 1<<5 | 1)

// Renderer implements publishing.Renderer.
type Renderer struct{}

// New returns an EPUB renderer.
func New() Renderer { return Renderer{} }

var _ publishing.Renderer = Renderer{}

func (Renderer) MediaType() string { return "application/epub+zip" }
func (Renderer) Extension() string { return ".epub" }

// chapter is one XHTML document in the book, in reading order.
type chapter struct {
	ID, File, Title string
}

// bookData is what every template sees.
type bookData struct {
	UID        string
	Title      string
	Source     string
	SourceURL  string
	Author     string
	Published  string
	Generated  string
	Modified   string
	Lesson     reading.Lesson
	Article    [][]reading.Segment
	Chapters   []chapter
	Chapter    chapter
	HasAnswers bool
}

// Render builds the EPUB.
func (Renderer) Render(_ context.Context, b publishing.Ebook) ([]byte, error) {
	l := b.Lesson
	chapters := []chapter{{ID: "article", File: "article.xhtml", Title: "記事"}}
	chapters = append(chapters, chapter{ID: "vocabulary", File: "vocabulary.xhtml", Title: "重要語彙"})
	if len(l.Grammar) > 0 {
		chapters = append(chapters, chapter{ID: "grammar", File: "grammar.xhtml", Title: "表現・文法"})
	}
	if len(l.SentenceAnalyses) > 0 {
		chapters = append(chapters, chapter{ID: "close-reading", File: "close-reading.xhtml", Title: "精読"})
	}
	hasReview := len(l.Review.Comprehension)+len(l.Review.Vocabulary) > 0
	if hasReview {
		chapters = append(chapters,
			chapter{ID: "review", File: "review.xhtml", Title: "復習"},
			chapter{ID: "answers", File: "answers.xhtml", Title: "解答"})
	}

	gen := b.GeneratedAt.UTC()
	if gen.IsZero() {
		gen = zipTime
	}
	d := bookData{
		UID:        "urn:uuid:" + b.ID,
		Title:      b.Article.Title,
		Source:     b.Article.SourceName,
		SourceURL:  b.Article.SourceURL,
		Author:     b.Article.Author,
		Generated:  gen.Format("2006年1月2日"),
		Modified:   gen.Format("2006-01-02T15:04:05Z"),
		Lesson:     l,
		Article:    reading.Annotate(b.Article.Paragraphs, l.Vocabulary),
		Chapters:   chapters,
		HasAnswers: hasReview,
	}
	if b.Article.PublishedAt != nil {
		d.Published = b.Article.PublishedAt.Format("2006年1月2日")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// The mimetype entry must be first and stored uncompressed (OCF
	// §4.3) — readers sniff it at a fixed offset.
	if err := add(zw, "mimetype", []byte("application/epub+zip"), zip.Store); err != nil {
		return nil, err
	}
	if err := add(zw, "META-INF/container.xml", []byte(containerXML), zip.Deflate); err != nil {
		return nil, err
	}
	css, err := files.ReadFile("style.css")
	if err != nil {
		return nil, err
	}
	if err := add(zw, "OEBPS/style.css", css, zip.Deflate); err != nil {
		return nil, err
	}
	for _, f := range []struct{ name, tmpl string }{
		{"OEBPS/content.opf", "content.opf.tmpl"},
		{"OEBPS/nav.xhtml", "nav.xhtml.tmpl"},
		{"OEBPS/toc.ncx", "toc.ncx.tmpl"},
	} {
		if err := addTemplate(zw, f.name, f.tmpl, d); err != nil {
			return nil, err
		}
	}
	for _, c := range chapters {
		cd := d
		cd.Chapter = c
		if err := addTemplate(zw, "OEBPS/"+c.File, c.ID+".xhtml.tmpl", cd); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func add(zw *zip.Writer, name string, data []byte, method uint16) error {
	// The MS-DOS date fields, not FileHeader.Modified: Modified makes Go
	// write an extended-timestamp "extra field", which OCF forbids on the
	// mimetype entry (EPUBCheck PKG-005) and which buys nothing elsewhere.
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, ModifiedDate: dosDate, ModifiedTime: 0}) //nolint:staticcheck // see above
	if err != nil {
		return fmt.Errorf("epub: %s: %w", name, err)
	}
	_, err = w.Write(data)
	return err
}

func addTemplate(zw *zip.Writer, name, tmplName string, d bookData) error {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	if err := tmpl.ExecuteTemplate(&b, tmplName, d); err != nil {
		return fmt.Errorf("epub: render %s: %w", name, err)
	}
	return add(zw, name, []byte(b.String()), zip.Deflate)
}

const containerXML = `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`
