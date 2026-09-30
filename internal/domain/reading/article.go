// Package reading is the 読解 bounded context: a Japanese article the
// learner chose to read (captured by the Chrome extension or pasted on
// /reading), and the AI-written study editions built around it — a
// vocabulary list, grammar notes, a 精読 of the hardest sentences and
// review questions — which are rendered as a reflowable EPUB and
// optionally sent to a Kindle.
//
// The article and its study material are deliberately separate
// aggregates. An Article is what the learner read; a StudyEdition is
// one analysis of it, pinned to the prompt/schema version that produced
// it. Regenerating the lesson with a better prompt makes a new edition
// of the same article rather than re-ingesting it, and a failed Kindle
// delivery never costs another model call.
//
// Pure domain: no ports, no adapters, no I/O.
package reading

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// DefaultMaxArticleRunes bounds one article's body. A long-form WSJ
// feature is ~6–8k characters of Japanese; 20k leaves generous room
// while keeping one analysis call's input — and its cost — bounded.
// Configurable via APP_READING_MAXARTICLERUNES.
const DefaultMaxArticleRunes = 20000

// maxTitleRunes/maxSourceRunes/maxURLBytes keep free-text metadata from
// the extension sane. A page title that long is navigation chrome, not
// a headline.
const (
	maxTitleRunes  = 300
	maxSourceRunes = 100
	maxAuthorRunes = 200
	maxURLBytes    = 2048
)

// Validation errors. The HTTP layer maps every one of these to a 400
// with err.Error() as the message, so each says what to fix.
var (
	ErrEmptyContent    = errors.New("reading: article content is empty")
	ErrArticleTooLarge = errors.New("reading: article content is too long")
	ErrInvalidURL      = errors.New("reading: source url must be an absolute http(s) url")
	// ErrTranslationMismatch is a translation that cannot stand in for its
	// original: the paragraph counts differ, or a paragraph came back empty.
	ErrTranslationMismatch = errors.New("reading: translation does not match the original paragraphs")
)

// Article is one piece of text the learner chose to study. It holds
// Japanese, or the original foreign text until the worker translates it.
// Paragraphs is the body after normalisation (see NormaliseParagraphs);
// ContentHash is a SHA-256 over those paragraphs, and is what makes
// ingestion idempotent: the same identity submitting the same text twice
// (a double right-click, a retried request) gets the existing article
// back instead of paying for a second analysis.
type Article struct {
	ID          string
	IdentityID  learner.IdentityID
	SourceURL   string
	SourceName  string
	Title       string
	Author      string
	PublishedAt *time.Time
	Paragraphs  []string
	ContentHash string
	CreatedAt   time.Time

	// OriginalLanguage is "ja" for an article written in Japanese, "und"
	// for one in another language that is not yet translated, and, once
	// translated, the model's name for the source language (英語). Title
	// and Paragraphs then hold the Japanese and Original* the source.
	OriginalLanguage   string
	OriginalTitle      string
	OriginalParagraphs []string
}

// Translation is a Japanese rendering of an article, as the translate
// agent returns it.
type Translation struct {
	SourceLanguage string
	Title          string
	Paragraphs     []string
}

// LanguageJapanese and LanguageUndetermined are the two OriginalLanguage
// values that are not a language name.
const (
	LanguageJapanese     = "ja"
	LanguageUndetermined = "und"
)

// NeedsTranslation reports whether the article is in another language and
// has not been translated yet. A translated article keeps its original,
// so regenerating a lesson never translates twice.
func (a Article) NeedsTranslation() bool {
	return a.OriginalLanguage != LanguageJapanese && len(a.OriginalParagraphs) == 0
}

// WithTranslation moves the article's current text into Original* and
// makes the Japanese the body to study. ContentHash stays the hash of the
// source text, so submitting the same source again is still a duplicate.
func (a Article) WithTranslation(t Translation) (Article, error) {
	if len(t.Paragraphs) != len(a.Paragraphs) {
		return Article{}, fmt.Errorf("%w: %d paragraphs, want %d", ErrTranslationMismatch, len(t.Paragraphs), len(a.Paragraphs))
	}
	for i, p := range t.Paragraphs {
		if strings.TrimSpace(p) == "" {
			return Article{}, fmt.Errorf("%w: paragraph %d is empty", ErrTranslationMismatch, i+1)
		}
	}
	a.OriginalTitle, a.OriginalParagraphs = a.Title, a.Paragraphs
	a.Title, a.Paragraphs = ClipTitle(t.Title), append([]string(nil), t.Paragraphs...)
	if a.Title == "" {
		a.Title = a.OriginalTitle
	}
	a.OriginalLanguage = t.SourceLanguage
	return a, nil
}

// Draft is an article as submitted, before validation.
type Draft struct {
	SourceURL   string
	SourceName  string
	Title       string
	Author      string
	PublishedAt *time.Time
	Content     string
	// Figures are the article's images. They never reach NewArticle's
	// text or ContentHash: an article is the same article with or
	// without its pictures.
	Figures []FigureDraft
}

