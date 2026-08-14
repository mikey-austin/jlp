package writing

import (
	"time"
	"unicode/utf8"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

type DocumentID string

// Document is a learner's single piece of writing for a session: one
// document per session, versioned on every autosave.
type Document struct {
	ID         DocumentID
	SessionID  session.ID
	IdentityID learner.IdentityID
	Content    string
	Version    int
	UpdatedAt  time.Time
}

// RuneCount reports the number of Japanese characters (runes) in the
// document, not its byte length — multi-byte UTF-8 content like kanji and
// kana would otherwise be over-counted.
func (d Document) RuneCount() int {
	return utf8.RuneCountInString(d.Content)
}
