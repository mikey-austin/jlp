package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ReadingRepository persists the 読解 pipeline (00030 migration): see
// storage.ReadingRepository for the contract, and db/queries/reading.sql
// for why each query is shaped the way it is — the claim/complete pairs
// in particular.
type ReadingRepository struct {
	q *sqlcgen.Queries
}

func NewReadingRepository(pool *pgxpool.Pool) *ReadingRepository {
	return &ReadingRepository{q: sqlcgen.New(pool)}
}

var _ storage.ReadingRepository = (*ReadingRepository)(nil)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func optTS(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return ts(*t)
}

func fromOptTS(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

func (r *ReadingRepository) UpsertArticle(ctx context.Context, a reading.Article) (reading.Article, bool, error) {
	id, err := parseUUID(a.ID)
	if err != nil {
		return reading.Article{}, false, fmt.Errorf("article id: %w", err)
	}
	paras, err := json.Marshal(a.Paragraphs)
	if err != nil {
		return reading.Article{}, false, err
	}
	row, err := r.q.UpsertReadingArticle(ctx, sqlcgen.UpsertReadingArticleParams{
		ID:          id,
		IdentityID:  string(a.IdentityID),
		SourceUrl:   a.SourceURL,
		SourceName:  a.SourceName,
		Title:       a.Title,
		Author:      a.Author,
		PublishedAt: optTS(a.PublishedAt),
		Paragraphs:  paras,
		ContentHash: a.ContentHash,
		CreatedAt:   ts(a.CreatedAt),
	})
	if err != nil {
		return reading.Article{}, false, err
	}
	stored, err := articleFrom(row.ID, row.IdentityID, row.SourceUrl, row.SourceName, row.Title, row.Author, row.PublishedAt, row.Paragraphs, row.ContentHash, row.CreatedAt)
	return stored, row.Inserted, err
}

func (r *ReadingRepository) GetArticle(ctx context.Context, identity learner.IdentityID, id string) (reading.Article, error) {
	pgID, err := parseUUID(id)
	if err != nil {
		return reading.Article{}, storage.ErrNotFound
	}
	row, err := r.q.GetReadingArticle(ctx, sqlcgen.GetReadingArticleParams{ID: pgID, IdentityID: string(identity)})
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.Article{}, storage.ErrNotFound
	}
	if err != nil {
		return reading.Article{}, err
	}
	return articleFrom(row.ID, row.IdentityID, row.SourceUrl, row.SourceName, row.Title, row.Author, row.PublishedAt, row.Paragraphs, row.ContentHash, row.CreatedAt)
}

func articleFrom(id pgtype.UUID, identity, srcURL, srcName, title, author string, published pgtype.Timestamptz, paras []byte, hash string, created pgtype.Timestamptz) (reading.Article, error) {
	var ps []string
	if err := json.Unmarshal(paras, &ps); err != nil {
		return reading.Article{}, fmt.Errorf("article %s paragraphs: %w", uuidString(id), err)
	}
	return reading.Article{
		ID:          uuidString(id),
		IdentityID:  learner.IdentityID(identity),
		SourceURL:   srcURL,
		SourceName:  srcName,
		Title:       title,
		Author:      author,
		PublishedAt: fromOptTS(published),
		Paragraphs:  ps,
		ContentHash: hash,
		CreatedAt:   created.Time,
	}, nil
}

