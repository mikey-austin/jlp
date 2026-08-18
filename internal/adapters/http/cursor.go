package httpx

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// The ?cursor= parameter on /vocabulary.
//
// It encodes the (last_event, id) position the next page starts after.
// base64url of "<RFC3339Nano>|<uuid>" — not because it is a secret (it
// is not; it names a row the learner already saw) but because an opaque
// blob does not invite hand-editing, and the encoding can change without
// breaking a bookmark that only ever round-trips it.
//
// Decoding is deliberately forgiving: anything that does not parse
// yields the zero cursor, which is the first page. A malformed cursor is
// a URL someone truncated or a stale bookmark, and answering it with the
// first page is both harmless and obvious, where a 400 would be a dead
// end on a page the learner did not know they were mistreating.
func encodeCursor(c storage.VocabularyCursor) string {
	if c.Zero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(c.LastEvent.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

func decodeCursor(raw string) storage.VocabularyCursor {
	if raw == "" {
		return storage.VocabularyCursor{}
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return storage.VocabularyCursor{}
	}
	ts, id, found := strings.Cut(string(decoded), "|")
	if !found || id == "" {
		return storage.VocabularyCursor{}
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return storage.VocabularyCursor{}
	}
	// The id is not validated here beyond being non-empty: the
	// repository parses it as a UUID and falls back to the first page if
	// it is not one, so there is exactly one place that decides what a
	// usable cursor is.
	return storage.VocabularyCursor{LastEvent: at, ID: id}
}
