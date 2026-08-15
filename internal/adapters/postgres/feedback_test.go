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

	if err := feedback.InsertFeedback(ctx, rec, corrections, nil); err != nil {
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

// TestFeedbackGetCorrection pins the Phase 3 Task 3 addition:
// GetCorrection reads a correction back identity-scoped, same join as
// UpdateCorrectionStatus — application/anki.Service.GenerateFromCorrection
// relies on this to fetch a correction's Original/Replacement/
// Explanation for the Anki agent to write a card from.
func TestFeedbackGetCorrection(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-getcorrection-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-getcorrection-b-" + uuid.NewString()), DisplayName: "B"}
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
	correctionID := uuid.New().String()
	rec := storage.FeedbackRecord{
		ID:             feedbackID,
		IdentityID:     identityA.ID,
		SessionID:      sess.ID,
		DocumentID:     doc.ID,
		SelectionStart: 0,
		SelectionEnd:   9,
		SelectionText:  "とても面白いでした",
		CorrectedText:  "とても面白かったです",
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
		Status:        "accepted",
	}}
	if err := feedback.InsertFeedback(ctx, rec, corrections, nil); err != nil {
		t.Fatal(err)
	}

	got, err := feedback.GetCorrection(ctx, identityA.ID, correctionID)
	if err != nil {
		t.Fatalf("GetCorrection: %v", err)
	}
	if got.Original != "面白いでした" || got.Replacement != "面白かったです" {
		t.Fatalf("Original/Replacement = %q/%q, want 面白いでした/面白かったです", got.Original, got.Replacement)
	}
	if got.ExplanationJA == "" || got.ExplanationEN == "" {
		t.Fatal("explanation fields were not returned")
	}
	if got.SessionID != sess.ID {
		t.Fatalf("SessionID = %q, want %q", got.SessionID, sess.ID)
	}

	if _, err := feedback.GetCorrection(ctx, identityB.ID, correctionID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity GetCorrection err = %v, want storage.ErrNotFound", err)
	}
	if _, err := feedback.GetCorrection(ctx, identityA.ID, uuid.NewString()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unknown correction id err = %v, want storage.ErrNotFound", err)
	}
}

