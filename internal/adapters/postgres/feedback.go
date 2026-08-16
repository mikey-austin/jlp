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

// InsertFeedback writes rec, its corrections, AND their concept tags
// (concepts, keyed by correction ID) in ONE transaction — the same
// WithTx pattern DocumentRepository.Save uses — so a feedback_requests
// row, its corrections rows, and their correction_concepts rows always
// land together, or none of them do. This atomicity is load-bearing,
// not just tidy: concept tagging used to run as a separate transaction
// after this one committed, so a failure there left feedback_requests
// and corrections durably committed while the client saw an error and
// (typically) retried under freshly-generated IDs — silently
// double-counting concept encounters downstream. Folding concepts into
// this same tx means a failure anywhere rolls back everything.
func (r *FeedbackRepository) InsertFeedback(ctx context.Context, rec storage.FeedbackRecord, corrections []storage.CorrectionRecord, concepts map[string][]storage.ConceptTag) error {
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
			HintJa:            c.HintJA,
			HintEn:            c.HintEN,
			Status:            c.Status,
			CreatedAt:         now,
		}); err != nil {
			return err
		}

		for _, tag := range concepts[c.ID] {
			if err := qtx.InsertCorrectionConcept(ctx, sqlcgen.InsertCorrectionConceptParams{
				CorrectionID: cid,
				ConceptSlug:  tag.Slug,
				Resolved:     tag.Resolved,
			}); err != nil {
				return fmt.Errorf("correction %s concept %q: %w", c.ID, tag.Slug, err)
			}
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

// RetryCorrection increments correctionID's attempts count and, if (and
// only if) trimmedAttempt exactly matches the stored Replacement,
// flips its Status to "accepted" — all in the single UPDATE
// db/queries/feedback.sql's RetryCorrection issues, so a race between
// two concurrent retries can't apply the increment twice while only
// one of them decides correctness. Same identity-scoped, "presented
// only" miss semantics as UpdateCorrectionStatus — see
// storage.FeedbackRepository's doc comment.
func (r *FeedbackRepository) RetryCorrection(ctx context.Context, identity learner.IdentityID, correctionID, trimmedAttempt string) (storage.CorrectionRecord, error) {
	id, err := parseUUID(correctionID)
	if err != nil {
		return storage.CorrectionRecord{}, fmt.Errorf("correction id: %w", err)
	}
	row, err := r.q.RetryCorrection(ctx, sqlcgen.RetryCorrectionParams{
		ID:          id,
		IdentityID:  string(identity),
		Replacement: trimmedAttempt,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.CorrectionRecord{}, storage.ErrNotFound
		}
		return storage.CorrectionRecord{}, err
	}
	return buildCorrectionRecord(row.ID, row.FeedbackRequestID, row.SessionID, row.Position,
		row.Original, row.Replacement, row.Type, row.Severity, row.ExplanationJa, row.ExplanationEn,
		row.HintJa, row.HintEn, row.Status, row.Attempts, row.Confidence, row.Revealed), nil
}

// RevealCorrection marks correctionID revealed (idempotent: revealing
// an already-revealed correction is a harmless no-op change). Same
// identity-scoped, "presented only" miss semantics as RetryCorrection.
func (r *FeedbackRepository) RevealCorrection(ctx context.Context, identity learner.IdentityID, correctionID string) (storage.CorrectionRecord, error) {
	id, err := parseUUID(correctionID)
	if err != nil {
		return storage.CorrectionRecord{}, fmt.Errorf("correction id: %w", err)
	}
	row, err := r.q.RevealCorrection(ctx, sqlcgen.RevealCorrectionParams{ID: id, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.CorrectionRecord{}, storage.ErrNotFound
		}
		return storage.CorrectionRecord{}, err
	}
	return buildCorrectionRecord(row.ID, row.FeedbackRequestID, row.SessionID, row.Position,
		row.Original, row.Replacement, row.Type, row.Severity, row.ExplanationJa, row.ExplanationEn,
		row.HintJa, row.HintEn, row.Status, row.Attempts, row.Confidence, row.Revealed), nil
}

// RecordConfidence sets correctionID's Confidence (already validated
// 1..5 by the caller — see storage.FeedbackRepository's doc comment).
// Unlike RetryCorrection/RevealCorrection this is NOT restricted to
// Status "presented": a learner may rate their confidence any time
// after a correction resolves.
func (r *FeedbackRepository) RecordConfidence(ctx context.Context, identity learner.IdentityID, correctionID string, confidence int) (storage.CorrectionRecord, error) {
	id, err := parseUUID(correctionID)
	if err != nil {
		return storage.CorrectionRecord{}, fmt.Errorf("correction id: %w", err)
	}
	row, err := r.q.RecordConfidence(ctx, sqlcgen.RecordConfidenceParams{
		ID:         id,
		IdentityID: string(identity),
		Confidence: pgtype.Int4{Int32: int32(confidence), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.CorrectionRecord{}, storage.ErrNotFound
		}
		return storage.CorrectionRecord{}, err
	}
	return buildCorrectionRecord(row.ID, row.FeedbackRequestID, row.SessionID, row.Position,
		row.Original, row.Replacement, row.Type, row.Severity, row.ExplanationJa, row.ExplanationEn,
		row.HintJa, row.HintEn, row.Status, row.Attempts, row.Confidence, row.Revealed), nil
}

// GetCorrectionConcepts returns correctionID's RESOLVED concept slugs,
// slug-ascending (the underlying query's ORDER BY concept_slug — there
// is no created_at on correction_concepts to order by insertion time
// instead). Unresolved tags are excluded, matching
// db/queries/grammar.sql's ConceptStats/CorrectionsForConcept
// "resolved only" convention; see the doc comment on
// storage.FeedbackRepository.GetCorrectionConcepts for why a caller
// (SetCorrectionStatus, re-rendering a correction card) needs this.
func (r *FeedbackRepository) GetCorrectionConcepts(ctx context.Context, correctionID string) ([]string, error) {
	cid, err := parseUUID(correctionID)
	if err != nil {
		return nil, fmt.Errorf("correction id: %w", err)
	}
	slugs, err := r.q.GetCorrectionConcepts(ctx, cid)
	if err != nil {
		return nil, err
	}
	return slugs, nil
}

// GetCorrection reads back one correction, identity-scoped via the same
// join UpdateCorrectionStatus uses: a wrong identity or unknown
// correction ID both miss with storage.ErrNotFound.
func (r *FeedbackRepository) GetCorrection(ctx context.Context, identity learner.IdentityID, correctionID string) (storage.CorrectionRecord, error) {
	id, err := parseUUID(correctionID)
	if err != nil {
		return storage.CorrectionRecord{}, fmt.Errorf("correction id: %w", err)
	}
	row, err := r.q.GetCorrection(ctx, sqlcgen.GetCorrectionParams{ID: id, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.CorrectionRecord{}, storage.ErrNotFound
		}
		return storage.CorrectionRecord{}, err
	}
	return buildCorrectionRecord(row.ID, row.FeedbackRequestID, row.SessionID, row.Position,
		row.Original, row.Replacement, row.Type, row.Severity, row.ExplanationJa, row.ExplanationEn,
		row.HintJa, row.HintEn, row.Status, row.Attempts, row.Confidence, row.Revealed), nil
}

// RecentCorrections reads back identity's most recent corrections
// across every session, newest first, at most limit — see
// storage.FeedbackRepository's doc comment.
func (r *FeedbackRepository) RecentCorrections(ctx context.Context, identity learner.IdentityID, limit int) ([]storage.CorrectionRecord, error) {
	rows, err := r.q.RecentCorrections(ctx, sqlcgen.RecentCorrectionsParams{
		IdentityID: string(identity),
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]storage.CorrectionRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildCorrectionRecord(row.ID, row.FeedbackRequestID, row.SessionID, row.Position,
			row.Original, row.Replacement, row.Type, row.Severity, row.ExplanationJa, row.ExplanationEn,
			row.HintJa, row.HintEn, row.Status, row.Attempts, row.Confidence, row.Revealed))
	}
	return out, nil
}

// ListForSession returns identity's feedback history for sessionID,
// most-recent-first — see storage.FeedbackRepository's doc comment for
// why a cross-identity sessionID comes back as an empty slice rather
// than storage.ErrNotFound.
func (r *FeedbackRepository) ListForSession(ctx context.Context, identity learner.IdentityID, sessionID session.ID) ([]storage.FeedbackSummary, error) {
	sid, err := parseUUID(string(sessionID))
	if err != nil {
		return nil, fmt.Errorf("session id: %w", err)
	}
	rows, err := r.q.ListFeedbackForSession(ctx, sqlcgen.ListFeedbackForSessionParams{
		IdentityID: string(identity),
		SessionID:  sid,
	})
	if err != nil {
		return nil, err
	}
	out := make([]storage.FeedbackSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.FeedbackSummary{
			ID:              uuid.UUID(row.ID.Bytes).String(),
			SelectionText:   row.SelectionText,
			CorrectionCount: int(row.CorrectionCount),
			Provider:        row.Provider,
			Model:           row.Model,
			CreatedAt:       row.CreatedAt.Time,
		})
	}
	return out, nil
}

