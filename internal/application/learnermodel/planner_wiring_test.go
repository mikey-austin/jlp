package learnermodel_test

import (
	"context"
	"testing"
	"time"

	applearnermodel "github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakePriorityRepo captures ReplaceAll calls — enough to prove the
// Updater/Rebuild -> planner.Planner wiring actually reaches
// Recompute, without needing a database.
type fakePriorityRepo struct {
	calls int
	last  []storage.Priority
}

func (f *fakePriorityRepo) ReplaceAll(_ context.Context, _ learner.IdentityID, ps []storage.Priority) error {
	f.calls++
	f.last = ps
	return nil
}

func (f *fakePriorityRepo) Top(context.Context, learner.IdentityID, int) ([]storage.Priority, error) {
	panic("not used by planner wiring tests")
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

// TestHandleEventTriggersPlannerRecomputeWhenWired pins the live-path
// half of the brief's Step 3 wiring: once SetPlanner has been called,
// every HandleEvent that classifies (a correction.presented or
// grammar.concept.encountered event) also recomputes the identity's
// priority list.
func TestHandleEventTriggersPlannerRecomputeWhenWired(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	prios := &fakePriorityRepo{}
	p := planner.NewPlanner(obs, store, panicGrammarRepo{}, prios, func() time.Time { return baseTime })

	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })
	u.SetPlanner(p)

	fireAll(t, u, store, correctionEvent("c1", "conjugation", "incorrect", baseTime))

	if prios.calls == 0 {
		t.Fatal("HandleEvent did not trigger planner.Recompute after a classifiable event")
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
	p := planner.NewPlanner(obs, store, panicGrammarRepo{}, prios, func() time.Time { return rebuildClock })

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
