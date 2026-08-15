package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

// VocabularyRepository persists the learner's personal vocabulary:
// vocabulary_items (one row per (identity, expression), UNIQUE — see
// the 00011 migration) and vocabulary_events (an append-only log of
// every lookup/production, whose partial UNIQUE(identity_id,
// client_event_id) WHERE client_event_id IS NOT NULL is what makes
// UpsertOnLookup idempotent under retry).
type VocabularyRepository interface {
	// UpsertOnLookup records a vocabulary.lookup: creating the item on
	// its first sighting or, on a repeat lookup of the same
	// (identity, expression), incrementing Lookups and refreshing
	// LastEvent/Reading/Meaning/Source in place. clientEventID, when
	// non-empty, is checked against vocabulary_events first — a repeat
	// call with the SAME clientEventID (a client retrying a POST whose
	// response it never saw) is a no-op: the existing item is returned
	// unchanged, with the bool return true, and Lookups is NOT
	// incremented again. An empty clientEventID skips that dedup check
	// entirely (every call counts).
	UpsertOnLookup(ctx context.Context, identity learner.IdentityID, expression, reading, meaning, source string, kind vocabulary.Kind, clientEventID string, at time.Time) (item vocabulary.Item, duplicate bool, err error)
	// RecordProduction increments itemID's Productions (and, when
	// successful, SuccessfulProductions) and refreshes LastEvent.
	RecordProduction(ctx context.Context, identity learner.IdentityID, itemID string, successful bool, at time.Time) error
	// List returns identity's vocabulary items, most-recently-active
	// first, narrowed by filter: "" (all), "looked-up" (Lookups > 0 —
	// i.e. every item, since every item is created by a lookup),
	// "produced" (Productions > 0), or "activate" — Task 7's expression
	// bank activation-candidate filter, which always answers empty
	// until that task wires it up.
	List(ctx context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error)
	// AllExpressions returns every one of identity's vocabulary
	// expressions mapped to its item ID — the candidate set
	// application/vocabulary.Service.DetectProduction scans a reviewed
	// text against.
	AllExpressions(ctx context.Context, identity learner.IdentityID) (map[string]string, error)
}
