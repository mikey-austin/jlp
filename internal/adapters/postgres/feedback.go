package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type FeedbackRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewFeedbackRepository(pool *pgxpool.Pool) *FeedbackRepository {
	return &FeedbackRepository{pool: pool, q: sqlcgen.New(pool)}
}

// InsertFeedback writes rec and its corrections in one transaction —
// the same WithTx pattern DocumentRepository.Save uses — so a
// feedback_requests row and its corrections rows always land together,
// or neither does: a review the learner sees always has a durable
// trace of every correction it offered.
func (r *FeedbackRepository) InsertFeedback(ctx context.Context, rec storage.FeedbackRecord, corrections []storage.CorrectionRecord) error {
	id, err := parseUUID(rec.ID)
	if err != nil {
		return fmt.Errorf("feedback id: %w", err)
	}
	sessionID, err := parseUUID(string(rec.SessionID))
	if err != nil {
		return fmt.Errorf("session id: %w", err)
	}
	documentID, err := parseUUID(string(rec.DocumentID))
	if err != nil {
		return fmt.Errorf("document id: %w", err)
	}
	aiRequestID, err := toOptionalUUID(rec.AIRequestID)
	if err != nil {
		return fmt.Errorf("ai request id: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	qtx := r.q.WithTx(tx)
	if err := qtx.InsertFeedbackRequest(ctx, sqlcgen.InsertFeedbackRequestParams{
		ID:             id,
		IdentityID:     string(rec.IdentityID),
		SessionID:      sessionID,
		DocumentID:     documentID,
		SelectionStart: int32(rec.SelectionStart),
		SelectionEnd:   int32(rec.SelectionEnd),
		SelectionText:  rec.SelectionText,
		CorrectedText:  rec.CorrectedText,
		AiRequestID:    aiRequestID,
		CreatedAt:      now,
	}); err != nil {
		return err
	}

	for _, c := range corrections {
		cid, err := parseUUID(c.ID)
		if err != nil {
			return fmt.Errorf("correction id: %w", err)
		}
		if err := qtx.InsertCorrection(ctx, sqlcgen.InsertCorrectionParams{
			ID:                cid,
			FeedbackRequestID: id,
			Position:          int32(c.Position),
			Original:          c.Original,
			Replacement:       c.Replacement,
			Type:              c.Type,
			Severity:          c.Severity,
			ExplanationJa:     c.ExplanationJA,
			ExplanationEn:     c.ExplanationEN,
			Status:            c.Status,
			CreatedAt:         now,
		}); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// UpdateCorrectionStatus re-checks identity via a join to
// feedback_requests (see db/queries/feedback.sql): a correctionID that
// exists but belongs to another identity's feedback misses exactly
// like one that doesn't exist at all, so callers can't distinguish
// "not yours" from "not found" — the same access-control shape every
// other identity-scoped repository in this package uses.
func (r *FeedbackRepository) UpdateCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (storage.CorrectionRecord, error) {
	id, err := parseUUID(correctionID)
	if err != nil {
		return storage.CorrectionRecord{}, fmt.Errorf("correction id: %w", err)
	}
	row, err := r.q.UpdateCorrectionStatus(ctx, sqlcgen.UpdateCorrectionStatusParams{
		ID:         id,
		IdentityID: string(identity),
		Status:     status,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.CorrectionRecord{}, storage.ErrNotFound
		}
		return storage.CorrectionRecord{}, err
	}
	return fromUpdateCorrectionStatusRow(row), nil
}

// toOptionalUUID converts an optional canonical UUID string (empty
// means "not set") to the nullable pgtype sqlc generates for
// feedback_requests.ai_request_id: empty maps to an invalid (SQL NULL)
// pgtype.UUID, matching that column being nullable — a fakeai-backed
// review never goes through the observability decorator that stamps
// StructuredResponse.RequestID, so this is the normal case in tests and
// offline dev, not an error condition.
func toOptionalUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	return parseUUID(s)
}

func fromUpdateCorrectionStatusRow(row sqlcgen.UpdateCorrectionStatusRow) storage.CorrectionRecord {
	return storage.CorrectionRecord{
		ID:            uuid.UUID(row.ID.Bytes).String(),
		FeedbackID:    uuid.UUID(row.FeedbackRequestID.Bytes).String(),
		Position:      int(row.Position),
		Original:      row.Original,
		Replacement:   row.Replacement,
		Type:          row.Type,
		Severity:      row.Severity,
		ExplanationJA: row.ExplanationJa,
		ExplanationEN: row.ExplanationEn,
		Status:        row.Status,
		SessionID:     session.ID(uuid.UUID(row.SessionID.Bytes).String()),
	}
}