// GetFeedback returns feedbackID's own FeedbackDetail plus every one of
// its corrections, identity-scoped via GetFeedbackRequest's join to
// feedback_requests — see storage.FeedbackRepository's doc comment for
// why ListCorrectionsForFeedback (the second query) doesn't repeat that
// check.
func (r *FeedbackRepository) GetFeedback(ctx context.Context, identity learner.IdentityID, feedbackID string) (storage.FeedbackDetail, []storage.CorrectionRecord, error) {
	id, err := parseUUID(feedbackID)
	if err != nil {
		return storage.FeedbackDetail{}, nil, fmt.Errorf("feedback id: %w", err)
	}
	row, err := r.q.GetFeedbackRequest(ctx, sqlcgen.GetFeedbackRequestParams{ID: id, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.FeedbackDetail{}, nil, storage.ErrNotFound
		}
		return storage.FeedbackDetail{}, nil, err
	}
	detail := storage.FeedbackDetail{
		ID:            uuid.UUID(row.ID.Bytes).String(),
		SelectionText: row.SelectionText,
		CorrectedText: row.CorrectedText,
		Provider:      row.Provider,
		Model:         row.Model,
		CreatedAt:     row.CreatedAt.Time,
	}

	corrRows, err := r.q.ListCorrectionsForFeedback(ctx, id)
	if err != nil {
		return storage.FeedbackDetail{}, nil, err
	}
	corrections := make([]storage.CorrectionRecord, 0, len(corrRows))
	for _, cr := range corrRows {
		corrections = append(corrections, buildCorrectionRecord(cr.ID, cr.FeedbackRequestID, cr.SessionID, cr.Position,
			cr.Original, cr.Replacement, cr.Type, cr.Severity, cr.ExplanationJa, cr.ExplanationEn,
			cr.HintJa, cr.HintEn, cr.Status, cr.Attempts, cr.Confidence, cr.Revealed))
	}
	return detail, corrections, nil
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
	return buildCorrectionRecord(row.ID, row.FeedbackRequestID, row.SessionID, row.Position,
		row.Original, row.Replacement, row.Type, row.Severity, row.ExplanationJa, row.ExplanationEn,
		row.HintJa, row.HintEn, row.Status, row.Attempts, row.Confidence, row.Revealed)
}

