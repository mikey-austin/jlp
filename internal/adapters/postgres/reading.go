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
	"github.com/jackc/pgx/v5/pgconn"
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
	q    *sqlcgen.Queries
	pool *pgxpool.Pool
}

func NewReadingRepository(pool *pgxpool.Pool) *ReadingRepository {
	return &ReadingRepository{q: sqlcgen.New(pool), pool: pool}
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
	// One transaction: purging the deleted match and inserting its
	// replacement must not be seen half-done (the unique index would
	// otherwise resurrect the old row, or a failed insert lose both).
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return reading.Article{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := purgeDeletedArticle(ctx, tx, string(a.IdentityID), a.ContentHash); err != nil {
		return reading.Article{}, false, err
	}
	row, err := r.q.WithTx(tx).UpsertReadingArticle(ctx, sqlcgen.UpsertReadingArticleParams{
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
	if err := tx.Commit(ctx); err != nil {
		return reading.Article{}, false, err
	}
	stored, err := articleFrom(row.ID, row.IdentityID, row.SourceUrl, row.SourceName, row.Title, row.Author, row.PublishedAt, row.Paragraphs, row.ContentHash, row.CreatedAt)
	return stored, row.Inserted, err
}

// purgeDeletedArticle removes a soft-deleted article matching
// (identity, hash) together with its editions, their deliveries and its
// figures, so a re-import of deleted text is a new article with a new
// edition rather than the old one coming back. A live match is left
// alone. Undo is RestoreArticle's job and never reaches here.
func purgeDeletedArticle(ctx context.Context, tx pgx.Tx, identity, hash string) error {
	var id pgtype.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM reading_articles WHERE identity_id = $1 AND content_hash = $2 AND deleted_at IS NOT NULL`, identity, hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM reading_deliveries WHERE edition_id IN (SELECT id FROM reading_editions WHERE article_id = $1)`,
		`DELETE FROM reading_editions WHERE article_id = $1`,
		`DELETE FROM reading_article_figures WHERE article_id = $1`,
		`DELETE FROM reading_articles WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, stmt, id); err != nil {
			return err
		}
	}
	return nil
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

// attachFiguresSQL is one statement, so a set of figures lands whole or
// not at all. NOT EXISTS makes a resubmission a no-op once an article has
// figures; two racing attaches that both pass it collide on the primary
// key, and the loser's whole statement fails — read as "already
// attached". Scoped to a visible article of this identity. It lives here
// rather than in db/queries because sqlc cannot type a multi-array unnest.
const attachFiguresSQL = `
INSERT INTO reading_article_figures (article_id, ordinal, after_paragraph, caption, alt, is_lead, in_text, media_type, width, height, sha256, data)
SELECT a.id, f.ordinal, f.after_paragraph, f.caption, f.alt, f.is_lead, f.in_text, f.media_type, f.width, f.height, f.sha256, f.data
FROM reading_articles a,
     unnest($1::int[], $2::int[], $3::text[], $4::text[], $5::boolean[], $6::boolean[], $7::text[],
            $8::int[], $9::int[], $10::text[], $11::bytea[])
       AS f(ordinal, after_paragraph, caption, alt, is_lead, in_text, media_type, width, height, sha256, data)
WHERE a.id = $12 AND a.identity_id = $13 AND a.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM reading_article_figures x WHERE x.article_id = a.id)`

func (r *ReadingRepository) AttachFigures(ctx context.Context, identity learner.IdentityID, articleID string, figs []reading.Figure) (bool, error) {
	if len(figs) == 0 {
		return false, nil
	}
	id, err := parseUUID(articleID)
	if err != nil {
		return false, nil
	}
	var (
		ordinals, afters, widths, heights []int32
		captions, alts, mediaTypes, sums  []string
		leads, inTexts                    []bool
		datas                             [][]byte
	)
	for _, f := range figs {
		ordinals = append(ordinals, int32(f.Ordinal))
		afters = append(afters, int32(f.AfterParagraph))
		captions = append(captions, f.Caption)
		alts = append(alts, f.Alt)
		leads = append(leads, f.Lead)
		inTexts = append(inTexts, f.InText)
		mediaTypes = append(mediaTypes, f.MediaType)
		widths = append(widths, int32(f.Width))
		heights = append(heights, int32(f.Height))
		sums = append(sums, f.SHA256)
		datas = append(datas, f.Data)
	}
	tag, err := r.pool.Exec(ctx, attachFiguresSQL, ordinals, afters, captions, alts, leads, inTexts, mediaTypes, widths, heights, sums, datas, id, string(identity))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, nil // a racing attach won
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *ReadingRepository) ListFigures(ctx context.Context, identity learner.IdentityID, articleID string) ([]reading.Figure, error) {
	id, err := parseUUID(articleID)
	if err != nil {
		return nil, nil
	}
	rows, err := r.q.ListReadingFigures(ctx, sqlcgen.ListReadingFiguresParams{ArticleID: id, IdentityID: string(identity)})
	if err != nil {
		return nil, err
	}
	out := make([]reading.Figure, 0, len(rows))
	for _, x := range rows {
		out = append(out, reading.Figure{ArticleID: articleID, Ordinal: int(x.Ordinal), AfterParagraph: int(x.AfterParagraph),
			Caption: x.Caption, Alt: x.Alt, Lead: x.IsLead, InText: x.InText, MediaType: x.MediaType,
			Width: int(x.Width), Height: int(x.Height), SHA256: x.Sha256})
	}
	return out, nil
}

func (r *ReadingRepository) FigureData(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error) {
	id, err := parseUUID(articleID)
	if err != nil {
		return reading.Figure{}, storage.ErrNotFound
	}
	x, err := r.q.GetReadingFigure(ctx, sqlcgen.GetReadingFigureParams{ArticleID: id, IdentityID: string(identity), Ordinal: int32(ordinal)})
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.Figure{}, storage.ErrNotFound
	}
	if err != nil {
		return reading.Figure{}, err
	}
	return reading.Figure{ArticleID: articleID, Ordinal: int(x.Ordinal), AfterParagraph: int(x.AfterParagraph),
		Caption: x.Caption, Alt: x.Alt, Lead: x.IsLead, InText: x.InText, MediaType: x.MediaType,
		Width: int(x.Width), Height: int(x.Height), SHA256: x.Sha256, Data: x.Data}, nil
}
