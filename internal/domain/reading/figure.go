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
