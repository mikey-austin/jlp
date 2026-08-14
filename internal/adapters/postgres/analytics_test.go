//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func TestAnalyticsStatisticsScopedToIdentity(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-analytics-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-analytics-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	docs := NewDocumentRepository(pool)
	feedback := NewFeedbackRepository(pool)

	// --- identity A: two sessions, one document written to (10 runes),
	// one feedback request with 2 corrections: 1 accepted, 1 presented
	// (never resolved). TopErrorTypes should show "conjugation" (2x)
	// ahead of "style" (0x, since style isn't used here) — just
	// "conjugation" present at all is enough to assert against.
	now := time.Now().UTC().Truncate(time.Microsecond)
	sessA1 := session.Session{ID: session.ID(uuid.New().String()), IdentityID: identityA.ID, Title: "A1", Purpose: "Diary", CreatedAt: now, UpdatedAt: now}
	sessA2 := session.Session{ID: session.ID(uuid.New().String()), IdentityID: identityA.ID, Title: "A2", Purpose: "Diary", CreatedAt: now, UpdatedAt: now}
	if err := sessions.Create(ctx, sessA1); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Create(ctx, sessA2); err != nil {
		t.Fatal(err)
	}

	docA, _, err := docs.GetOrCreateForSession(ctx, identityA.ID, sessA1.ID)
	if err != nil {
		t.Fatal(err)
	}
	content := "0123456789" // 10 ASCII runes
	if _, err := docs.Save(ctx, identityA.ID, docA.ID, content); err != nil {
		t.Fatal(err)
	}

	feedbackAID := uuid.New().String()
	corrAcceptedID := uuid.New().String()
	corrPresentedID := uuid.New().String()
	recA := storage.FeedbackRecord{
		ID: feedbackAID, IdentityID: identityA.ID, SessionID: sessA1.ID, DocumentID: docA.ID,
		SelectionStart: 0, SelectionEnd: 10, SelectionText: content, CorrectedText: content,
	}
	corrsA := []storage.CorrectionRecord{
		{ID: corrAcceptedID, FeedbackID: feedbackAID, Position: 0, Original: "a", Replacement: "b", Type: "conjugation", Severity: "incorrect", Status: "presented"},
		{ID: corrPresentedID, FeedbackID: feedbackAID, Position: 1, Original: "c", Replacement: "d", Type: "conjugation", Severity: "unnatural", Status: "presented"},
	}
	if err := feedback.InsertFeedback(ctx, recA, corrsA, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := feedback.UpdateCorrectionStatus(ctx, identityA.ID, corrAcceptedID, "accepted"); err != nil {
		t.Fatal(err)
	}

	// --- identity B: a second identity's worth of rows, larger in every
	// dimension, so identity A's stats can be shown to exclude them.
	sessB := session.Session{ID: session.ID(uuid.New().String()), IdentityID: identityB.ID, Title: "B1", Purpose: "Diary", CreatedAt: now, UpdatedAt: now}
	if err := sessions.Create(ctx, sessB); err != nil {
		t.Fatal(err)
	}
	docB, _, err := docs.GetOrCreateForSession(ctx, identityB.ID, sessB.ID)
	if err != nil {
		t.Fatal(err)
	}
	bContent := "01234567890123456789" // 20 runes
	if _, err := docs.Save(ctx, identityB.ID, docB.ID, bContent); err != nil {
		t.Fatal(err)
	}
	feedbackBID := uuid.New().String()
	corrB1, corrB2, corrB3 := uuid.New().String(), uuid.New().String(), uuid.New().String()
	recB := storage.FeedbackRecord{
		ID: feedbackBID, IdentityID: identityB.ID, SessionID: sessB.ID, DocumentID: docB.ID,
		SelectionStart: 0, SelectionEnd: 20, SelectionText: bContent, CorrectedText: bContent,
	}
	corrsB := []storage.CorrectionRecord{
		{ID: corrB1, FeedbackID: feedbackBID, Position: 0, Original: "a", Replacement: "b", Type: "particle", Severity: "incorrect", Status: "presented"},
		{ID: corrB2, FeedbackID: feedbackBID, Position: 1, Original: "c", Replacement: "d", Type: "particle", Severity: "incorrect", Status: "presented"},
		{ID: corrB3, FeedbackID: feedbackBID, Position: 2, Original: "e", Replacement: "f", Type: "particle", Severity: "incorrect", Status: "presented"},
	}
	if err := feedback.InsertFeedback(ctx, recB, corrsB, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := feedback.UpdateCorrectionStatus(ctx, identityB.ID, corrB1, "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := feedback.UpdateCorrectionStatus(ctx, identityB.ID, corrB2, "rejected"); err != nil {
		t.Fatal(err)
	}

	repo := NewAnalyticsRepository(pool)

	statsA, err := repo.Statistics(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statsA.RunesWritten != 10 {
		t.Fatalf("A RunesWritten = %d, want 10 (must exclude identity B's 20)", statsA.RunesWritten)
	}
	if statsA.SessionCount != 2 {
		t.Fatalf("A SessionCount = %d, want 2 (must exclude identity B's 1)", statsA.SessionCount)
	}
	if statsA.FeedbackRequests != 1 {
		t.Fatalf("A FeedbackRequests = %d, want 1 (must exclude identity B's 1)", statsA.FeedbackRequests)
	}
	if statsA.CorrectionsPresented != 2 {
		t.Fatalf("A CorrectionsPresented = %d, want 2 (all corrections regardless of status; must exclude identity B's 3)", statsA.CorrectionsPresented)
	}
	if statsA.CorrectionsAccepted != 1 {
		t.Fatalf("A CorrectionsAccepted = %d, want 1", statsA.CorrectionsAccepted)
	}
	if statsA.CorrectionsRejected != 0 {
		t.Fatalf("A CorrectionsRejected = %d, want 0 (must exclude identity B's 2 rejected)", statsA.CorrectionsRejected)
	}
	// Ratios are left zeroed by the repository — the application
	// service computes them, not the repo.
	if statsA.AcceptanceRate != 0 {
		t.Fatalf("A AcceptanceRate = %v, want 0 (repository must not compute derived ratios)", statsA.AcceptanceRate)
	}
	if statsA.CorrectionsPer1000 != 0 {
		t.Fatalf("A CorrectionsPer1000 = %v, want 0 (repository must not compute derived ratios)", statsA.CorrectionsPer1000)
	}
	if len(statsA.TopErrorTypes) != 1 || statsA.TopErrorTypes[0].Type != "conjugation" || statsA.TopErrorTypes[0].Count != 2 {
		t.Fatalf("A TopErrorTypes = %+v, want [{conjugation 2}] (must exclude identity B's particle rows)", statsA.TopErrorTypes)
	}

	statsB, err := repo.Statistics(ctx, identityB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statsB.RunesWritten != 20 {
		t.Fatalf("B RunesWritten = %d, want 20", statsB.RunesWritten)
	}
	if statsB.CorrectionsPresented != 3 || statsB.CorrectionsRejected != 2 || statsB.CorrectionsAccepted != 0 {
		t.Fatalf("B correction counts = presented=%d accepted=%d rejected=%d, want 3/0/2", statsB.CorrectionsPresented, statsB.CorrectionsAccepted, statsB.CorrectionsRejected)
	}
	if len(statsB.TopErrorTypes) != 1 || statsB.TopErrorTypes[0].Type != "particle" || statsB.TopErrorTypes[0].Count != 3 {
		t.Fatalf("B TopErrorTypes = %+v, want [{particle 3}]", statsB.TopErrorTypes)
	}

	// An identity with no rows at all gets all-zero stats, not an error.
	identityC := learner.Identity{ID: learner.IdentityID("test-analytics-c-" + uuid.NewString()), DisplayName: "C"}
	if err := identities.Upsert(ctx, identityC); err != nil {
		t.Fatal(err)
	}
	statsC, err := repo.Statistics(ctx, identityC.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statsC.RunesWritten != 0 || statsC.SessionCount != 0 || statsC.FeedbackRequests != 0 || statsC.CorrectionsPresented != 0 {
		t.Fatalf("C stats = %+v, want all zero", statsC)
	}
	if len(statsC.TopErrorTypes) != 0 {
		t.Fatalf("C TopErrorTypes = %+v, want empty", statsC.TopErrorTypes)
	}
}
