package reading

import (
	"errors"
	"time"
	"unicode/utf8"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// A draft is an article still being collected: the learner captures it
// page by page, each page becomes ordered blocks, and blocks can be
// excluded before the whole thing is sent as one article.

type BlockKind string

const (
	BlockParagraph BlockKind = "paragraph"
	BlockImage     BlockKind = "image"
)

// DraftBlock is one paragraph or image of a draft. Seq orders blocks
// across pages (the repository assigns it); Page numbers the captured
// pages. PageURL is the page's URL, which lets a re-capture replace it.
type DraftBlock struct {
	Seq, Page int
	PageURL   string
	Kind      BlockKind
	Text      string // paragraph
	Figure    Figure // image (Data nil when listed)
	Excluded  bool
}

// DraftMeta is the article metadata taken from the first page captured.
type DraftMeta struct {
	Title, SourceName, SourceURL, Author string
	PublishedAt                          *time.Time
}

type DraftInfo struct {
	ID                   string
	Identity             learner.IdentityID
	Meta                 DraftMeta
	CreatedAt, UpdatedAt time.Time
}

// MaxDraftPages bounds distinct pages in one draft; with the per-page
// figure cap it keeps a draft's stored bytes bounded.
const MaxDraftPages = 10

var ErrDraftFull = errors.New("reading: a draft holds at most 10 pages")

// afterTextRunes is how much of the preceding paragraph anchors a figure
// on send — the same prefix NewFigures matches against.
const afterTextRunes = 40

// PageBlocks turns one captured page into ordered blocks: paragraphs from
// NormaliseParagraphs(text), each followed by the figures NewFigures
// anchored after it (figures anchored -1 first). Seq/Page are left 0 for
// the repository to assign. rejected is NewFigures' per-figure errors.
func PageBlocks(pageURL, text string, figs []FigureDraft) (blocks []DraftBlock, rejected []error) {
	paras := NormaliseParagraphs(text)
	kept, rejected := NewFigures(figs, paras)
	imgBlock := func(f Figure) DraftBlock {
		return DraftBlock{PageURL: pageURL, Kind: BlockImage, Figure: f}
	}
	for _, f := range kept {
		if f.AfterParagraph < 0 {
			blocks = append(blocks, imgBlock(f))
		}
	}
	for i, p := range paras {
		blocks = append(blocks, DraftBlock{PageURL: pageURL, Kind: BlockParagraph, Text: p})
		for _, f := range kept {
			if f.AfterParagraph == i {
				blocks = append(blocks, imgBlock(f))
			}
		}
	}
	return blocks, rejected
}

// DraftSummary counts what a draft holds and what a send would keep.
type DraftSummary struct {
	Pages, Paragraphs, KeptParagraphs, Images, KeptImages, Chars int
}

// Summarise counts blocks; Chars is the runes of the kept paragraphs.
func Summarise(blocks []DraftBlock) DraftSummary {
	var s DraftSummary
	pages := map[int]bool{}
	for _, b := range blocks {
		pages[b.Page] = true
		switch b.Kind {
		case BlockParagraph:
			s.Paragraphs++
			if !b.Excluded {
				s.KeptParagraphs++
				s.Chars += utf8.RuneCountInString(b.Text)
			}
		case BlockImage:
			s.Images++
			if !b.Excluded {
				s.KeptImages++
			}
		}
	}
	s.Pages = len(pages)
	return s
}

// Submission builds the article Draft from the kept blocks, in order:
// paragraphs joined by blank lines; each kept image anchored to the
// kept paragraph before it (-1 when none precedes), the first the lead.
func Submission(meta DraftMeta, blocks []DraftBlock) Draft {
	d := Draft{
		SourceURL: meta.SourceURL, SourceName: meta.SourceName, Title: meta.Title,
		Author: meta.Author, PublishedAt: meta.PublishedAt,
	}
	paras := 0
	prev := ""
	for _, b := range blocks {
		if b.Excluded {
			continue
		}
		switch b.Kind {
		case BlockParagraph:
			if paras > 0 {
				d.Content += "\n\n"
			}
			d.Content += b.Text
			prev = b.Text
			paras++
		case BlockImage:
			f := FigureDraft{
				Caption: b.Figure.Caption, Alt: b.Figure.Alt, InText: true,
				Lead: len(d.Figures) == 0, Data: b.Figure.Data, AfterParagraph: paras - 1,
			}
			if prev != "" {
				f.AfterText = string([]rune(prev)[:min(afterTextRunes, utf8.RuneCountInString(prev))])
			}
			d.Figures = append(d.Figures, f)
		}
	}
	return d
}
