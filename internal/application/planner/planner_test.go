package planner_test

import (
	"context"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
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
	// topResult/topErr back TopConcept's tests below; topCalls records
	// every limit Top was called with, so those tests can pin
	// TopConcept's own "Top(identity, 1)" contract, not just its return
	// value.
	topResult []storage.Priority
	topErr    error
	topCalls  []int
}

func (f *fakePriorityRepo) ReplaceAll(_ context.Context, _ learner.IdentityID, ps []storage.Priority) error {
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.replaced = ps
	return nil
}

func (f *fakePriorityRepo) Top(_ context.Context, _ learner.IdentityID, limit int) ([]storage.Priority, error) {
	f.topCalls = append(f.topCalls, limit)
	if f.topErr != nil {
		return nil, f.topErr
	}
	return f.topResult, nil
}

// fakeVocabRepo is an in-memory storage.VocabularyRepository double
// exercised only by the ActivationCandidates tests below: byIdentity
// holds pre-set, already-"activate"-filtered data (the real WHERE-clause
// filter is pinned at the postgres integration layer, not
// re-implemented here — same "canned data" style fakePriorityRepo.Top
// uses); ListActivationCandidates mirrors the real adapter's own
// contract by sorting/capping it (see
// storage.VocabularyRepository.ListActivationCandidates), so these
// tests exercise planner.ActivationCandidates as the thin passthrough
// it now is.
type fakeVocabRepo struct {
	byIdentity map[learner.IdentityID][]vocabulary.Item
	listErr    error
}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used by planner tests")
}

func (f *fakeVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	panic("not used by planner tests")
}

func (f *fakeVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabulary.Item, error) {
	panic("not used by planner tests")
}

// ListActivationCandidates copies byIdentity's slice before sorting it
// (never mutating the test's own backing array in place) — mirroring
// the real postgres adapter, whose SQL query always returns a fresh
// slice, never a reference into caller-owned state.
func (f *fakeVocabRepo) ListActivationCandidates(_ context.Context, identity learner.IdentityID, limit int) ([]vocabulary.Item, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	items := append([]vocabulary.Item(nil), f.byIdentity[identity]...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Lookups > items[j].Lookups })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (f *fakeVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by planner tests")
}

func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	panic("not used by planner tests")
}

func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used by planner tests")
}

func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used by planner tests")
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	return nil
}

func (f *fakeVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	return nil
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
	return newPlannerWithVocab(obs, events, grammarRepo, prios, &fakeVocabRepo{}, now)
}

func newPlannerWithVocab(obs *fakeObsRepo, events *fakeEventStore, grammarRepo *fakeGrammarRepo, prios *fakePriorityRepo, vocab *fakeVocabRepo, now time.Time) *planner.Planner {
	return planner.NewPlanner(obs, events, grammarRepo, prios, vocab, func() time.Time { return now })
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

// --- ActivationCandidates (Task 7, PRD §55/§17.5) ---

// TestActivationCandidatesOrdersByLookupsDescendingAndLimits pins the
// brief's "ordered lookups DESC, LIMIT n" contract: List's own
// ordering (last_event DESC in the real query) must NOT leak through —
// ActivationCandidates re-sorts by Lookups DESC itself before applying
// limit.
func TestActivationCandidatesOrdersByLookupsDescendingAndLimits(t *testing.T) {
	vocab := &fakeVocabRepo{byIdentity: map[learner.IdentityID][]vocabulary.Item{
		testIdentity: {
			{ID: "v1", Expression: "気配", Lookups: 3},
			{ID: "v2", Expression: "それはそれとして", Lookups: 7},
			{ID: "v3", Expression: "〜に越したことはない", Lookups: 5},
		},
	}}
	p := newPlannerWithVocab(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{}, &fakePriorityRepo{}, vocab, time.Now())

	got, err := p.ActivationCandidates(context.Background(), testIdentity, 2)
	if err != nil {
		t.Fatalf("ActivationCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (limited)", len(got))
	}
	if got[0].Expression != "それはそれとして" || got[1].Expression != "〜に越したことはない" {
		t.Fatalf("got = %+v, want [それはそれとして(7), 〜に越したことはない(5)] in that order", got)
	}
}

// TestActivationCandidatesLimitLessThanOrEqualZeroReturnsAllSorted:
// limit<=0 means "no cap" — every matching item comes back, still
// sorted.
func TestActivationCandidatesLimitLessThanOrEqualZeroReturnsAllSorted(t *testing.T) {
	vocab := &fakeVocabRepo{byIdentity: map[learner.IdentityID][]vocabulary.Item{
		testIdentity: {
			{ID: "v1", Expression: "a", Lookups: 1},
			{ID: "v2", Expression: "b", Lookups: 9},
		},
	}}
	p := newPlannerWithVocab(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{}, &fakePriorityRepo{}, vocab, time.Now())

	got, err := p.ActivationCandidates(context.Background(), testIdentity, 0)
	if err != nil {
		t.Fatalf("ActivationCandidates: %v", err)
	}
	if len(got) != 2 || got[0].Expression != "b" || got[1].Expression != "a" {
		t.Fatalf("got = %+v, want both items, [b, a] sorted by Lookups DESC", got)
	}
}

// TestActivationCandidatesNoneReturnsEmpty: an identity with nothing
// matching the activate filter gets an empty (not nil-panicking) slice.
func TestActivationCandidatesNoneReturnsEmpty(t *testing.T) {
	vocab := &fakeVocabRepo{byIdentity: map[learner.IdentityID][]vocabulary.Item{}}
	p := newPlannerWithVocab(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{}, &fakePriorityRepo{}, vocab, time.Now())

	got, err := p.ActivationCandidates(context.Background(), testIdentity, 5)
	if err != nil {
		t.Fatalf("ActivationCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %+v, want empty", got)
	}
}

// TestActivationCandidatesPropagatesListError: a repository failure
// must surface to the caller, not be swallowed.
func TestActivationCandidatesPropagatesListError(t *testing.T) {
	vocab := &fakeVocabRepo{listErr: context.DeadlineExceeded}
	p := newPlannerWithVocab(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{}, &fakePriorityRepo{}, vocab, time.Now())

	if _, err := p.ActivationCandidates(context.Background(), testIdentity, 5); err == nil {
		t.Fatal("expected an error when List fails, got nil")
	}
}

// --- TopConcept (Task 9, PRD §17.2/§58) ---

// TestTopConceptResolvesHighestConceptPriority pins the brief's core
// scenario: application/practice.Service.Start's "fake planner returns
// i-adjective-past" — a top-1 concept-type priority resolves via
// GrammarRepository, and Top itself is called with limit=1 (not some
// larger scan-then-filter window).
func TestTopConceptResolvesHighestConceptPriority(t *testing.T) {
	prios := &fakePriorityRepo{topResult: []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5},
	}}
	grammarRepo := &fakeGrammarRepo{concepts: map[string]grammar.Concept{
		"i-adjective-past": {Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5},
	}}
	p := newPlanner(&fakeObsRepo{}, &fakeEventStore{}, grammarRepo, prios, time.Now())

	concept, ok, err := p.TopConcept(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("TopConcept: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if concept.Slug != "i-adjective-past" || concept.Name != "い-adjective past tense" {
		t.Fatalf("concept = %+v, want the resolved i-adjective-past concept", concept)
	}
	if len(prios.topCalls) != 1 || prios.topCalls[0] != 1 {
		t.Fatalf("Top called with limits %v, want [1]", prios.topCalls)
	}
}

