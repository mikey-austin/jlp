package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// AnkiCard is one generated flashcard in the Anki review queue (PRD
// §19). It has no dedicated domain package — unlike FeedbackRecord/
// CorrectionRecord above, a card is presentation data derived from a
// source (a correction today; PRD §19 lists recurring grammar
// problems, vocabulary, tutor lessons, and successful examples as
// future sources), not a first-class learning concept the rest of the
// system reasons about. SourceType/SourceID together identify what the
// card was generated from — "correction"/<correction id> is the only
// combination Phase 3 Task 3 actually produces. Status starts "draft"
// and moves to "approved" or "rejected" (application/anki.Service.
// SetStatus), and an approved card moves on to "exported" once
// ExportTSV or PushToAnkiConnect has actually sent it out — never
// backwards, and never skipping "approved" (a draft can't export
// directly).
type AnkiCard struct {
	ID                   string
	IdentityID           learner.IdentityID
	SourceType, SourceID string
	Front, Back, Notes   string
	Status               string // "draft" | "approved" | "rejected" | "exported"
	CreatedAt            time.Time
}

// AnkiCardRepository persists the Anki review queue. Every method
// except Insert is identity-scoped, matching this codebase's other
// identity-scoped repositories (see storage.FeedbackRepository's doc
// comment): a card ID that exists but belongs to a different identity
// misses with ErrNotFound from UpdateStatus, exactly like
// FeedbackRepository.UpdateCorrectionStatus.
type AnkiCardRepository interface {
	// Insert persists a newly generated draft card.
	Insert(ctx context.Context, c AnkiCard) error
	// List returns identity's cards, newest first. status filters to
	// exactly that status ("draft" | "approved" | "rejected" |
	// "exported"); an empty status returns every card regardless of
	// status.
	List(ctx context.Context, identity learner.IdentityID, status string) ([]AnkiCard, error)
	// UpdateStatus sets id's status (application/anki.Service.SetStatus
	// validates it's "approved" or "rejected" before calling this) and
	// returns the updated card. A wrong identity or unknown id both miss
	// with ErrNotFound.
	UpdateStatus(ctx context.Context, identity learner.IdentityID, id, status string) (AnkiCard, error)
	// ApprovedForExport returns every "approved" card for identity,
	// oldest first — the set ExportTSV and PushToAnkiConnect both read
	// from before deciding what to mark exported.
	ApprovedForExport(ctx context.Context, identity learner.IdentityID) ([]AnkiCard, error)
	// MarkExported flips exactly the given ids to "exported", scoped to
	// identity and restricted to cards still "approved" — calling it
	// twice with the same ids is a safe no-op the second time.
	MarkExported(ctx context.Context, identity learner.IdentityID, ids []string) error
}
