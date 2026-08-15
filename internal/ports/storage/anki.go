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
	// with ErrNotFound. Also used internally by
	// application/anki.Service.PushToAnkiConnect to revert a card taken
	// by TakeApprovedForExport back to "approved" when the AnkiConnect
	// push fails — status is not restricted to an enum here, so that
	// revert is just an ordinary UpdateStatus(ctx, identity, id,
	// "approved") call, no separate method needed.
	UpdateStatus(ctx context.Context, identity learner.IdentityID, id, status string) (AnkiCard, error)
	// TakeApprovedForExport atomically reads every "approved" card for
	// identity AND marks them "exported", in ONE transaction (postgres:
	// SELECT ... FOR UPDATE, then UPDATE ... for exactly those ids),
	// returning the taken cards (with Status already "exported"). This
	// replaced a separate ApprovedForExport+MarkExported pair (Phase 3
	// Task 3 code review, finding 2): those were two sequential,
	// non-transactional calls, which left a real (if narrow) race for
	// two concurrent callers of the same identity to both read the same
	// approved set before either one marked it, double-exporting rows.
	//
	// Concurrency contract: two callers racing for the same identity get
	// DISJOINT results — the row lock SELECT ... FOR UPDATE takes means
	// exactly one caller's transaction proceeds first and sees (and
	// takes) the full approved set; any other concurrent caller either
	// blocks until the first commits (then sees nothing left approved,
	// so returns empty) or, under a stricter isolation level, would
	// itself fail — either way, no card is ever handed to two callers.
	// at is accepted for future use / caller-controlled clock injection
	// (this codebase's usual "inject the clock" convention) but is not
	// currently persisted anywhere (anki_cards has no exported_at
	// column).
	//
	// A caller that needs to undo a take (e.g.
	// application/anki.Service.PushToAnkiConnect, when the subsequent
	// AnkiConnect push fails) does so via UpdateStatus(ctx, identity,
	// id, "approved") per card — see that method's doc comment.
	TakeApprovedForExport(ctx context.Context, identity learner.IdentityID, at time.Time) ([]AnkiCard, error)
}