// TestFeedbackInsertFeedbackPersistsConceptsAtomicallyAndIdempotently
// pins Phase 2 Task 2's concept-tagging persistence, now folded into
// InsertFeedback's single transaction (fix round 3, Finding 2) rather
// than a separate InsertCorrectionConcepts call: passing concepts
// alongside rec/corrections in ONE InsertFeedback call writes one
// correction_concepts row per (correction, slug) pair — resolved=true
// for a known slug, resolved=false for an unknown one — and a
// duplicate (correction, slug) pair WITHIN that same concepts argument
// is a no-op (ON CONFLICT DO NOTHING), not a duplicate-row error or a
// failure of the whole call.
func TestFeedbackInsertFeedbackPersistsConceptsAtomicallyAndIdempotently(t *testing.T) {
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
	concepts := map[string][]storage.ConceptTag{
		correctionID: {
			{Slug: "i-adjective-past", Resolved: true},
			{Slug: "no-such-slug", Resolved: false},
			// Deliberately duplicated: proves ON CONFLICT DO NOTHING
			// swallows a repeated (correction, slug) pair even within
			// the SAME InsertFeedback call, rather than erroring the
			// whole transaction out.
			{Slug: "i-adjective-past", Resolved: true},
		},
	}
	if err := feedback.InsertFeedback(ctx, rec, corrections, concepts); err != nil {
		t.Fatal(err)
	}

	rows, err := selectCorrectionConcepts(ctx, pool, correctionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("correction_concepts rows = %d, want 2 (duplicate collapsed): %+v", len(rows), rows)
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
}

// TestFeedbackInsertFeedbackFailureRollsBackAlreadyPersistedConcepts
// pins fix round 3's Finding 2 at the database level: concept tags are
// no longer written in a separate transaction after
// feedback_requests/corrections commit — they're part of the SAME
// InsertFeedback transaction. A concepts row can no longer independently
// violate a constraint by design (correction_id is always derived from
// the correction actually being inserted right then, so its FK can
// never dangle, and a duplicate (correction, slug) pair within one call
// is swallowed by ON CONFLICT DO NOTHING rather than erroring) — which
// is itself proof the fix closed the gap it targeted. So this test
// forces a real, later failure in the SAME transaction (a second
// correction reusing the first correction's ID, a corrections PK
// violation) and asserts that the FIRST correction's already-inserted
// concept row — which would have been happily committed under the OLD,
// separate-transaction design — is rolled back along with everything
// else: no feedback_requests row, no corrections row, no
// correction_concepts row survives.
func TestFeedbackInsertFeedbackFailureRollsBackAlreadyPersistedConcepts(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-feedback-rollback-" + uuid.NewString()), DisplayName: "A"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identityA.ID,
		Title:      "ロールバックテスト",
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
	corrections := []storage.CorrectionRecord{
		{
			ID:          correctionID,
			FeedbackID:  feedbackID,
			Position:    0,
			Original:    "面白いでした",
			Replacement: "面白かったです",
			Type:        "conjugation",
			Severity:    "incorrect",
			Status:      "presented",
		},
		{
			// Reuses correctionID: corrections.id is a primary key, so
			// this second InsertCorrection call fails with a PK
			// violation AFTER the first correction (and its concept
			// row, inserted between the two InsertCorrection calls)
			// already succeeded within this same, still-uncommitted
			// transaction.
			ID:          correctionID,
			FeedbackID:  feedbackID,
			Position:    1,
			Original:    "とても",
			Replacement: "非常に",
			Type:        "style",
			Severity:    "optional",
			Status:      "presented",
		},
	}
	concepts := map[string][]storage.ConceptTag{
		correctionID: {{Slug: "i-adjective-past", Resolved: true}},
	}

	if err := feedback.InsertFeedback(ctx, rec, corrections, concepts); err == nil {
		t.Fatal("expected an error from the duplicate correction ID PK violation, got nil")
	}

	var feedbackCount, correctionCount, conceptCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM feedback_requests WHERE id = $1`, feedbackID).Scan(&feedbackCount); err != nil {
		t.Fatal(err)
	}
	if feedbackCount != 0 {
		t.Fatalf("feedback_requests row count = %d, want 0 (the whole transaction must have rolled back)", feedbackCount)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM corrections WHERE id = $1`, correctionID).Scan(&correctionCount); err != nil {
		t.Fatal(err)
	}
	if correctionCount != 0 {
		t.Fatalf("corrections row count = %d, want 0 (the whole transaction must have rolled back)", correctionCount)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM correction_concepts WHERE correction_id = $1`, correctionID).Scan(&conceptCount); err != nil {
		t.Fatal(err)
	}
	if conceptCount != 0 {
		t.Fatalf("correction_concepts row count = %d, want 0 (the first correction's already-inserted concept row must have rolled back too, not been left committed under the old separate-transaction design)", conceptCount)
	}
}

// TestFeedbackGetCorrectionConceptsReturnsResolvedOnlySlugAscending
// pins the code-review fix backing SetCorrectionStatus's concept-chip
// carry-through: GetCorrectionConcepts must return ONLY resolved slugs
// (an unresolved tag — recorded but not a real catalog concept — must
// never surface as something a re-rendered card can link to
// /grammar/{slug}), in slug-ascending order regardless of insertion
// order (correction_concepts has no created_at to order by instead).
func TestFeedbackGetCorrectionConceptsReturnsResolvedOnlySlugAscending(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-feedback-getconcepts-" + uuid.NewString()), DisplayName: "A"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identityA.ID,
		Title:      "文法タグ取得テスト",
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
		ID:          correctionID,
		FeedbackID:  feedbackID,
		Position:    0,
		Original:    "面白いでした",
		Replacement: "面白かったです",
		Type:        "conjugation",
		Severity:    "incorrect",
		Status:      "presented",
	}}
	// Inserted in reverse-alphabetical order deliberately, plus one
	// unresolved slug, so the assertion below can't pass by accident of
	// insertion order.
	concepts := map[string][]storage.ConceptTag{
		correctionID: {
			{Slug: "te-form", Resolved: true},
			{Slug: "i-adjective-past", Resolved: true},
			{Slug: "no-such-slug", Resolved: false},
		},
	}
	if err := feedback.InsertFeedback(ctx, rec, corrections, concepts); err != nil {
		t.Fatal(err)
	}

	got, err := feedback.GetCorrectionConcepts(ctx, correctionID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"i-adjective-past", "te-form"}
	if len(got) != len(want) {
		t.Fatalf("GetCorrectionConcepts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetCorrectionConcepts = %v, want %v", got, want)
		}
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

// --- Phase 2 Task 8: active recall (hints, retry, reveal) + confidence
// tracking (PRD §9/§53). ---

// activeRecallFixture wires the FK chain (identities -> session ->
// document -> feedback_requests -> corrections) up through ONE
// "presented", hint-bearing correction, ready for
// RetryCorrection/RevealCorrection/RecordConfidence tests — the setup
// every test below needs, factored out since Task 8 adds several of
// them against the identical shape.
type activeRecallFixture struct {
	repo         *FeedbackRepository
	identityA    learner.Identity
	identityB    learner.Identity // a second, unrelated identity for cross-identity checks
	session      session.Session
	correctionID string
	replacement  string
}

func setupActiveRecallFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) activeRecallFixture {
	t.Helper()

	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-recall-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-recall-b-" + uuid.NewString()), DisplayName: "B"}
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
		Title:      "ソクラテス式テスト",
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
		SelectionEnd:   9,
		SelectionText:  "とても面白いでした",
		CorrectedText:  "とても面白かったです",
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
		HintJA:        "い形容詞の過去形の作り方を思い出してください。",
		HintEN:        "Recall how い-adjectives form the past tense.",
		Status:        "presented",
	}}
	if err := feedback.InsertFeedback(ctx, rec, corrections, nil); err != nil {
		t.Fatal(err)
	}

	return activeRecallFixture{
		repo:         feedback,
		identityA:    identityA,
		identityB:    identityB,
		session:      sess,
		correctionID: correctionID,
		replacement:  "面白かったです",
	}
}

// TestFeedbackInsertCorrectionPersistsHintColumns pins that
// InsertFeedback actually writes hint_ja/hint_en through to the DB
// (not just the request DTO in memory) — the fixture's hint round-trips
// back via UpdateCorrectionStatus's RETURNING columns, and Attempts/
// Confidence/Revealed all start at their migration 00012 defaults
// (0/nil/false).
func TestFeedbackInsertCorrectionPersistsHintColumns(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	// UpdateCorrectionStatus with the correction's OWN current status
	// ("presented" -> "presented") is a no-op write that still returns
	// the full row, letting this test read back every Task 8 column
	// without needing a raw SQL SELECT of its own.
	row, err := fx.repo.UpdateCorrectionStatus(ctx, fx.identityA.ID, fx.correctionID, "presented")
	if err != nil {
		t.Fatal(err)
	}
	wantJA := "い形容詞の過去形の作り方を思い出してください。"
	wantEN := "Recall how い-adjectives form the past tense."
	if row.HintJA != wantJA || row.HintEN != wantEN {
		t.Fatalf("HintJA/HintEN = %q/%q, want %q/%q", row.HintJA, row.HintEN, wantJA, wantEN)
	}
	if row.Attempts != 0 {
		t.Fatalf("Attempts = %d, want 0", row.Attempts)
	}
	if row.Confidence != nil {
		t.Fatalf("Confidence = %v, want nil", row.Confidence)
	}
	if row.Revealed {
		t.Fatal("Revealed = true, want false")
	}
}

// TestFeedbackRetryCorrectionCorrectAcceptsAndIncrementsAttempts pins
// the single-UPDATE "increment attempts, accept iff exact match"
// contract against a real database — including that a plain accept
// (via UpdateCorrectionStatus) is NOT what flips it; RetryCorrection's
// own comparison is.
func TestFeedbackRetryCorrectionCorrectAcceptsAndIncrementsAttempts(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	row, err := fx.repo.RetryCorrection(ctx, fx.identityA.ID, fx.correctionID, fx.replacement)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "accepted" {
		t.Fatalf("Status = %q, want accepted", row.Status)
	}
	if row.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", row.Attempts)
	}
	if row.Revealed {
		t.Fatal("Revealed = true, want unchanged false")
	}
	if row.SessionID != fx.session.ID {
		t.Fatalf("SessionID = %q, want %q", row.SessionID, fx.session.ID)
	}
}

// TestFeedbackRetryCorrectionWrongAttemptIncrementsOnly pins the
// "wrong attempt" half at the DB level, across two consecutive wrong
// attempts to prove attempts actually accumulates rather than resetting.
func TestFeedbackRetryCorrectionWrongAttemptIncrementsOnly(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	row, err := fx.repo.RetryCorrection(ctx, fx.identityA.ID, fx.correctionID, "面白いです")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "presented" {
		t.Fatalf("Status = %q, want unchanged presented", row.Status)
	}
	if row.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", row.Attempts)
	}

	row2, err := fx.repo.RetryCorrection(ctx, fx.identityA.ID, fx.correctionID, "違います")
	if err != nil {
		t.Fatal(err)
	}
	if row2.Status != "presented" {
		t.Fatalf("Status (2nd) = %q, want unchanged presented", row2.Status)
	}
	if row2.Attempts != 2 {
		t.Fatalf("Attempts (2nd) = %d, want 2", row2.Attempts)
	}
}

// TestFeedbackRetryCorrectionCrossIdentityReturnsErrNotFound.
func TestFeedbackRetryCorrectionCrossIdentityReturnsErrNotFound(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	if _, err := fx.repo.RetryCorrection(ctx, fx.identityB.ID, fx.correctionID, fx.replacement); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
	// Must not have mutated the row: a subsequent retry from the RIGHT
	// identity should still see Attempts=0, not 1.
	row, err := fx.repo.RetryCorrection(ctx, fx.identityA.ID, fx.correctionID, fx.replacement)
	if err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1 (the cross-identity attempt must not have incremented it)", row.Attempts)
	}
}

// TestFeedbackRetryCorrectionOnNonPresentedReturnsErrNotFound: once a
// correction has left Status "presented" (here via a plain
// UpdateCorrectionStatus accept, exercising the OTHER path to a
// resolved correction besides RetryCorrection itself), a further retry
// misses — the WHERE clause's status = 'presented' excludes it.
func TestFeedbackRetryCorrectionOnNonPresentedReturnsErrNotFound(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	if _, err := fx.repo.UpdateCorrectionStatus(ctx, fx.identityA.ID, fx.correctionID, "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.repo.RetryCorrection(ctx, fx.identityA.ID, fx.correctionID, fx.replacement); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestFeedbackRevealCorrectionSetsRevealedTrue pins RevealCorrection's
// DB-level contract: Revealed flips true, Status is left alone, and a
// second reveal call is a harmless idempotent no-op (not an error).
func TestFeedbackRevealCorrectionSetsRevealedTrue(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	row, err := fx.repo.RevealCorrection(ctx, fx.identityA.ID, fx.correctionID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Revealed {
		t.Fatal("Revealed = false, want true")
	}
	if row.Status != "presented" {
		t.Fatalf("Status = %q, want unchanged presented", row.Status)
	}

	// Idempotent: revealing again succeeds and stays revealed.
	row2, err := fx.repo.RevealCorrection(ctx, fx.identityA.ID, fx.correctionID)
	if err != nil {
		t.Fatalf("second RevealCorrection returned error: %v", err)
	}
	if !row2.Revealed {
		t.Fatal("Revealed (2nd call) = false, want true")
	}
}

// TestFeedbackRevealCorrectionCrossIdentityReturnsErrNotFound.
func TestFeedbackRevealCorrectionCrossIdentityReturnsErrNotFound(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	if _, err := fx.repo.RevealCorrection(ctx, fx.identityB.ID, fx.correctionID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestFeedbackRecordConfidencePersistsValue pins RecordConfidence's
// DB-level contract, INCLUDING that it is NOT restricted to Status
// "presented" (accepted first, via RetryCorrection, then rated).
func TestFeedbackRecordConfidencePersistsValue(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	if _, err := fx.repo.RetryCorrection(ctx, fx.identityA.ID, fx.correctionID, fx.replacement); err != nil {
		t.Fatal(err)
	}

	row, err := fx.repo.RecordConfidence(ctx, fx.identityA.ID, fx.correctionID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if row.Confidence == nil || *row.Confidence != 4 {
		t.Fatalf("Confidence = %v, want pointer to 4", row.Confidence)
	}
	if row.Status != "accepted" {
		t.Fatalf("Status = %q, want unchanged accepted", row.Status)
	}

	// Re-rating overwrites the previous value.
	row2, err := fx.repo.RecordConfidence(ctx, fx.identityA.ID, fx.correctionID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if row2.Confidence == nil || *row2.Confidence != 2 {
		t.Fatalf("Confidence (2nd) = %v, want pointer to 2", row2.Confidence)
	}
}

// TestFeedbackRecordConfidenceCrossIdentityReturnsErrNotFound.
func TestFeedbackRecordConfidenceCrossIdentityReturnsErrNotFound(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	if _, err := fx.repo.RecordConfidence(ctx, fx.identityB.ID, fx.correctionID, 3); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestFeedbackCorrectionsConfidenceCheckConstraint pins migration
// 00012's CHECK constraint directly against raw SQL, bypassing the
// repository layer entirely (which never sends an out-of-range value —
// see application/feedback.Service.RecordConfidence's own 1..5
// validation): the DB itself must still refuse to store confidence
// outside 1..5, as defense in depth against any future write path that
// skips the service layer's guard.
func TestFeedbackCorrectionsConfidenceCheckConstraint(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	for _, bad := range []int{0, 6, -1} {
		_, err := pool.Exec(ctx, `UPDATE corrections SET confidence = $2 WHERE id = $1`, fx.correctionID, bad)
		if err == nil {
			t.Fatalf("confidence=%d: expected a CHECK constraint violation, got nil", bad)
		}
	}
	// NULL and the boundary values 1/5 remain valid.
	for _, ok := range []int{1, 5} {
		if _, err := pool.Exec(ctx, `UPDATE corrections SET confidence = $2 WHERE id = $1`, fx.correctionID, ok); err != nil {
			t.Fatalf("confidence=%d: unexpected error: %v", ok, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE corrections SET confidence = NULL WHERE id = $1`, fx.correctionID); err != nil {
		t.Fatalf("confidence=NULL: unexpected error: %v", err)
	}
}

