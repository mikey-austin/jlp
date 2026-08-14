package planner_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const testIdentity = learner.IdentityID("learner-planner-test")

// --- fakes: same "in-memory, identity-scoped, panic on unused methods"
// shape as feedback/service_test.go and learnermodel/consumers_test.go. ---

type fakeObsRepo struct {
	byIdentity map[learner.IdentityID][]learnermodel.Observation
}

func (f *fakeObsRepo) Upsert(context.Context, learnermodel.Observation) error {
	panic("not used by planner tests")
}

func (f *fakeObsRepo) List(_ context.Context, identity learner.IdentityID) ([]learnermodel.Observation, error) {
	return f.byIdentity[identity], nil
}

func (f *fakeObsRepo) DeleteAll(context.Context, learner.IdentityID) error {
	panic("not used by planner tests")
}

type fakeEventStore struct {
	byIdentity map[learner.IdentityID][]event.LearningEvent
}

func (f *fakeEventStore) Append(context.Context, event.LearningEvent) error {
	panic("not used by planner tests")
}

func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	panic("not used by planner tests")
}

func (f *fakeEventStore) ListAll(_ context.Context, identity learner.IdentityID) ([]event.LearningEvent, error) {
	return f.byIdentity[identity], nil
}

type fakeGrammarRepo struct {
	concepts map[string]grammar.Concept
}

func (f *fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by planner tests")
}

func (f *fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	panic("not used by planner tests")
}

func (f *fakeGrammarRepo) GetConcept(_ context.Context, slug string) (grammar.Concept, error) {
	c, ok := f.concepts[slug]
	if !ok {
		return grammar.Concept{}, storage.ErrNotFound
	}
	return c, nil
}

func (f *fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by planner tests")
}

func (f *fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by planner tests")
}

type fakePriorityRepo struct {
	replaced   []storage.Priority
	replaceErr error
}

func (f *fakePriorityRepo) ReplaceAll(_ context.Context, _ learner.IdentityID, ps []storage.Priority) error {
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.replaced = ps
	return nil
}

func (f *fakePriorityRepo) Top(context.Context, learner.IdentityID, int) ([]storage.Priority, error) {
	panic("not used by planner tests")
}

// conceptEvent builds a grammar.concept.encountered event exactly as
// internal/application/feedback's pipeline records one: Subject is the
// concept slug, Evidence carries the originating correction_id (the
// dedup key) and severity.
func conceptEvent(id, slug, correctionID, severity string, at time.Time) event.LearningEvent {
	return event.LearningEvent{
		ID:         id,
		IdentityID: testIdentity,
		Type:       event.TypeGrammarConceptEncountered,
		Subject:    slug,
		Evidence:   map[string]any{"correction_id": correctionID, "severity": severity},
		OccurredAt: at,
	}
}

// correctionEvent builds a correction.presented event: Subject IS the
// correction ID (and thus the dedup key), Evidence carries the
// correction type (the subject a correction-type observation keys on)
// and severity.
func correctionEvent(correctionID, corrType, severity string, at time.Time) event.LearningEvent {
	return event.LearningEvent{
		ID:         "ev-" + correctionID,
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionPresented,
		Subject:    correctionID,
		Evidence:   map[string]any{"type": corrType, "severity": severity},
		OccurredAt: at,
	}
}

func newPlanner(obs *fakeObsRepo, events *fakeEventStore, grammarRepo *fakeGrammarRepo, prios *fakePriorityRepo, now time.Time) *planner.Planner {
	return planner.NewPlanner(obs, events, grammarRepo, prios, func() time.Time { return now })
}