func (r *ReadingRepository) SoftDeleteArticle(ctx context.Context, identity learner.IdentityID, id string, at time.Time) error {
	pgID, err := parseUUID(id)
	if err != nil {
		return storage.ErrNotFound
	}
	n, err := r.q.SoftDeleteReadingArticle(ctx, sqlcgen.SoftDeleteReadingArticleParams{ID: pgID, IdentityID: string(identity), At: ts(at)})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *ReadingRepository) RestoreArticle(ctx context.Context, identity learner.IdentityID, id string) error {
	pgID, err := parseUUID(id)
	if err != nil {
		return storage.ErrNotFound
	}
	n, err := r.q.RestoreReadingArticle(ctx, sqlcgen.RestoreReadingArticleParams{ID: pgID, IdentityID: string(identity)})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *ReadingRepository) InsertEdition(ctx context.Context, e reading.StudyEdition) error {
	id, err := parseUUID(e.ID)
	if err != nil {
		return fmt.Errorf("edition id: %w", err)
	}
	articleID, err := parseUUID(e.ArticleID)
	if err != nil {
		return fmt.Errorf("article id: %w", err)
	}
	return r.q.InsertReadingEdition(ctx, sqlcgen.InsertReadingEditionParams{
		ID:               id,
		ArticleID:        articleID,
		IdentityID:       string(e.IdentityID),
		Status:           string(e.Status),
		PromptName:       e.PromptName,
		PromptVersion:    e.PromptVersion,
		SchemaName:       e.SchemaName,
		DeliverWhenReady: e.DeliverWhenReady,
		NextAttemptAt:    ts(e.CreatedAt),
		CreatedAt:        ts(e.CreatedAt),
	})
}

// editionFrom builds a domain edition from the columns every edition
// query selects. sqlc gives each query its own row type, so the rows are
// passed through by field rather than by type.
func editionFrom(id, articleID pgtype.UUID, identity, status, promptName, promptVersion, schemaName string, lesson []byte, aiRequestID string, attempts int32, lastError string, deliver bool, created, updated pgtype.Timestamptz) (reading.StudyEdition, error) {
	e := reading.StudyEdition{
		ID:               uuidString(id),
		ArticleID:        uuidString(articleID),
		IdentityID:       learner.IdentityID(identity),
		Status:           reading.EditionStatus(status),
		PromptName:       promptName,
		PromptVersion:    promptVersion,
		SchemaName:       schemaName,
		AIRequestID:      aiRequestID,
		Attempts:         int(attempts),
		LastError:        lastError,
		DeliverWhenReady: deliver,
		CreatedAt:        created.Time,
		UpdatedAt:        updated.Time,
	}
	if len(lesson) > 0 {
		var l reading.Lesson
		if err := json.Unmarshal(lesson, &l); err != nil {
			return reading.StudyEdition{}, fmt.Errorf("edition %s lesson: %w", e.ID, err)
		}
		e.Lesson = &l
	}
	return e, nil
}

func (r *ReadingRepository) LatestEdition(ctx context.Context, identity learner.IdentityID, articleID, promptName, promptVersion string) (reading.StudyEdition, error) {
	pgID, err := parseUUID(articleID)
	if err != nil {
		return reading.StudyEdition{}, storage.ErrNotFound
	}
	row, err := r.q.LatestReadingEdition(ctx, sqlcgen.LatestReadingEditionParams{
		ArticleID: pgID, IdentityID: string(identity), PromptName: promptName, PromptVersion: promptVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.StudyEdition{}, storage.ErrNotFound
	}
	if err != nil {
		return reading.StudyEdition{}, err
	}
	return editionFrom(row.ID, row.ArticleID, row.IdentityID, row.Status, row.PromptName, row.PromptVersion, row.SchemaName, row.Lesson, row.AiRequestID, row.Attempts, row.LastError, row.DeliverWhenReady, row.CreatedAt, row.UpdatedAt)
}

func (r *ReadingRepository) GetEdition(ctx context.Context, identity learner.IdentityID, id string) (reading.StudyEdition, error) {
	pgID, err := parseUUID(id)
	if err != nil {
		return reading.StudyEdition{}, storage.ErrNotFound
	}
	row, err := r.q.GetReadingEdition(ctx, sqlcgen.GetReadingEditionParams{ID: pgID, IdentityID: string(identity)})
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.StudyEdition{}, storage.ErrNotFound
	}
	if err != nil {
		return reading.StudyEdition{}, err
	}
	return editionFrom(row.ID, row.ArticleID, row.IdentityID, row.Status, row.PromptName, row.PromptVersion, row.SchemaName, row.Lesson, row.AiRequestID, row.Attempts, row.LastError, row.DeliverWhenReady, row.CreatedAt, row.UpdatedAt)
}

