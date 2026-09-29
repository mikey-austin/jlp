// Package publishing defines the ports the 読解 pipeline publishes
// through: turning a study edition into an ebook file, and sending that
// file somewhere a reader will open it (a Kindle's Send-to-Kindle
// address today).
//
// Two ports, not one, because they fail and change independently: the
// EPUB is a pure function of stored data and never touches the network,
// while delivery is a network call with its own retries. Keeping them
// apart is what lets a failed email be retried without regenerating
// anything, and lets the same renderer serve a browser download.
package publishing

import (
	"context"
	"errors"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/reading"
)

// Ebook is everything a renderer needs for one book.
type Ebook struct {
	// ID is a stable identifier for the book (the edition id): EPUB
	// readers use it to tell a re-sent book from a different one.
	ID      string
	Article reading.Article
	Lesson  reading.Lesson
	// GeneratedAt is when the edition was analysed; it goes in the
	// book's metadata and colophon.
	GeneratedAt time.Time
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

// Renderer turns one edition into a complete ebook file.
type Renderer interface {
	// Render returns the file's bytes.
	Render(ctx context.Context, book Ebook) ([]byte, error)
	// MediaType is the file's MIME type, e.g. application/epub+zip.
	MediaType() string
	// Extension is the file extension including the dot, e.g. ".epub".
	Extension() string
}

// Parcel is one file to deliver.
type Parcel struct {
	To        string
	Subject   string
	Filename  string
	MediaType string
	Data      []byte
}

// ErrPermanent wraps a delivery failure that retrying cannot fix (a
// rejected recipient, a file over the service's size limit, bad
// credentials). The pipeline stops retrying and marks the delivery
// failed straight away rather than burning its remaining attempts.
var ErrPermanent = errors.New("publishing: permanent delivery failure")

// Deliverer sends a parcel to its recipient.
type Deliverer interface {
	Deliver(ctx context.Context, p Parcel) error
}