// TestWeaknessScorePinnedExample pins the brief's exact worked example
// (PRD §16): a weakness on concept i-adjective-past (N5 => JLPT weight
// 2) with 5 occurrences/30d (from the observation's Evidence count) and
// 2 distinct correction_ids in the trailing 7 days =>
// persistence=min(5/3,3)=1.667, recency=2+1=3, value=1+0.25*2=1.5,
// score=1.667*3*1.5=7.5.
func TestWeaknessScorePinnedExample(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	obs := &fakeObsRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{
		testIdentity: {{
			ID: "obs-1", IdentityID: testIdentity, Kind: learnermodel.KindWeakness,
			SubjectType: learnermodel.SubjectConcept, Subject: "i-adjective-past",
			Evidence: map[string]any{"count": 5, "window_days": 30},
		}},
	}}
	events := &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{testIdentity: {
		conceptEvent("ev1", "i-adjective-past", "c1", "incorrect", now.Add(-40*24*time.Hour)),      // outside the 7d recency window
		conceptEvent("ev2", "i-adjective-past", "c2", "incorrect", now.Add(-6*24*time.Hour)),       // within 7d
		conceptEvent("ev2-retry", "i-adjective-past", "c2", "incorrect", now.Add(-5*24*time.Hour)), // duplicate correction_id — must not double-count
		conceptEvent("ev3", "i-adjective-past", "c3", "incorrect", now.Add(-1*time.Hour)),          // within 7d
	}}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{
		"i-adjective-past": {Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5},
	}}
	prios := &fakePriorityRepo{}

	p := newPlanner(obs, events, grammarRepo, prios, now)
	if err := p.Recompute(context.Background(), testIdentity); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	if len(prios.replaced) != 1 {
		t.Fatalf("ReplaceAll received %d rows, want 1: %+v", len(prios.replaced), prios.replaced)
	}
	got := prios.replaced[0]
	if diff := math.Abs(got.Score - 7.5); diff > 0.01 {
		t.Errorf("Score = %v, want 7.5 (+/-0.01)", got.Score)
	}
	if got.IdentityID != testIdentity {
		t.Errorf("IdentityID = %q, want %q", got.IdentityID, testIdentity)
	}
	if got.SubjectType != "concept" || got.Subject != "i-adjective-past" {
		t.Errorf("SubjectType/Subject = %s/%s, want concept/i-adjective-past", got.SubjectType, got.Subject)
	}
	for _, want := range []string{"5 occurrences", "2 in last 7d", "N5", "weight 2"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason = %q, missing %q", got.Reason, want)
		}
	}
}

// TestWeaknessOnCorrectionTypeUsesValueOne pins the other half of the
// value formula: a correction-type subject (not a grammar concept) has
// no JLPT level to look up, so value is always 1.0 regardless of the
// GrammarRepository's contents.
func TestWeaknessOnCorrectionTypeUsesValueOne(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	obs := &fakeObsRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{
		testIdentity: {{
			ID: "obs-2", IdentityID: testIdentity, Kind: learnermodel.KindWeakness,
			SubjectType: learnermodel.SubjectCorrectionType, Subject: "conjugation",
			Evidence: map[string]any{"count": 3, "window_days": 30},
		}},
	}}
	events := &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{}}
	prios := &fakePriorityRepo{}

	p := newPlanner(obs, events, grammarRepo, prios, now)
	if err := p.Recompute(context.Background(), testIdentity); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	got := prios.replaced[0]
	// persistence=min(3/3,3)=1, recency=0+1=1, value=1 => score=1.
	if diff := math.Abs(got.Score - 1.0); diff > 0.01 {
		t.Errorf("Score = %v, want 1.0 (+/-0.01)", got.Score)
	}
}

// TestWeaknessOnConceptNotInCatalogFallsBackToValueOne pins the brief's
// ErrNotFound handling: a concept slug the catalog no longer (or never)
// knows about must not fail Recompute — it falls back to the weight-0
// band (value 1.0), same as an N1/N2 concept would.
func TestWeaknessOnConceptNotInCatalogFallsBackToValueOne(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	obs := &fakeObsRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{
		testIdentity: {{
			ID: "obs-3", IdentityID: testIdentity, Kind: learnermodel.KindWeakness,
			SubjectType: learnermodel.SubjectConcept, Subject: "unknown-slug",
			Evidence: map[string]any{"count": 3, "window_days": 30},
		}},
	}}
	events := &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{}}
	prios := &fakePriorityRepo{}

	p := newPlanner(obs, events, grammarRepo, prios, now)
	if err := p.Recompute(context.Background(), testIdentity); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	got := prios.replaced[0]
	if diff := math.Abs(got.Score - 1.0); diff > 0.01 {
		t.Errorf("Score = %v, want 1.0 (+/-0.01)", got.Score)
	}
}

