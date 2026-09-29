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