// buildCorrectionRecord assembles a storage.CorrectionRecord from the
// scalar columns every *-RETURNING corrections query in this file
// selects — UpdateCorrectionStatus, RetryCorrection, RevealCorrection,
// and RecordConfidence all return their own distinct sqlc row struct
// (one per query), but with an IDENTICAL column list, so each of their
// From*Row functions just unpacks its row into this one shared
// constructor rather than repeating the pgtype-to-domain conversion
// four times.
func buildCorrectionRecord(id, feedbackRequestID, sessionID pgtype.UUID, position int32,
	original, replacement, typ, severity, explanationJA, explanationEN, hintJA, hintEN, status string,
	attempts int32, confidence pgtype.Int4, revealed bool) storage.CorrectionRecord {
	return storage.CorrectionRecord{
		ID:            uuid.UUID(id.Bytes).String(),
		FeedbackID:    uuid.UUID(feedbackRequestID.Bytes).String(),
		Position:      int(position),
		Original:      original,
		Replacement:   replacement,
		Type:          typ,
		Severity:      severity,
		ExplanationJA: explanationJA,
		ExplanationEN: explanationEN,
		HintJA:        hintJA,
		HintEN:        hintEN,
		Status:        status,
		Attempts:      int(attempts),
		Confidence:    fromOptionalInt32(confidence),
		Revealed:      revealed,
		SessionID:     session.ID(uuid.UUID(sessionID.Bytes).String()),
	}
}

// fromOptionalInt32 converts a nullable pgtype.Int4 (corrections.confidence)
// to *int: nil when the column is SQL NULL (no confidence recorded
// yet), a pointer to the value otherwise.
func fromOptionalInt32(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}
