package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Draft queries are plain pgx rather than sqlc: AddDraftPage is a
// multi-statement transaction on r.pool anyway (like AttachFigures), and
// the reads are one-liners over the same two tables (00032 migration).

// pageStride spaces pages in seq: seq = page*pageStride + index, so a
// re-captured page keeps its slot in reading order without renumbering
// the others. A page can therefore hold at most pageStride-1 blocks.
const pageStride = 10000

const draftCols = `id, identity_id, title, source_name, source_url, author, published_at, created_at, updated_at`

func scanDraft(row pgx.Row) (reading.DraftInfo, error) {
	var (
		id           pgtype.UUID
		identity     string
		m            reading.DraftMeta
		published    pgtype.Timestamptz
		created, upd pgtype.Timestamptz
	)
	if err := row.Scan(&id, &identity, &m.Title, &m.SourceName, &m.SourceURL, &m.Author, &published, &created, &upd); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reading.DraftInfo{}, storage.ErrNotFound
		}
		return reading.DraftInfo{}, err
	}
	m.PublishedAt = fromOptTS(published)
	return reading.DraftInfo{ID: uuidString(id), Identity: learner.IdentityID(identity), Meta: m, CreatedAt: created.Time, UpdatedAt: upd.Time}, nil
}

func (r *ReadingRepository) AddDraftPage(ctx context.Context, identity learner.IdentityID, meta reading.DraftMeta, pageURL string, blocks []reading.DraftBlock, now time.Time) (reading.DraftInfo, error) {
	if len(blocks) >= pageStride {
		return reading.DraftInfo{}, fmt.Errorf("draft page has %d blocks; the limit is %d", len(blocks), pageStride-1)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return reading.DraftInfo{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Create-or-touch. The conflict branch leaves the meta alone (it is
	// the first page's) and takes the row lock, so two pages added at
	// once queue up rather than both computing the same page number.
	info, err := scanDraft(tx.QueryRow(ctx, `
INSERT INTO reading_drafts (id, identity_id, title, source_name, source_url, author, published_at, created_at, updated_at)
VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $7)
ON CONFLICT (identity_id) DO UPDATE SET updated_at = EXCLUDED.updated_at
RETURNING `+draftCols,
		string(identity), meta.Title, meta.SourceName, meta.SourceURL, meta.Author, optTS(meta.PublishedAt), ts(now)))
	if err != nil {
		return reading.DraftInfo{}, err
	}
	id, err := parseUUID(info.ID)
	if err != nil {
		return reading.DraftInfo{}, err
	}

	// A known non-empty page_url replaces that page; anything else is new.
	page, replacing := 0, false
	if pageURL != "" {
		err := tx.QueryRow(ctx, `SELECT page FROM reading_draft_blocks WHERE draft_id = $1 AND page_url = $2 LIMIT 1`, id, pageURL).Scan(&page)
		switch {
		case err == nil:
			replacing = true
		case !errors.Is(err, pgx.ErrNoRows):
			return reading.DraftInfo{}, err
		}
	}
	if replacing {
		if _, err := tx.Exec(ctx, `DELETE FROM reading_draft_blocks WHERE draft_id = $1 AND page = $2`, id, page); err != nil {
			return reading.DraftInfo{}, err
		}
	} else {
		var pages, maxPage int
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT page), COALESCE(max(page), 0) FROM reading_draft_blocks WHERE draft_id = $1`, id).Scan(&pages, &maxPage); err != nil {
			return reading.DraftInfo{}, err
		}
		if pages >= reading.MaxDraftPages {
			return reading.DraftInfo{}, reading.ErrDraftFull
		}
		page = maxPage + 1
	}

	batch := &pgx.Batch{}
	for i, b := range blocks {
		var data []byte
		if b.Kind == reading.BlockImage {
			data = b.Figure.Data
		}
		batch.Queue(`
INSERT INTO reading_draft_blocks (draft_id, seq, page, page_url, kind, text, caption, alt, media_type, width, height, sha256, data, excluded)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			id, page*pageStride+i, page, pageURL, string(b.Kind), b.Text, b.Figure.Caption, b.Figure.Alt, b.Figure.MediaType,
			b.Figure.Width, b.Figure.Height, b.Figure.SHA256, data, b.Excluded)
	}
	res := tx.SendBatch(ctx, batch)
	for range blocks {
		if _, err := res.Exec(); err != nil {
			_ = res.Close()
			return reading.DraftInfo{}, err
		}
	}
	if err := res.Close(); err != nil {
		return reading.DraftInfo{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return reading.DraftInfo{}, err
	}
	return info, nil
}

func (r *ReadingRepository) ActiveDraft(ctx context.Context, identity learner.IdentityID) (reading.DraftInfo, error) {
	return scanDraft(r.pool.QueryRow(ctx, `SELECT `+draftCols+` FROM reading_drafts WHERE identity_id = $1`, string(identity)))
}

func (r *ReadingRepository) GetDraft(ctx context.Context, identity learner.IdentityID, id string) (reading.DraftInfo, error) {
	pgID, err := parseUUID(id)
	if err != nil {
		return reading.DraftInfo{}, storage.ErrNotFound
	}
	return scanDraft(r.pool.QueryRow(ctx, `SELECT `+draftCols+` FROM reading_drafts WHERE id = $1 AND identity_id = $2`, pgID, string(identity)))
}

func (r *ReadingRepository) ListDraftBlocks(ctx context.Context, identity learner.IdentityID, id string, withData bool) ([]reading.DraftBlock, error) {
	pgID, err := parseUUID(id)
	if err != nil {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT b.seq, b.page, b.page_url, b.kind, b.text, b.caption, b.alt, b.media_type, b.width, b.height, b.sha256,
       CASE WHEN $3 THEN b.data END, b.excluded
FROM reading_draft_blocks b
JOIN reading_drafts d ON d.id = b.draft_id
WHERE b.draft_id = $1 AND d.identity_id = $2
ORDER BY b.seq`, pgID, string(identity), withData)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reading.DraftBlock
	for rows.Next() {
		var (
			b        reading.DraftBlock
			kind     string
			w, h     int32
			seq, pg  int32
			fig      = &b.Figure
			dataCols []byte
		)
		if err := rows.Scan(&seq, &pg, &b.PageURL, &kind, &b.Text, &fig.Caption, &fig.Alt, &fig.MediaType, &w, &h, &fig.SHA256, &dataCols, &b.Excluded); err != nil {
			return nil, err
		}
		b.Seq, b.Page, b.Kind = int(seq), int(pg), reading.BlockKind(kind)
		fig.Width, fig.Height = int(w), int(h)
		fig.Data = dataCols
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *ReadingRepository) DraftImage(ctx context.Context, identity learner.IdentityID, id string, seq int) (reading.Figure, error) {
	pgID, err := parseUUID(id)
	if err != nil {
		return reading.Figure{}, storage.ErrNotFound
	}
	var (
		f    reading.Figure
		w, h int32
	)
	err = r.pool.QueryRow(ctx, `
SELECT b.caption, b.alt, b.media_type, b.width, b.height, b.sha256, b.data
FROM reading_draft_blocks b
JOIN reading_drafts d ON d.id = b.draft_id
WHERE b.draft_id = $1 AND d.identity_id = $2 AND b.seq = $3 AND b.kind = 'image' AND b.data IS NOT NULL`,
		pgID, string(identity), int32(seq)).Scan(&f.Caption, &f.Alt, &f.MediaType, &w, &h, &f.SHA256, &f.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return reading.Figure{}, storage.ErrNotFound
	}
	if err != nil {
		return reading.Figure{}, err
	}
	f.Width, f.Height = int(w), int(h)
	return f, nil
}

func (r *ReadingRepository) SetDraftBlockExcluded(ctx context.Context, identity learner.IdentityID, id string, seq int, excluded bool, now time.Time) error {
	pgID, err := parseUUID(id)
	if err != nil {
		return storage.ErrNotFound
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The draft's updated_at moves only when the block exists: a tap on
	// nothing must not keep a stale draft alive past the sweep.
	tag, err := tx.Exec(ctx, `
UPDATE reading_draft_blocks b SET excluded = $4
FROM reading_drafts d
WHERE b.draft_id = d.id AND d.id = $1 AND d.identity_id = $2 AND b.seq = $3`, pgID, string(identity), int32(seq), excluded)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return storage.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE reading_drafts SET updated_at = $2 WHERE id = $1`, pgID, ts(now)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *ReadingRepository) SetDraftTitle(ctx context.Context, identity learner.IdentityID, id, title string, now time.Time) error {
	pgID, err := parseUUID(id)
	if err != nil {
		return storage.ErrNotFound
	}
	tag, err := r.pool.Exec(ctx, `UPDATE reading_drafts SET title = $3, updated_at = $4 WHERE id = $1 AND identity_id = $2`, pgID, string(identity), title, ts(now))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *ReadingRepository) DeleteDraft(ctx context.Context, identity learner.IdentityID, id string) error {
	pgID, err := parseUUID(id)
	if err != nil {
		return storage.ErrNotFound
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM reading_drafts WHERE id = $1 AND identity_id = $2`, pgID, string(identity))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *ReadingRepository) SweepDrafts(ctx context.Context, before time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM reading_drafts WHERE updated_at < $1`, ts(before))
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
