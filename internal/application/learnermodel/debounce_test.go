package learnermodel_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	applearnermodel "github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// fakeSchedule is a manually-triggered storage.PriorityRepository.
// Recompute-adjacent test double for the armRecompute debounce
// contract: instead of a real timer, schedule() just remembers the
// LATEST fire func per key (mirroring the production
// time.AfterFunc-based schedule's coalescing-by-Reset behavior — a
// re-arm for a key that's already pending never queues a second fire)
// and counts how many times schedule() itself was called per key.
// trigger(key) simulates the debounce window elapsing by invoking the
// stored fire func synchronously, on the test's own goroutine, so
// assertions immediately after trigger need no sleep/retry.
type fakeSchedule struct {
	mu        sync.Mutex
	fires     map[string]func()
	scheduled map[string]int // how many times schedule() was called per key
	triggered map[string]int // how many times trigger() actually invoked a fire func
}

func newFakeSchedule() *fakeSchedule {
	return &fakeSchedule{
		fires:     map[string]func(){},
		scheduled: map[string]int{},
		triggered: map[string]int{},
	}
}

func (f *fakeSchedule) schedule(key string, _ time.Duration, fire func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fires[key] = fire
	f.scheduled[key]++
}

// trigger fires the pending callback for key, if any — a no-op
// (silently) for a key nothing has ever been armed for, matching a
// real timer that was never started.
func (f *fakeSchedule) trigger(key string) {
	f.mu.Lock()
	fire, ok := f.fires[key]
	if ok {
		f.triggered[key]++
	}
	f.mu.Unlock()
	if ok {
		fire()
	}
}

func (f *fakeSchedule) scheduleCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduled[key]
}

// TestHandleEventDebouncesBurstIntoOneRecompute pins the controller
// ruling's core coalescing contract: 6 rapid HandleEvent calls for the
// SAME identity must arm the debounce repeatedly (schedule() called 6
// times — each one a legitimate re-arm) but must NOT run Recompute
// until the test fires it, and firing it must run Recompute EXACTLY
// once, not 6 times.
func TestHandleEventDebouncesBurstIntoOneRecompute(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	prios := &fakePriorityRepo{}
	p := planner.NewPlanner(obs, store, panicGrammarRepo{}, prios, panicVocabRepo{}, func() time.Time { return baseTime })

	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })
	u.SetPlanner(p)
	sched := newFakeSchedule()
	u.SetSchedule(sched.schedule)

	ctx := context.Background()
	for i := 0; i < 6; i++ {
		ev := correctionEvent(fmt.Sprintf("c%d", i), "conjugation", "incorrect", baseTime.Add(time.Duration(i)*time.Minute))
		if err := store.Append(ctx, ev); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := u.HandleEvent(ctx, ev); err != nil {
			t.Fatalf("HandleEvent: %v", err)
		}
	}

	if sched.scheduleCount(string(testIdentity)) != 6 {
		t.Fatalf("schedule() called %d times, want 6 (one re-arm per HandleEvent)", sched.scheduleCount(string(testIdentity)))
	}
	if prios.calls != 0 {
		t.Fatalf("Recompute ran %d times before the debounce fired, want 0", prios.calls)
	}

	sched.trigger(string(testIdentity))

	if prios.calls != 1 {
		t.Fatalf("Recompute ran %d times after firing once, want exactly 1", prios.calls)
	}
}

// TestHandleEventDebouncesPerIdentityIndependently pins the other half
// of the controller ruling: two different identities' debounces are
// independent — firing one must not run Recompute for the other, and
// each identity gets its own eventual call.
func TestHandleEventDebouncesPerIdentityIndependently(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	prios := &fakePriorityRepo{}
	p := planner.NewPlanner(obs, store, panicGrammarRepo{}, prios, panicVocabRepo{}, func() time.Time { return baseTime })

	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })
	u.SetPlanner(p)
	sched := newFakeSchedule()
	u.SetSchedule(sched.schedule)

	otherIdentity := learner.IdentityID("learner-debounce-other")
	evMine := correctionEvent("c1", "conjugation", "incorrect", baseTime)
	evOther := event.LearningEvent{
		ID:         "ev-other-c1",
		IdentityID: otherIdentity,
		Type:       event.TypeCorrectionPresented,
		Subject:    "c-other-1",
		Evidence:   map[string]any{"type": "conjugation", "severity": "incorrect"},
		OccurredAt: baseTime,
	}

	ctx := context.Background()
	if err := store.Append(ctx, evMine); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := u.HandleEvent(ctx, evMine); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if err := store.Append(ctx, evOther); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := u.HandleEvent(ctx, evOther); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if prios.calls != 0 {
		t.Fatalf("Recompute ran %d times before either debounce fired, want 0", prios.calls)
	}

	sched.trigger(string(testIdentity))
	if prios.calls != 1 {
		t.Fatalf("after firing testIdentity's debounce, Recompute ran %d times total, want 1 (otherIdentity's must not have fired)", prios.calls)
	}

	sched.trigger(string(otherIdentity))
	if prios.calls != 2 {
		t.Fatalf("after firing otherIdentity's debounce, Recompute ran %d times total, want 2", prios.calls)
	}
}

// TestArmRecomputeWithoutPlannerNeverSchedules: armRecompute (via
// HandleEvent) must be a complete no-op — including never calling
// schedule at all — when no planner is wired, so a schedule double set
// up by a test that doesn't care about priorities never sees a call it
// didn't expect.
func TestArmRecomputeWithoutPlannerNeverSchedules(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })
	sched := newFakeSchedule()
	u.SetSchedule(sched.schedule)

	fireAll(t, u, store, correctionEvent("c1", "conjugation", "incorrect", baseTime))

	if sched.scheduleCount(string(testIdentity)) != 0 {
		t.Fatalf("schedule() called %d times with no planner wired, want 0", sched.scheduleCount(string(testIdentity)))
	}
}

// TestRealTimerScheduleEventuallyFires is a light integration check on
// the PRODUCTION schedule (newTimerSchedule, installed by NewUpdater
// when SetSchedule is never called) — proving the real
// time.AfterFunc-based implementation actually fires on its own,
// asynchronously, without a test-provided manual trigger. It polls
// briefly rather than sleeping a fixed 250ms+ to keep the test fast and
// non-flaky under load.
func TestRealTimerScheduleEventuallyFires(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	prios := &fakePriorityRepo{}
	p := planner.NewPlanner(obs, store, panicGrammarRepo{}, prios, panicVocabRepo{}, func() time.Time { return baseTime })

	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })
	u.SetPlanner(p)
	// No SetSchedule call: exercises NewUpdater's default production
	// schedule (a real timer), not a test double.

	fireAll(t, u, store, correctionEvent("c1", "conjugation", "incorrect", baseTime))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls, _ := prios.snapshot(); calls > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("real timer-backed schedule never fired Recompute within 2s of a classifiable event")
}
