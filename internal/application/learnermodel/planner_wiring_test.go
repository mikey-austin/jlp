package learnermodel_test

import (
	"context"
	"sync"
	"testing"
	"time"

	applearnermodel "github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakePriorityRepo captures ReplaceAll calls — enough to prove the
// Updater/Rebuild -> planner.Planner wiring actually reaches
// Recompute, without needing a database. Guarded by mu: with debounced
// Recompute (see debounce_test.go), a production-schedule test runs
// ReplaceAll from a real background goroutine (a time.AfterFunc
// callback) concurrently with the test goroutine's own assertions.
type fakePriorityRepo struct {
	mu    sync.Mutex
	calls int
	last  []storage.Priority
}

func (f *fakePriorityRepo) ReplaceAll(_ context.Context, _ learner.IdentityID, ps []storage.Priority) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = ps
	return nil
}

func (f *fakePriorityRepo) Top(context.Context, learner.IdentityID, int) ([]storage.Priority, error) {
	panic("not used by planner wiring tests")
}

// snapshot returns calls/last under the lock — the race-safe way test
// goroutines should read state a background Recompute might be writing
// concurrently.
func (f *fakePriorityRepo) snapshot() (calls int, last []storage.Priority) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.last
}

// panicGrammarRepo is a storage.GrammarRepository double whose
// GetConcept always reports ErrNotFound (the planner's documented
// weight-0 fallback) — these wiring tests only care THAT Recompute
// ran, not what it scored, so no concept ever needs to resolve.
type panicGrammarRepo struct{}

func (panicGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by planner wiring tests")
}

func (panicGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	panic("not used by planner wiring tests")
}

func (panicGrammarRepo) GetConcept(context.Context, string) (grammar.Concept, error) {
	return grammar.Concept{}, storage.ErrNotFound
}

func (panicGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by planner wiring tests")
}

func (panicGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by planner wiring tests")
}

// panicVocabRepo is a storage.VocabularyRepository double for the same
// reason panicGrammarRepo exists: these wiring tests exercise
// planner.Recompute only, never ActivationCandidates, so every method
// panics if actually called.
type panicVocabRepo struct{}

func (panicVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabulary.Item, error) {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used by planner wiring tests")
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (panicVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	panic("not used by planner wiring tests")
}

func (panicVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	panic("not used by planner wiring tests")
}

// TestHandleEventTriggersPlannerRecomputeWhenWired pins the live-path
// half of the brief's Step 3 wiring — now debounced (see
// debounce_test.go for the full coalescing contract): once SetPlanner
// has been called, HandleEvent for a classifiable event must NOT call
// Recompute synchronously (the controller-ruled fix keeping the
// planner off the request path), but must ARM it — Recompute runs only
// once the (test-controlled) schedule actually fires.
func TestHandleEventTriggersPlannerRecomputeWhenWired(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	prios := &fakePriorityRepo{}
	p := planner.NewPlanner(obs, store, panicGrammarRepo{}, prios, panicVocabRepo{}, func() time.Time { return baseTime })

	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })
	u.SetPlanner(p)
	sched := newFakeSchedule()
	u.SetSchedule(sched.schedule)

	fireAll(t, u, store, correctionEvent("c1", "conjugation", "incorrect", baseTime))

	if prios.calls != 0 {
		t.Fatalf("Recompute ran %d times synchronously inside HandleEvent — it must be debounced off the request path", prios.calls)
	}

	sched.trigger(string(testIdentity))
	if prios.calls == 0 {
		t.Fatal("HandleEvent did not arm a debounced Recompute for a classifiable event")
	}
}

// TestHandleEventWithoutPlannerIsSafeNoOp: an Updater built via
// NewUpdater alone (SetPlanner never called) must behave exactly as it
// did before this task — no nil-pointer panic, no attempt to touch a
// priority repository.
func TestHandleEventWithoutPlannerIsSafeNoOp(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })

	fireAll(t, u, store, correctionEvent("c1", "conjugation", "incorrect", baseTime))
}

// TestRebuildRecomputesPrioritiesExactlyOnceAtTheEnd pins the
// Rebuild-path half of Step 3: a full replay must trigger exactly ONE
// Recompute, after the replay finishes — not once per historical event
// (Rebuild's internal Updater is deliberately built without a planner
// wired; see rebuild.go's doc comment) — because replaying years of
// history one event at a time would otherwise mean thousands of wasted
// full recomputes.
func TestRebuildRecomputesPrioritiesExactlyOnceAtTheEnd(t *testing.T) {
	stream := buildSyntheticStream()
	store := newFakeEventStore()
	ctx := context.Background()
	for _, ev := range stream {
		if err := store.Append(ctx, ev); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	obs := newFakeObsRepo()
	prios := &fakePriorityRepo{}
	rebuildClock := baseTime.Add(365 * 24 * time.Hour)
	p := planner.NewPlanner(obs, store, panicGrammarRepo{}, prios, panicVocabRepo{}, func() time.Time { return rebuildClock })

	if err := applearnermodel.Rebuild(ctx, testIdentity, store, obs, func() time.Time { return rebuildClock }, p); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if prios.calls != 1 {
		t.Fatalf("planner.Recompute called %d times during Rebuild, want exactly 1 (once at the end, not once per replayed event)", prios.calls)
	}
}

// TestRebuildWithNilPlannerIsSafe: passing nil (a caller that doesn't
// need priorities recomputed, e.g. a test focused only on observation
// determinism) must not panic or error.
func TestRebuildWithNilPlannerIsSafe(t *testing.T) {
	stream := buildSyntheticStream()
	store := newFakeEventStore()
	ctx := context.Background()
	for _, ev := range stream {
		if err := store.Append(ctx, ev); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	obs := newFakeObsRepo()
	if err := applearnermodel.Rebuild(ctx, testIdentity, store, obs, func() time.Time { return baseTime }, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
}

func (panicVocabRepo) ListPage(context.Context, learner.IdentityID, string, storage.VocabularyCursor, int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
	panic("planner must not reach the vocabulary repository here")
}

// ListRecentUnpracticed is 練習's word-drill source; no test in this
// package drills words, so reaching it means a wiring mistake.
func (panicVocabRepo) ListRecentUnpracticed(context.Context, learner.IdentityID, time.Time, int) ([]vocabulary.Item, error) {
	panic("not used by these tests")
}