// NewArticle validates d and builds the Article it describes. Content is
// normalised to paragraphs first, so a submission that is all whitespace
// or navigation fragments fails as empty rather than being sent to a
// model.
func NewArticle(id string, identity learner.IdentityID, d Draft, maxRunes int, now time.Time) (Article, error) {
	if maxRunes <= 0 {
		maxRunes = DefaultMaxArticleRunes
	}
	paras := NormaliseParagraphs(d.Content)
	if len(paras) == 0 {
		return Article{}, ErrEmptyContent
	}
	total := 0
	for _, p := range paras {
		total += utf8.RuneCountInString(p)
	}
	if total > maxRunes {
		return Article{}, fmt.Errorf("%w (%d characters; the limit is %d — select the part you want to study)", ErrArticleTooLarge, total, maxRunes)
	}
	lang := LanguageJapanese
	if !looksJapanese(paras) {
		lang = LanguageUndetermined
	}
	src := strings.TrimSpace(d.SourceURL)
	if src != "" {
		if len(src) > maxURLBytes {
			return Article{}, ErrInvalidURL
		}
		u, err := url.Parse(src)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Article{}, ErrInvalidURL
		}
		// The fragment never changes what was read, and keeping it would
		// make the same article from two in-page anchors look different.
		u.Fragment = ""
		src = u.String()
	}
	title := ClipTitle(d.Title)
	if title == "" {
		title = clip(paras[0], 60)
	}
	a := Article{
		ID:          id,
		IdentityID:  identity,
		SourceURL:   src,
		SourceName:  clip(oneLine(d.SourceName), maxSourceRunes),
		Title:       title,
		Author:      clip(oneLine(d.Author), maxAuthorRunes),
		PublishedAt: d.PublishedAt,
		Paragraphs:  paras,
		ContentHash: ContentHash(paras),
		CreatedAt:   now.UTC(),

		OriginalLanguage: lang,
	}
	return a, nil
}

// Text is the article body with paragraphs separated by blank lines —
// what the analysis prompt receives.
func (a Article) Text() string { return strings.Join(a.Paragraphs, "\n\n") }

// Runes is the body's length in characters.
func (a Article) Runes() int {
	n := 0
	for _, p := range a.Paragraphs {
		n += utf8.RuneCountInString(p)
	}
	return n
}

// ContentHash identifies a body by its normalised paragraphs. Metadata
// (title, URL) is deliberately NOT part of it: the same text reached
// through a share link and through the canonical URL is the same
// article.
func ContentHash(paragraphs []string) string {
	h := sha256.New()
	for _, p := range paragraphs {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NormaliseParagraphs splits raw text into paragraphs: CRLF folded, each
// line trimmed (including the ideographic space U+3000 newspapers indent
// with), blank lines as separators, runs of internal whitespace inside a
// line collapsed. Lines inside one paragraph are joined without a space —
// Japanese wraps without one, and a browser's innerText of a <p> with a
// <br> in it would otherwise grow a stray gap mid-sentence.
func NormaliseParagraphs(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for _, line := range strings.Split(raw, "\n") {
		line = collapseSpace(strings.TrimFunc(line, unicode.IsSpace))
		if line == "" {
			flush()
			continue
		}
		if cur.Len() > 0 && needsSpace(cur.String(), line) {
			cur.WriteByte(' ')
		}
		cur.WriteString(line)
	}
	flush()
	return out
}

// needsSpace reports whether joining prev and next needs a space — only
// when both sides of the join are Latin letters or digits (an English
// phrase wrapped across lines). Japanese never gets one.
func needsSpace(prev, next string) bool {
	last, _ := utf8.DecodeLastRuneInString(prev)
	first, _ := utf8.DecodeRuneInString(next)
	isLatin := func(r rune) bool { return r < 0x3000 && (unicode.IsLetter(r) || unicode.IsDigit(r)) }
	return isLatin(last) && isLatin(first)
}

func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// looksJapanese requires at least a fifth of the letters to be kana or
// kanji. It decides whether an article is studied as it is or translated
// first, and is not a language detector: mixed articles full of English
// company names pass easily.
func looksJapanese(paras []string) bool {
	ja, letters := 0, 0
	for _, p := range paras {
		for _, r := range p {
			switch {
			case unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han):
				ja++
				letters++
			case unicode.IsLetter(r):
				letters++
			}
		}
	}
	return letters > 0 && ja*5 >= letters
}

// ClipTitle is the title as NewArticle stores it: one line, at most
// maxTitleRunes. Drafts apply it on the way in so a stored title is
// never larger than what a send would keep.
func ClipTitle(s string) string { return clip(oneLine(s), maxTitleRunes) }

func oneLine(s string) string { return collapseSpace(strings.TrimSpace(s)) }

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
