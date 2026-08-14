//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func TestFeedbackInsertAndUpdateCorrectionStatus(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// FK chain: identities -> sessions -> documents -> feedback_requests -> corrections.
	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-feedback-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-feedback-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identityA.ID,
		Title:      "旅行について書く",
		Purpose:    "Diary",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	docs := NewDocumentRepository(pool)
	doc, _, err := docs.GetOrCreateForSession(ctx, identityA.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	feedback := NewFeedbackRepository(pool)

	feedbackID := uuid.New().String()
	correction1ID := uuid.New().String()
	correction2ID := uuid.New().String()
	rec := storage.FeedbackRecord{
		ID:             feedbackID,
		IdentityID:     identityA.ID,
		SessionID:      sess.ID,
		DocumentID:     doc.ID,
		SelectionStart: 0,
		SelectionEnd:   9,
		SelectionText:  "とても面白いでした",
		CorrectedText:  "とても面白かったです",
		// AIRequestID intentionally left empty: fakeai-backed reviews
		// (used throughout this task's tests) never go through the
		// observability decorator that stamps one, and the column is
		// nullable for exactly this reason.
	}
	corrections := []storage.CorrectionRecord{
		{
			ID:            correction1ID,
			FeedbackID:    feedbackID,
			Position:      0,
			Original:      "面白いでした",
			Replacement:   "面白かったです",
			Type:          "conjugation",
			Severity:      "incorrect",
			ExplanationJA: "い形容詞の過去形は「〜かった」を使います。",
			ExplanationEN: "い-adjectives form the past tense with 〜かった.",
			Status:        "presented",
		},
		{
			ID:            correction2ID,
			FeedbackID:    feedbackID,
			Position:      1,
			Original:      "とても",
			Replacement:   "非常に",
			Type:          "style",
			Severity:      "optional",
			ExplanationJA: "より正式な表現です。",
			ExplanationEN: "A more formal alternative.",
			Status:        "presented",
		},
	}

	if err := feedback.InsertFeedback(ctx, rec, corrections); err != nil {
		t.Fatal(err)
	}

	// UpdateCorrectionStatus with the right identity succeeds and
	// returns the refreshed row.
	updated, err := feedback.UpdateCorrectionStatus(ctx, identityA.ID, correction1ID, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "accepted" {
		t.Fatalf("Status = %q, want accepted", updated.Status)
	}
	if updated.ID != correction1ID {
		t.Fatalf("ID = %q, want %q", updated.ID, correction1ID)
	}
	if updated.FeedbackID != feedbackID {
		t.Fatalf("FeedbackID = %q, want %q", updated.FeedbackID, feedbackID)
	}
	if updated.Position != 0 {
		t.Fatalf("Position = %d, want 0", updated.Position)
	}
	if updated.Original != "面白いでした" || updated.Replacement != "面白かったです" {
		t.Fatalf("Original/Replacement = %q/%q, want 面白いでした/面白かったです", updated.Original, updated.Replacement)
	}
	if updated.ExplanationJA == "" || updated.ExplanationEN == "" {
		t.Fatal("explanation fields were not returned")
	}
	if updated.SessionID != sess.ID {
		t.Fatalf("SessionID = %q, want %q (the feedback pipeline's event needs this to scope correction.accepted to a session)", updated.SessionID, sess.ID)
	}

	// The second correction is untouched.
	untouched, err := feedback.UpdateCorrectionStatus(ctx, identityA.ID, correction2ID, "rejected")
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Status != "rejected" {
		t.Fatalf("Status = %q, want rejected", untouched.Status)
	}

	// UpdateCorrectionStatus with the WRONG identity misses with
	// storage.ErrNotFound rather than leaking or mutating another
	// learner's correction.
	if _, err := feedback.UpdateCorrectionStatus(ctx, identityB.ID, correction1ID, "rejected"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity UpdateCorrectionStatus err = %v, want storage.ErrNotFound", err)
	}

	// The wrong-identity attempt must not have mutated the row: it's
	// still "accepted" from the earlier, correctly-identified call.
	unchanged, err := feedback.UpdateCorrectionStatus(ctx, identityA.ID, correction1ID, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != "accepted" {
		t.Fatalf("after cross-identity attempt, Status = %q, want unchanged accepted", unchanged.Status)
	}

	// A correction ID that doesn't exist at all also misses with
	// storage.ErrNotFound.
	if _, err := feedback.UpdateCorrectionStatus(ctx, identityA.ID, uuid.NewString(), "accepted"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unknown correction id err = %v, want storage.ErrNotFound", err)
	}
}

// TestFeedbackInsertCorrectionConceptsRoundTripAndIdempotent pins Phase
// 2 Task 2's Step 3: InsertCorrectionConcepts writes one row per slug —
// resolved=true for a known slug, resolved=false for an unknown one,
// per the resolved map the caller supplies — and re-inserting the same
// (correction_id, concept_slug) pairs is a no-op (ON CONFLICT DO
// NOTHING), not a duplicate-row error.
func TestFeedbackInsertCorrectionConceptsRoundTripAndIdempotent(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-feedback-concepts-" + uuid.NewString()), DisplayName: "A"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identityA.ID,
		Title:      "文法タグ付けテスト",
		Purpose:    "Diary",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	docs := NewDocumentRepository(pool)
	doc, _, err := docs.GetOrCreateForSession(ctx, identityA.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	feedback := NewFeedbackRepository(pool)
	feedbackID := uuid.New().String()
	correctionID := uuid.New().String()
	rec := storage.FeedbackRecord{
		ID:             feedbackID,
		IdentityID:     identityA.ID,
		SessionID:      sess.ID,
		DocumentID:     doc.ID,
		SelectionStart: 0,
		SelectionEnd:   6,
		SelectionText:  "面白いでした",
		CorrectedText:  "面白かったです",
	}
	corrections := []storage.CorrectionRecord{{
		ID:            correctionID,
		FeedbackID:    feedbackID,
		Position:      0,
		Original:      "面白いでした",
		Replacement:   "面白かったです",
		Type:          "conjugation",
		Severity:      "incorrect",
		ExplanationJA: "い形容詞の過去形は「〜かった」を使います。",
		ExplanationEN: "い-adjectives form the past tense with 〜かった.",
		Status:        "presented",
	}}
	if err := feedback.InsertFeedback(ctx, rec, corrections); err != nil {
		t.Fatal(err)
	}

	slugs := []string{"i-adjective-past", "no-such-slug"}
	resolved := map[string]bool{"i-adjective-past": true, "no-such-slug": false}
	if err := feedback.InsertCorrectionConcepts(ctx, correctionID, slugs, resolved); err != nil {
		t.Fatal(err)
	}

	rows, err := selectCorrectionConcepts(ctx, pool, correctionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("correction_concepts rows = %d, want 2: %+v", len(rows), rows)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.slug] = r.resolved
	}
	if resolvedVal, ok := got["i-adjective-past"]; !ok || !resolvedVal {
		t.Fatalf("i-adjective-past row: present=%v resolved=%v, want present resolved=true", ok, resolvedVal)
	}
	if resolvedVal, ok := got["no-such-slug"]; !ok || resolvedVal {
		t.Fatalf("no-such-slug row: present=%v resolved=%v, want present resolved=false", ok, resolvedVal)
	}

	// Re-inserting the same pairs (ON CONFLICT DO NOTHING) must not
	// duplicate rows or error, even with a different resolved value —
	// the existing row wins, confirming idempotency rather than upsert
	// semantics.
	if err := feedback.InsertCorrectionConcepts(ctx, correctionID, slugs, resolved); err != nil {
		t.Fatalf("re-insert of the same pairs returned an error, want idempotent no-op: %v", err)
	}
	rowsAgain, err := selectCorrectionConcepts(ctx, pool, correctionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsAgain) != 2 {
		t.Fatalf("after re-insert, correction_concepts rows = %d, want still 2 (idempotent)", len(rowsAgain))
	}
}

type correctionConceptRow struct {
	slug     string
	resolved bool
}

func selectCorrectionConcepts(ctx context.Context, pool *pgxpool.Pool, correctionID string) ([]correctionConceptRow, error) {
	rows, err := pool.Query(ctx, `SELECT concept_slug, resolved FROM correction_concepts WHERE correction_id = $1`, correctionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []correctionConceptRow
	for rows.Next() {
		var r correctionConceptRow
		if err := rows.Scan(&r.slug, &r.resolved); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
