package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
)

// ReadingEditionSummary is one row of /reading's list: an article and
// its newest edition, with that edition's newest delivery status
// ("" when it has never been sent).
type ReadingEditionSummary struct {
	EditionID      string
	ArticleID      string
	Title          string
	SourceName     string
	SourceURL      string
	PublishedAt    *time.Time
	Status         reading.EditionStatus
	LastError      string
	DeliveryStatus reading.DeliveryStatus
	CreatedAt      time.Time
}

// ClaimedEdition is an edition a worker has claimed for one analysis
// attempt. ClaimedAt is the claim's token: completing or failing the
// attempt must quote it back, so a worker whose claim went stale (and
// was taken over by another) cannot overwrite the newer attempt.
type ClaimedEdition struct {
	reading.StudyEdition
	ClaimedAt time.Time
}

// ClaimedDelivery is ClaimedEdition's counterpart for deliveries.
type ClaimedDelivery struct {
	reading.Delivery
	ClaimedAt time.Time
}

// ReadingRepository persists the 読解 pipeline: articles, their study
// editions, and Send-to-Kindle deliveries.
//
// Every learner-facing method is identity-scoped with this package's
// usual contract: an id that belongs to someone else misses with
// ErrNotFound exactly like an id that does not exist. The Claim*/
// Complete*/Fail*/MarkDeliverySent methods are the background worker's
// and are deliberately NOT identity-scoped — the worker serves every
// learner, and the rows it touches carry their own identity.
type ReadingRepository interface {
	// UpsertArticle inserts a, or — when identity already has an article
	// with the same ContentHash — returns that existing article instead
	// (restoring it if it was soft-deleted). created reports which.
	UpsertArticle(ctx context.Context, a reading.Article) (stored reading.Article, created bool, err error)
	// GetArticle reads one visible (not soft-deleted) article.
	GetArticle(ctx context.Context, identity learner.IdentityID, id string) (reading.Article, error)
	// SaveTranslation stores a translated article's Japanese title and
	// paragraphs and the original beside them. The worker's path, so not
	// identity-scoped (like CompleteEdition).
	SaveTranslation(ctx context.Context, articleID string, a reading.Article) error
	// SoftDeleteArticle hides an article and every edition of it from
	// every read. Idempotent; ErrNotFound for someone else's article.
	SoftDeleteArticle(ctx context.Context, identity learner.IdentityID, id string, at time.Time) error
	// RestoreArticle undoes SoftDeleteArticle.
	RestoreArticle(ctx context.Context, identity learner.IdentityID, id string) error
	// AttachFigures stores figs as articleID's figures unless it already
	// has some; attached reports which. All or nothing, and safe against
	// a racing attach. ErrNotFound-free: a foreign or deleted article
	// simply attaches nothing.
	AttachFigures(ctx context.Context, identity learner.IdentityID, articleID string, figs []reading.Figure) (attached bool, err error)
	// ListFigures returns a visible article's figures in order, without
	// their bytes.
	ListFigures(ctx context.Context, identity learner.IdentityID, articleID string) ([]reading.Figure, error)
	// FigureData returns one figure with its bytes, or ErrNotFound.
	FigureData(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error)

	// InsertEdition queues a new pending edition, due at e.CreatedAt.
	InsertEdition(ctx context.Context, e reading.StudyEdition) error
	// LatestEdition returns the newest edition of articleID made with
	// promptName/promptVersion, or ErrNotFound when there is none.
	LatestEdition(ctx context.Context, identity learner.IdentityID, articleID, promptName, promptVersion string) (reading.StudyEdition, error)
	// GetEdition reads one edition whose article is visible.
	GetEdition(ctx context.Context, identity learner.IdentityID, id string) (reading.StudyEdition, error)
	// ListEditions returns one summary per visible article, newest
	// article first.
	ListEditions(ctx context.Context, identity learner.IdentityID) ([]ReadingEditionSummary, error)

	// ClaimDueEdition claims the oldest edition that is pending and due
	// at now, or stuck in analysing since before staleBefore, counting
	// the claim as an attempt. ok is false when there is nothing to do.
	ClaimDueEdition(ctx context.Context, now, staleBefore time.Time) (claimed ClaimedEdition, ok bool, err error)
	// CompleteEdition stores the lesson and marks the edition ready.
	// ErrNotFound when the claim is no longer current.
	CompleteEdition(ctx context.Context, c ClaimedEdition, lesson []byte, aiRequestID string, now time.Time) error
	// FailEditionAttempt records a failed attempt: back to pending, due
	// at retryAt, when retry is true; failed for good otherwise.
	// ErrNotFound when the claim is no longer current.
	FailEditionAttempt(ctx context.Context, c ClaimedEdition, msg string, retry bool, retryAt, now time.Time) error

	// QueueDelivery queues a pending delivery of d.EditionID to
	// d.Destination — unless one is already in flight for that pair, in
	// which case the in-flight delivery is returned and created is false.
	QueueDelivery(ctx context.Context, d reading.Delivery) (stored reading.Delivery, created bool, err error)
	// ListDeliveries returns an edition's deliveries, newest first.
	ListDeliveries(ctx context.Context, identity learner.IdentityID, editionID string) ([]reading.Delivery, error)
	// ClaimDueDelivery is ClaimDueEdition for deliveries.
	ClaimDueDelivery(ctx context.Context, now, staleBefore time.Time) (claimed ClaimedDelivery, ok bool, err error)
	// MarkDeliverySent records a successful send.
	MarkDeliverySent(ctx context.Context, c ClaimedDelivery, at time.Time) error
	// FailDeliveryAttempt is FailEditionAttempt for deliveries.
	FailDeliveryAttempt(ctx context.Context, c ClaimedDelivery, msg string, retry bool, retryAt time.Time) error

	// AddDraftPage appends (or, when pageURL is already in the draft, replaces)
	// one page of blocks in identity's active draft, creating it with meta if
	// none exists, in one transaction. ErrDraftFull past MaxDraftPages new pages.
	// Seq is page*10000 + index within the page, so a replaced page keeps its
	// place in reading order.
	AddDraftPage(ctx context.Context, identity learner.IdentityID, meta reading.DraftMeta, pageURL string, blocks []reading.DraftBlock, now time.Time) (reading.DraftInfo, error)
	ActiveDraft(ctx context.Context, identity learner.IdentityID) (reading.DraftInfo, error) // ErrNotFound
	GetDraft(ctx context.Context, identity learner.IdentityID, id string) (reading.DraftInfo, error)
	// ListDraftBlocks returns the draft's blocks in reading order; image
	// bytes only when withData.
	ListDraftBlocks(ctx context.Context, identity learner.IdentityID, id string, withData bool) ([]reading.DraftBlock, error)
	// DraftImage returns one image block's figure with its bytes, or ErrNotFound.
	DraftImage(ctx context.Context, identity learner.IdentityID, id string, seq int) (reading.Figure, error)
	SetDraftBlockExcluded(ctx context.Context, identity learner.IdentityID, id string, seq int, excluded bool, now time.Time) error
	SetDraftTitle(ctx context.Context, identity learner.IdentityID, id, title string, now time.Time) error
	DeleteDraft(ctx context.Context, identity learner.IdentityID, id string) error
	// SweepDrafts deletes drafts untouched since before, blocks with them.
	SweepDrafts(ctx context.Context, before time.Time) (int, error)
}