func (r *ReadingRepository) ListEditions(ctx context.Context, identity learner.IdentityID) ([]storage.ReadingEditionSummary, error) {
	rows, err := r.q.ListReadingEditions(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.ReadingEditionSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.ReadingEditionSummary{
			EditionID:      uuidString(row.EditionID),
			ArticleID:      uuidString(row.ArticleID),
			Title:          row.Title,
			SourceName:     row.SourceName,
			SourceURL:      row.SourceUrl,
			PublishedAt:    fromOptTS(row.PublishedAt),
			Status:         reading.EditionStatus(row.Status),
			LastError:      row.LastError,
			DeliveryStatus: reading.DeliveryStatus(row.DeliveryStatus),
			CreatedAt:      row.ArticleCreatedAt.Time,
		})
	}
	// DISTINCT ON forces the query's ORDER BY to lead with the article
	// id; the page wants newest first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r *ReadingRepository) ClaimDueEdition(ctx context.Context, now, staleBefore time.Time) (storage.ClaimedEdition, bool, error) {
	row, err := r.q.ClaimDueReadingEdition(ctx, sqlcgen.ClaimDueReadingEditionParams{Now: ts(now), StaleBefore: ts(staleBefore)})
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ClaimedEdition{}, false, nil
	}
	if err != nil {
		return storage.ClaimedEdition{}, false, err
	}
	e, err := editionFrom(row.ID, row.ArticleID, row.IdentityID, row.Status, row.PromptName, row.PromptVersion, row.SchemaName, row.Lesson, row.AiRequestID, row.Attempts, row.LastError, row.DeliverWhenReady, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return storage.ClaimedEdition{}, false, err
	}
	// ClaimedAt comes back from postgres, at postgres' microsecond
	// precision, so quoting it back in Complete/Fail matches exactly.
	return storage.ClaimedEdition{StudyEdition: e, ClaimedAt: row.ClaimedAt.Time}, true, nil
}