// TestTopConceptFalseWhenTopPriorityIsCorrectionType: when identity's
// single highest-scoring priority is correction-type (not concept), ok
// is false — application/practice.Service.Start falls back to a random
// catalog concept rather than drilling something unrelated.
func TestTopConceptFalseWhenTopPriorityIsCorrectionType(t *testing.T) {
	prios := &fakePriorityRepo{topResult: []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "correction-type", Subject: "conjugation", Score: 9.0},
	}}
	p := newPlanner(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{}, prios, time.Now())

	_, ok, err := p.TopConcept(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("TopConcept: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false (top priority is correction-type)")
	}
}

// TestTopConceptFalseWhenNoPriorities: a learner with no priorities yet
// gets ok=false, no error.
func TestTopConceptFalseWhenNoPriorities(t *testing.T) {
	prios := &fakePriorityRepo{}
	p := newPlanner(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{}, prios, time.Now())

	_, ok, err := p.TopConcept(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("TopConcept: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false (no priorities)")
	}
}

// TestTopConceptFalseWhenConceptSlugNotInCatalog mirrors scoreObservation's
// value() tolerance for a stale/hallucinated concept slug the catalog no
// longer resolves: TopConcept reports ok=false rather than erroring.
func TestTopConceptFalseWhenConceptSlugNotInCatalog(t *testing.T) {
	prios := &fakePriorityRepo{topResult: []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "stale-slug", Score: 5.0},
	}}
	p := newPlanner(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{concepts: map[string]grammar.Concept{}}, prios, time.Now())

	_, ok, err := p.TopConcept(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("TopConcept: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false (stale concept slug not in catalog)")
	}
}

// TestTopConceptPropagatesTopError: a PriorityRepository.Top failure
// must surface to the caller, not be swallowed.
func TestTopConceptPropagatesTopError(t *testing.T) {
	prios := &fakePriorityRepo{topErr: context.DeadlineExceeded}
	p := newPlanner(&fakeObsRepo{}, &fakeEventStore{}, &fakeGrammarRepo{}, prios, time.Now())

	if _, _, err := p.TopConcept(context.Background(), testIdentity); err == nil {
		t.Fatal("expected an error when Top fails, got nil")
	}
}

// ListPage delegates to List and truncates. This package does not page;
// implemented rather than stubbed so it returns real rows if it ever
// starts. See storage.VocabularyRepository.ListPage.
func (f *fakeVocabRepo) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
	all, err := f.List(ctx, identity, filter)
	if err != nil || len(all) <= limit {
		return all, storage.VocabularyCursor{}, err
	}
	page := all[:limit]
	last := page[len(page)-1]
	return page, storage.VocabularyCursor{LastEvent: last.LastEvent, ID: last.ID}, nil
}

// ListRecentUnpracticed is 練習's word-drill source; no test in this
// package drills words, so reaching it means a wiring mistake.
func (r *fakeVocabRepo) ListRecentUnpracticed(context.Context, learner.IdentityID, time.Time, int) ([]vocabulary.Item, error) {
	panic("not used by these tests")
}

// GetByIDs resolves due words for 練習; the practice double below is the
// only one that needs real behaviour.
func (r *fakeVocabRepo) GetByIDs(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by these tests")
}

// LatestExamples backs 練習's cloze drills.
func (r *fakeVocabRepo) LatestExamples(context.Context, learner.IdentityID, []string) (map[string]string, error) {
	panic("not used by these tests")
}