// TestFeedbackRecentCorrections pins the Phase 3 Task 4 RecentCorrections
// contract: identity-scoped (identityB sees nothing), unfiltered by
// status, and respects limit.
func TestFeedbackRecentCorrections(t *testing.T) {
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

	fx := setupActiveRecallFixture(t, ctx, pool)

	got, err := fx.repo.RecentCorrections(ctx, fx.identityA.ID, 10)
	if err != nil {
		t.Fatalf("RecentCorrections: %v", err)
	}
	if len(got) != 1 || got[0].ID != fx.correctionID {
		t.Fatalf("RecentCorrections(identityA) = %+v, want exactly [%s]", got, fx.correctionID)
	}
	if got[0].Original != "面白いでした" || got[0].Replacement != fx.replacement {
		t.Fatalf("got[0] = %+v, want Original/Replacement matching the fixture", got[0])
	}

	// identityB never sees identityA's correction.
	gotB, err := fx.repo.RecentCorrections(ctx, fx.identityB.ID, 10)
	if err != nil {
		t.Fatalf("RecentCorrections(identityB): %v", err)
	}
	if len(gotB) != 0 {
		t.Fatalf("RecentCorrections(identityB) = %+v, want empty", gotB)
	}

	// limit=0 returns nothing (SQL LIMIT 0), not "unlimited" — matching
	// the plain LIMIT $2 the query issues.
	gotLimited, err := fx.repo.RecentCorrections(ctx, fx.identityA.ID, 0)
	if err != nil {
		t.Fatalf("RecentCorrections(limit=0): %v", err)
	}
	if len(gotLimited) != 0 {
		t.Fatalf("RecentCorrections(limit=0) = %+v, want empty", gotLimited)
	}
}