func (r *ReadingRepository) CompleteEdition(ctx context.Context, c storage.ClaimedEdition, lesson []byte, aiRequestID string, now time.Time) error {
	id, err := parseUUID(c.ID)
	if err != nil {
		return storage.ErrNotFound
	}
	n, err := r.q.CompleteReadingEdition(ctx, sqlcgen.CompleteReadingEditionParams{
		ID: id, Lesson: lesson, AiRequestID: aiRequestID, UpdatedAt: ts(now), ClaimedAt: ts(c.ClaimedAt),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *ReadingRepository) FailEditionAttempt(ctx context.Context, c storage.ClaimedEdition, msg string, retry bool, retryAt, now time.Time) error {
	id, err := parseUUID(c.ID)
	if err != nil {
		return storage.ErrNotFound
	}
	n, err := r.q.FailReadingEditionAttempt(ctx, sqlcgen.FailReadingEditionAttemptParams{
		ID: id, LastError: msg, NextAttemptAt: ts(retryAt), UpdatedAt: ts(now), Retry: retry, ClaimedAt: ts(c.ClaimedAt),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func deliveryFrom(id, editionID pgtype.UUID, identity, dest, status string, attempts int32, lastError string, created, sent pgtype.Timestamptz) reading.Delivery {
	return reading.Delivery{
		ID:          uuidString(id),
		EditionID:   uuidString(editionID),
		IdentityID:  learner.IdentityID(identity),
		Destination: dest,
		Status:      reading.DeliveryStatus(status),
		Attempts:    int(attempts),
		LastError:   lastError,
		CreatedAt:   created.Time,
		SentAt:      fromOptTS(sent),
	}
}

func (r *ReadingRepository) QueueDelivery(ctx context.Context, d reading.Delivery) (reading.Delivery, bool, error) {
	id, err := parseUUID(d.ID)
	if err != nil {
		return reading.Delivery{}, false, fmt.Errorf("delivery id: %w", err)
	}
	editionID, err := parseUUID(d.EditionID)
	if err != nil {
		return reading.Delivery{}, false, fmt.Errorf("edition id: %w", err)
	}
	row, err := r.q.InsertReadingDelivery(ctx, sqlcgen.InsertReadingDeliveryParams{
		ID: id, EditionID: editionID, IdentityID: string(d.IdentityID), Destination: d.Destination, NextAttemptAt: ts(d.CreatedAt),
	})
	if err == nil {
		return deliveryFrom(row.ID, row.EditionID, row.IdentityID, row.Destination, row.Status, row.Attempts, row.LastError, row.CreatedAt, row.SentAt), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return reading.Delivery{}, false, err
	}
	// ON CONFLICT DO NOTHING: one is already in flight. Read it back.
	// (If it finished in the instant between the two statements this
	// misses, and the caller sees ErrNotFound — a retry then queues a
	// fresh delivery, which is the right outcome for a finished one.)
	cur, err := r.q.InFlightReadingDelivery(ctx, sqlcgen.InFlightReadingDeliveryParams{EditionID: editionID, Destination: d.Destination})
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.Delivery{}, false, storage.ErrNotFound
	}
	if err != nil {
		return reading.Delivery{}, false, err
	}
	return deliveryFrom(cur.ID, cur.EditionID, cur.IdentityID, cur.Destination, cur.Status, cur.Attempts, cur.LastError, cur.CreatedAt, cur.SentAt), false, nil
}

func (r *ReadingRepository) ListDeliveries(ctx context.Context, identity learner.IdentityID, editionID string) ([]reading.Delivery, error) {
	pgID, err := parseUUID(editionID)
	if err != nil {
		return nil, nil
	}
	rows, err := r.q.ListReadingDeliveries(ctx, sqlcgen.ListReadingDeliveriesParams{EditionID: pgID, IdentityID: string(identity)})
	if err != nil {
		return nil, err
	}
	out := make([]reading.Delivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, deliveryFrom(row.ID, row.EditionID, row.IdentityID, row.Destination, row.Status, row.Attempts, row.LastError, row.CreatedAt, row.SentAt))
	}
	return out, nil
}

func (r *ReadingRepository) ClaimDueDelivery(ctx context.Context, now, staleBefore time.Time) (storage.ClaimedDelivery, bool, error) {
	row, err := r.q.ClaimDueReadingDelivery(ctx, sqlcgen.ClaimDueReadingDeliveryParams{Now: ts(now), StaleBefore: ts(staleBefore)})
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ClaimedDelivery{}, false, nil
	}
	if err != nil {
		return storage.ClaimedDelivery{}, false, err
	}
	d := deliveryFrom(row.ID, row.EditionID, row.IdentityID, row.Destination, row.Status, row.Attempts, row.LastError, row.CreatedAt, row.SentAt)
	return storage.ClaimedDelivery{Delivery: d, ClaimedAt: row.ClaimedAt.Time}, true, nil
}

func (r *ReadingRepository) MarkDeliverySent(ctx context.Context, c storage.ClaimedDelivery, at time.Time) error {
	id, err := parseUUID(c.ID)
	if err != nil {
		return storage.ErrNotFound
	}
	n, err := r.q.MarkReadingDeliverySent(ctx, sqlcgen.MarkReadingDeliverySentParams{ID: id, SentAt: ts(at), ClaimedAt: ts(c.ClaimedAt)})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *ReadingRepository) FailDeliveryAttempt(ctx context.Context, c storage.ClaimedDelivery, msg string, retry bool, retryAt time.Time) error {
	id, err := parseUUID(c.ID)
	if err != nil {
		return storage.ErrNotFound
	}
	n, err := r.q.FailReadingDeliveryAttempt(ctx, sqlcgen.FailReadingDeliveryAttemptParams{
		ID: id, LastError: msg, NextAttemptAt: ts(retryAt), Retry: retry, ClaimedAt: ts(c.ClaimedAt),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}
