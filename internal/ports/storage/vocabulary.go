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
	// entirely (every call counts). example is the ingest event's
	// example sentence — it has no column on Item, but is persisted
	// into the appended vocabulary_events row's payload alongside
	// reading/meaning/source, for audit.
	UpsertOnLookup(ctx context.Context, identity learner.IdentityID, expression, reading, meaning, source, example string, kind vocabulary.Kind, clientEventID string, at time.Time) (item vocabulary.Item, duplicate bool, err error)
	// RecordProduction increments itemID's Productions (and, when
	// successful, SuccessfulProductions) and refreshes LastEvent.
	RecordProduction(ctx context.Context, identity learner.IdentityID, itemID string, successful bool, at time.Time) error
	// List returns identity's vocabulary items, most-recently-active
	// first, narrowed by filter: "" (all), "looked-up" (Lookups > 0 —
	// i.e. every item, since every item is created by a lookup),
	// "produced" (Productions > 0), or "activate" — PRD §55/§17.5's
	// vocabulary activator: items looked up often but never produced
	// (Lookups >= 3 AND Productions = 0), OR any bank expression/pattern
	// never produced (Kind IN (expression, pattern) AND Productions =
	// 0) — the latter clause is what makes a freshly-seeded bank item
	// (Lookups=0) a candidate from the moment it's seeded, not only
	// after the learner has looked it up three times themselves.
	// This filter's raw membership; used by the /vocabulary page's
	// 活性化候補 tab, unlimited. application/planner.Planner.
	// ActivationCandidates — backed by ListActivationCandidates below,
	// NOT this method — is the intended caller for a ranked/limited
	// view of the same condition.
	List(ctx context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error)
	// ListActivationCandidates returns identity's "activate"-filter
	// items (same condition as List's "activate" branch — see that
	// method's doc comment), ordered by Lookups DESC and capped at
	// limit (limit <= 0 means unlimited) — pushed down into SQL (ORDER
	// BY ... LIMIT), the same convention PriorityRepository.Top uses
	// for its analogous "top N" query, rather than fetching everything
	// and sorting/capping in Go. This is what backs
	// application/planner.Planner.ActivationCandidates, called on every
	// feedback request (see application/feedback.Service), so it stays
	// one indexed query regardless of how large a learner's vocabulary
	// grows.
	ListActivationCandidates(ctx context.Context, identity learner.IdentityID, limit int) ([]vocabulary.Item, error)
	// AllExpressions returns every one of identity's vocabulary
	// expressions mapped to its item ID — the candidate set
	// application/vocabulary.Service.DetectProduction scans a reviewed
	// text against.
	AllExpressions(ctx context.Context, identity learner.IdentityID) (map[string]string, error)
	// SeedBank inserts entries as expression-bank baseline items (Task
	// 7, PRD §55/§17.5) — the curated catalog from
	// data/expressions/core.yaml — each with Lookups/Productions/
	// SuccessfulProductions zero, all in ONE transaction (mirroring
	// GrammarRepository.UpsertConcepts' bulk-in-one-tx shape for the
	// analogous curated-catalog seed path), but ONLY inserting entries
	// whose (identity, expression) has no existing row. Unlike
	// UpsertOnLookup, an entry that already has a row (because a prior
	// `jlp seed` already inserted it, or the learner has since looked it
	// up or produced it for real) is a silent no-op for that entry: it
	// must never reset or touch that row's counts. Implemented per-entry
	// as INSERT ... ON CONFLICT (identity_id, expression) DO NOTHING
	// against the same UNIQUE constraint UpsertOnLookup's own ON
	// CONFLICT targets — see cmd/jlp/seed.go's seedExpressionBank for
	// the only intended caller.
	SeedBank(ctx context.Context, identity learner.IdentityID, entries []vocabulary.BankEntry, at time.Time) error
	// BulkUpsertWords upserts words by (identity_id, expression) in ONE
	// transaction (Phase 3 Task 8: POST /api/v1/words, Nihongo Daily's
	// bulk sync contract) and returns how many rows were
	// created-or-updated (== len(words); every entry is validated by
	// application/vocabulary.Service.IngestWords before this is called,
	// so each one always touches exactly one row).
	//
	// Unlike UpsertOnLookup, counters (Lookups/Productions/
	// SuccessfulProductions) are NEVER touched — importing a deck is not
	// a lookup event — and Kind is always vocabulary.KindWord on first
	// insert. Sparse re-sync must not erase richer data already on
	// file: an empty incoming Expression/Reading/Meaning/MeaningEN/
	// Source string does NOT overwrite an existing non-empty value, and
	// JLPTLevel 0 does not overwrite a known level. Tags is the one
	// field that replaces wholesale rather than merging: a non-empty
	// incoming Tags list replaces the stored list outright (never
	// unioned), while an empty/nil incoming Tags leaves the stored list
	// untouched.
	BulkUpsertWords(ctx context.Context, identity learner.IdentityID, words []WordInput, at time.Time) (int, error)
}

// WordInput is one entry of POST /api/v1/words's "words" array,
// translated from the wire's "kanji" field name into vocabulary.Item's
// own Expression — see internal/adapters/http/words.go's wordInputDTO
// for the wire shape this is decoded from.
type WordInput struct {
	Expression string // "kanji" on the wire
	Reading    string
	Meaning    string // Japanese definition
	MeaningEN  string
	JLPTLevel  int // 0..5, 0 = unknown
	Tags       []string
	Source     string
}
