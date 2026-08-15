//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func TestGrammarUpsertConceptsIdempotentAndGetList(t *testing.T) {
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

	repo := NewGrammarRepository(pool)
	slug := "test-concept-" + uuid.NewString()

	c := grammar.Concept{
		Slug:          slug,
		Name:          "original name",
		JLPTLevel:     5,
		Description:   "original description",
		Examples:      []string{"例文一。", "例文二。"},
		Related:       []string{"te-form"},
		Prerequisites: []string{},
	}
	if err := repo.UpsertConcepts(ctx, []grammar.Concept{c}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.GetConcept(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "original name" || got.JLPTLevel != 5 {
		t.Fatalf("got %+v", got)
	}
	if len(got.Examples) != 2 || len(got.Related) != 1 {
		t.Fatalf("got %+v", got)
	}

	// Upsert again with the SAME slug but different fields: ON CONFLICT
	// DO UPDATE must replace every field, and no second row must be
	// created — this is upsert idempotency, not insert-only.
	c.Name = "updated name"
	c.JLPTLevel = 3
	c.Description = "updated description"
	c.Examples = []string{"新しい例文。"}
	if err := repo.UpsertConcepts(ctx, []grammar.Concept{c}); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetConcept(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "updated name" || got.JLPTLevel != 3 || got.Description != "updated description" {
		t.Fatalf("after re-upsert, got %+v, want updated fields", got)
	}
	if len(got.Examples) != 1 || got.Examples[0] != "新しい例文。" {
		t.Fatalf("after re-upsert, Examples = %v, want [新しい例文。]", got.Examples)
	}

	all, err := repo.ListConcepts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, lc := range all {
		if lc.Slug == slug {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ListConcepts contains %d rows for slug %q, want exactly 1 (upsert must not duplicate)", count, slug)
	}
}

func TestGrammarGetConceptNotFound(t *testing.T) {
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

	repo := NewGrammarRepository(pool)
	if _, err := repo.GetConcept(ctx, "no-such-slug-"+uuid.NewString()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestGrammarConceptStatsCrossIdentityIsolation is the contract-by-test
// case the brief calls out: a concept tagged only by ANOTHER identity's
// corrections must still appear for the caller, with Encounters=0 — not
// be silently omitted. See db/queries/grammar.sql's ConceptStats query
// comment for why a straight LEFT JOIN + WHERE fails this (it drops the
// grammar_concepts row entirely instead of zeroing it).
func TestGrammarConceptStatsCrossIdentityIsolation(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-grammar-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-grammar-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	grammarRepo := NewGrammarRepository(pool)
	slug := "test-concept-" + uuid.NewString()
	if err := grammarRepo.UpsertConcepts(ctx, []grammar.Concept{{
		Slug: slug, Name: "isolation test concept", JLPTLevel: 4, Description: "d",
		Examples: []string{"a", "b"},
	}}); err != nil {
		t.Fatal(err)
	}

	// identity B writes a session/document/feedback/correction, then
	// gets that correction tagged with slug.
	sessions := NewSessionRepository(pool)
	docs := NewDocumentRepository(pool)
	feedback := NewFeedbackRepository(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	sessB := session.Session{ID: session.ID(uuid.New().String()), IdentityID: identityB.ID, Title: "B", Purpose: "Diary", CreatedAt: now, UpdatedAt: now}
	if err := sessions.Create(ctx, sessB); err != nil {
		t.Fatal(err)
	}
	docB, _, err := docs.GetOrCreateForSession(ctx, identityB.ID, sessB.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedbackBID := uuid.New().String()
	correctionBID := uuid.New().String()
	recB := storage.FeedbackRecord{
		ID: feedbackBID, IdentityID: identityB.ID, SessionID: sessB.ID, DocumentID: docB.ID,
		SelectionStart: 0, SelectionEnd: 1, SelectionText: "a", CorrectedText: "b",
	}
	corrsB := []storage.CorrectionRecord{
		{ID: correctionBID, FeedbackID: feedbackBID, Position: 0, Original: "a", Replacement: "b", Type: "conjugation", Severity: "incorrect", Status: "presented"},
	}
	if err := feedback.InsertFeedback(ctx, recB, corrsB, nil); err != nil {
		t.Fatal(err)
	}

	// Tag the correction with slug. Tagging isn't part of this task's
	// repository surface (Phase 2 Task 2 adds it), so this test writes
	// the correction_concepts row directly.
	if _, err := pool.Exec(ctx, `INSERT INTO correction_concepts (correction_id, concept_slug, resolved) VALUES ($1, $2, true)`, correctionBID, slug); err != nil {
		t.Fatal(err)
	}

	// identity A never tagged this concept. It must still appear, with
	// Encounters=0 and LastSeen zero — not be missing from the list.
	statsA, err := grammarRepo.ConceptStats(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundA := findStat(statsA, slug)
	if foundA == nil {
		t.Fatalf("ConceptStats(identityA) does not contain slug %q at all — it must appear with Encounters=0, not be omitted", slug)
	}
	if foundA.Encounters != 0 {
		t.Fatalf("ConceptStats(identityA)[%q].Encounters = %d, want 0 (identity B's tag must not leak)", slug, foundA.Encounters)
	}
	if !foundA.LastSeen.IsZero() {
		t.Fatalf("ConceptStats(identityA)[%q].LastSeen = %v, want zero", slug, foundA.LastSeen)
	}

	// identity B tagged this concept once: Encounters=1, LastSeen set.
	statsB, err := grammarRepo.ConceptStats(ctx, identityB.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundB := findStat(statsB, slug)
	if foundB == nil {
		t.Fatalf("ConceptStats(identityB) does not contain slug %q", slug)
	}
	if foundB.Encounters != 1 {
		t.Fatalf("ConceptStats(identityB)[%q].Encounters = %d, want 1", slug, foundB.Encounters)
	}
	if foundB.LastSeen.IsZero() {
		t.Fatalf("ConceptStats(identityB)[%q].LastSeen is zero, want set", slug)
	}

	// CorrectionsForConcept must also be identity-scoped: identity A
	// sees none, identity B sees the one correction.
	corrsForA, err := grammarRepo.CorrectionsForConcept(ctx, identityA.ID, slug, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrsForA) != 0 {
		t.Fatalf("CorrectionsForConcept(identityA, %q) = %d rows, want 0", slug, len(corrsForA))
	}

	corrsForB, err := grammarRepo.CorrectionsForConcept(ctx, identityB.ID, slug, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrsForB) != 1 {
		t.Fatalf("CorrectionsForConcept(identityB, %q) = %d rows, want 1", slug, len(corrsForB))
	}
	if corrsForB[0].ID != correctionBID {
		t.Fatalf("CorrectionsForConcept(identityB, %q)[0].ID = %q, want %q", slug, corrsForB[0].ID, correctionBID)
	}
	if corrsForB[0].SessionID != sessB.ID {
		t.Fatalf("CorrectionsForConcept(identityB, %q)[0].SessionID = %q, want %q", slug, corrsForB[0].SessionID, sessB.ID)
	}
}

// TestGrammarCorrectionsForConceptExcludesGatedSocratic pins a
// code-review finding: a still-gated socratic correction (Status
// "presented", not yet Revealed, carrying a hint) must be EXCLUDED
// entirely from CorrectionsForConcept, not merely have its Replacement
// blanked by the caller — a gated correction shouldn't advertise its
// existence on the /grammar/{slug} concept detail page either. See
// db/queries/grammar.sql's CorrectionsForConcept WHERE NOT (...)
// clause, which mirrors the exact isGatedCorrection predicate
// internal/adapters/http/api.go already uses for the JSON API
// (HasHint() && Status=="presented" && !Revealed). Once
// RevealCorrection flips Revealed to true, the correction must appear
// like any other.
func TestGrammarCorrectionsForConceptExcludesGatedSocratic(t *testing.T) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-grammar-gate-" + uuid.NewString()), DisplayName: "Gate"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}

	grammarRepo := NewGrammarRepository(pool)
	slug := "test-concept-gate-" + uuid.NewString()
	if err := grammarRepo.UpsertConcepts(ctx, []grammar.Concept{{
		Slug: slug, Name: "gate test concept", JLPTLevel: 4, Description: "d",
		Examples: []string{"a", "b"},
	}}); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	docs := NewDocumentRepository(pool)
	feedbackRepo := NewFeedbackRepository(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := session.Session{ID: session.ID(uuid.New().String()), IdentityID: identity.ID, Title: "Gate", Purpose: "Diary", CreatedAt: now, UpdatedAt: now}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	doc, _, err := docs.GetOrCreateForSession(ctx, identity.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	feedbackID := uuid.New().String()
	gatedID := uuid.New().String()
	resolvedID := uuid.New().String()
	rec := storage.FeedbackRecord{
		ID: feedbackID, IdentityID: identity.ID, SessionID: sess.ID, DocumentID: doc.ID,
		SelectionStart: 0, SelectionEnd: 1, SelectionText: "a", CorrectedText: "b",
	}
	// gatedID: Status "presented" + a hint + (default) Revealed=false —
	// the exact socratic pre-reveal shape. resolvedID: Status "accepted",
	// no hint — an ordinary already-resolved correction, tagged to the
	// same concept, that must always be visible.
	corrs := []storage.CorrectionRecord{
		{ID: gatedID, FeedbackID: feedbackID, Position: 0, Original: "面白いでした", Replacement: "面白かったです", Type: "conjugation", Severity: "incorrect", Status: "presented", HintJA: "ヒント"},
		{ID: resolvedID, FeedbackID: feedbackID, Position: 1, Original: "c", Replacement: "d", Type: "conjugation", Severity: "incorrect", Status: "accepted"},
	}
	if err := feedbackRepo.InsertFeedback(ctx, rec, corrs, nil); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{gatedID, resolvedID} {
		if _, err := pool.Exec(ctx, `INSERT INTO correction_concepts (correction_id, concept_slug, resolved) VALUES ($1, $2, true)`, id, slug); err != nil {
			t.Fatal(err)
		}
	}

	got, err := grammarRepo.CorrectionsForConcept(ctx, identity.ID, slug, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != resolvedID {
		t.Fatalf("CorrectionsForConcept before reveal = %+v, want exactly one row (the resolved correction %q) — the gated correction must not appear", got, resolvedID)
	}

	if _, err := feedbackRepo.RevealCorrection(ctx, identity.ID, gatedID); err != nil {
		t.Fatal(err)
	}

	gotAfter, err := grammarRepo.CorrectionsForConcept(ctx, identity.ID, slug, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAfter) != 2 {
		t.Fatalf("CorrectionsForConcept after RevealCorrection = %d rows, want 2 (the formerly-gated correction must now appear)", len(gotAfter))
	}
	foundGated := false
	for _, c := range gotAfter {
		if c.ID == gatedID {
			foundGated = true
			if c.Replacement != "面白かったです" {
				t.Fatalf("revealed correction Replacement = %q, want 面白かったです", c.Replacement)
			}
		}
	}
	if !foundGated {
		t.Fatalf("CorrectionsForConcept after reveal = %+v, missing gated correction %q", gotAfter, gatedID)
	}
}

func findStat(stats []storage.ConceptStat, slug string) *storage.ConceptStat {
	for i := range stats {
		if stats[i].Slug == slug {
			return &stats[i]
		}
	}
	return nil
}