// TestEmergingScoreHalvesPersistenceOnly pins the emerging formula:
// score = 0.5 * persistence, with recency and value dropped entirely —
// even when there ARE recent qualifying events in the store, they must
// not affect an emerging observation's score.
func TestEmergingScoreHalvesPersistenceOnly(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	obs := &fakeObsRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{
		testIdentity: {{
			ID: "obs-4", IdentityID: testIdentity, Kind: learnermodel.KindEmerging,
			SubjectType: learnermodel.SubjectCorrectionType, Subject: "conjugation",
			Evidence: map[string]any{"count": 4, "window_days": 30},
		}},
	}}
	events := &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{testIdentity: {
		correctionEvent("c1", "conjugation", "incorrect", now.Add(-1*time.Hour)),
	}}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{}}
	prios := &fakePriorityRepo{}

	p := newPlanner(obs, events, grammarRepo, prios, now)
	if err := p.Recompute(context.Background(), testIdentity); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	got := prios.replaced[0]
	wantPersistence := 4.0 / 3.0
	wantScore := 0.5 * wantPersistence
	if diff := math.Abs(got.Score - wantScore); diff > 0.01 {
		t.Errorf("Score = %v, want %v (+/-0.01)", got.Score, wantScore)
	}
	if !strings.Contains(got.Reason, "emerging") {
		t.Errorf("Reason = %q, want it to mention emerging", got.Reason)
	}
	if !strings.Contains(got.Reason, "4 occurrences") {
		t.Errorf("Reason = %q, want it to state the occurrence count input", got.Reason)
	}
}

// TestReplaceAllReceivesRowsSortedByScoreDescending pins the brief's
// ordering contract on ReplaceAll's input.
func TestReplaceAllReceivesRowsSortedByScoreDescending(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	obs := &fakeObsRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{
		testIdentity: {
			// score = min(3/3,3)*1*1 = 1.0
			{ID: "o1", IdentityID: testIdentity, Kind: learnermodel.KindWeakness, SubjectType: learnermodel.SubjectCorrectionType, Subject: "conjugation", Evidence: map[string]any{"count": 3}},
			// score = min(9/3,3)*1*1 = 3.0 (highest)
			{ID: "o2", IdentityID: testIdentity, Kind: learnermodel.KindWeakness, SubjectType: learnermodel.SubjectCorrectionType, Subject: "particle", Evidence: map[string]any{"count": 9}},
			// score = 0.5*min(12/3,3) = 1.5
			{ID: "o3", IdentityID: testIdentity, Kind: learnermodel.KindEmerging, SubjectType: learnermodel.SubjectCorrectionType, Subject: "word-order", Evidence: map[string]any{"count": 12}},
		},
	}}
	events := &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{}}
	prios := &fakePriorityRepo{}

	p := newPlanner(obs, events, grammarRepo, prios, now)
	if err := p.Recompute(context.Background(), testIdentity); err != nil {
		t.Fatalf("Recompute: %v", err)
	}

	if len(prios.replaced) != 3 {
		t.Fatalf("ReplaceAll received %d rows, want 3: %+v", len(prios.replaced), prios.replaced)
	}
	wantOrder := []string{"particle", "word-order", "conjugation"}
	for i, want := range wantOrder {
		if prios.replaced[i].Subject != want {
			t.Errorf("replaced[%d].Subject = %q, want %q (order: %+v)", i, prios.replaced[i].Subject, want, prios.replaced)
		}
	}
	for i := 1; i < len(prios.replaced); i++ {
		if prios.replaced[i-1].Score < prios.replaced[i].Score {
			t.Errorf("rows not sorted score DESC: [%d]=%.2f < [%d]=%.2f", i-1, prios.replaced[i-1].Score, i, prios.replaced[i].Score)
		}
	}
}

// TestRecomputeCallsReplaceAllEvenWithNoObservations: a learner with no
// observations yet must still get an (empty) ReplaceAll call — a
// pre-existing (now stale) priority list must be cleared, not left
// behind.
func TestRecomputeCallsReplaceAllEvenWithNoObservations(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	obs := &fakeObsRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{}}
	events := &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{}}
	prios := &fakePriorityRepo{replaced: []storage.Priority{{Subject: "stale"}}}

	p := newPlanner(obs, events, grammarRepo, prios, now)
	if err := p.Recompute(context.Background(), testIdentity); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if len(prios.replaced) != 0 {
		t.Errorf("replaced = %+v, want empty (stale row must be cleared)", prios.replaced)
	}
}

// TestRecomputePropagatesReplaceAllError: a ReplaceAll failure must
// surface to the caller, not be swallowed.
func TestRecomputePropagatesReplaceAllError(t *testing.T) {
	now := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	obs := &fakeObsRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{}}
	events := &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{}}
	prios := &fakePriorityRepo{replaceErr: context.DeadlineExceeded}

	p := newPlanner(obs, events, grammarRepo, prios, now)
	if err := p.Recompute(context.Background(), testIdentity); err == nil {
		t.Fatal("expected an error when ReplaceAll fails, got nil")
	}
}
