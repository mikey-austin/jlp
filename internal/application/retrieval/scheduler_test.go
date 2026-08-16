package retrieval_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	apperetrieval "github.com/mikeyaustin/jlp/internal/application/retrieval"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const testIdentity = learner.IdentityID("learner-a")

// stepDurations mirrors the brief's base interval table verbatim —
// duplicated here (not imported) so the test pins the actual PRD §54
// values, not whatever the package under test happens to compute them
// as.
var stepDurations = []time.Duration{
	24 * time.Hour,
	3 * 24 * time.Hour,
	7 * 24 * time.Hour,
	16 * 24 * time.Hour,
	35 * 24 * time.Hour,
	90 * 24 * time.Hour,
}

// fakeRetrievalRepo is an in-memory storage.RetrievalRepository double:
// identity-scoped Get/Upsert over a map keyed by (identity, subjectType,
// subject), plus captured/injectable Due behaviour for the ordering
// tests.
type fakeRetrievalRepo struct {
	items map[string]storage.RetrievalItem

	getErr error

	dueItems     []storage.RetrievalItem
	dueErr       error
	dueIdentity  learner.IdentityID
	dueAt        time.Time
	dueLimit     int
	dueCallCount int

	upsertErr error
}

func newFakeRetrievalRepo() *fakeRetrievalRepo {
	return &fakeRetrievalRepo{items: map[string]storage.RetrievalItem{}}
}

func retrievalKey(identity learner.IdentityID, subjectType, subject string) string {
	return string(identity) + "|" + subjectType + "|" + subject
}

func (f *fakeRetrievalRepo) Upsert(_ context.Context, it storage.RetrievalItem) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.items[retrievalKey(it.IdentityID, it.SubjectType, it.Subject)] = it
	return nil
}

func (f *fakeRetrievalRepo) Get(_ context.Context, identity learner.IdentityID, subjectType, subject string) (storage.RetrievalItem, error) {
	if f.getErr != nil {
		return storage.RetrievalItem{}, f.getErr
	}
	it, ok := f.items[retrievalKey(identity, subjectType, subject)]
	if !ok {
		return storage.RetrievalItem{}, storage.ErrNotFound
	}
	return it, nil
}

func (f *fakeRetrievalRepo) Due(_ context.Context, identity learner.IdentityID, at time.Time, limit int) ([]storage.RetrievalItem, error) {
	f.dueCallCount++
	f.dueIdentity = identity
	f.dueAt = at
	f.dueLimit = limit
	return f.dueItems, f.dueErr
}

func (f *fakeRetrievalRepo) List(context.Context, learner.IdentityID, int) ([]storage.RetrievalItem, error) {
	panic("not used by scheduler tests")
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// seedStep pre-populates repo with an item at step idx (an index into
// stepDurations) for (subjectType, subject) — idx == -1 means "no
// existing row" (nothing seeded, Get misses with ErrNotFound), matching
// how stepFor treats a zero-value Interval.
func seedStep(repo *fakeRetrievalRepo, subjectType, subject string, idx int) {
	if idx < 0 {
		return
	}
	repo.items[retrievalKey(testIdentity, subjectType, subject)] = storage.RetrievalItem{
		IdentityID:  testIdentity,
		SubjectType: subjectType,
		Subject:     subject,
		Interval:    stepDurations[idx],
		Successes:   2,
		Failures:    1,
		LastSeen:    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		DueAt:       time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(stepDurations[idx]),
	}
}

// TestRecordOutcomeIntervalTable is the brief's Step 1 requirement:
// every transition of the interval policy, from every starting step
// (including "never scheduled"), for every outcome kind (failure,
// plain success, shaky-confidence success, max-confidence success),
// against a fixed clock.
func TestRecordOutcomeIntervalTable(t *testing.T) {
	now := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)

	type outcome struct {
		name       string
		success    bool
		confidence int
	}
	outcomes := []struct {
		outcome outcome
		wantIdx map[int]int // startIdx -> wantIdx
	}{
		{outcome{"failure", false, 0}, map[int]int{-1: 0, 0: 0, 1: 0, 2: 0, 3: 1, 4: 2, 5: 3}},
		{outcome{"success/no-confidence", true, 0}, map[int]int{-1: 0, 0: 1, 1: 2, 2: 3, 3: 4, 4: 5, 5: 5}},
		{outcome{"success/confidence-3", true, 3}, map[int]int{-1: 0, 0: 1, 1: 2, 2: 3, 3: 4, 4: 5, 5: 5}},
		{outcome{"success/confidence-4", true, 4}, map[int]int{-1: 0, 0: 1, 1: 2, 2: 3, 3: 4, 4: 5, 5: 5}},
		{outcome{"success/confidence-1-shaky", true, 1}, map[int]int{-1: 0, 0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5}},
		{outcome{"success/confidence-2-shaky", true, 2}, map[int]int{-1: 0, 0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5}},
		{outcome{"success/confidence-5-double-advance", true, 5}, map[int]int{-1: 1, 0: 2, 1: 3, 2: 4, 3: 5, 4: 5, 5: 5}},
	}

	for _, oc := range outcomes {
		for startIdx, wantIdx := range oc.wantIdx {
			t.Run(fmt.Sprintf("%s/from-step-%d", oc.outcome.name, startIdx), func(t *testing.T) {
				repo := newFakeRetrievalRepo()
				subject := "test-subject"
				seedStep(repo, "concept", subject, startIdx)

				sched := apperetrieval.NewScheduler(repo, fixedClock(now))
				if err := sched.RecordOutcome(context.Background(), testIdentity, "concept", subject, oc.outcome.success, oc.outcome.confidence); err != nil {
					t.Fatalf("RecordOutcome (%s, start=%d) returned error: %v", oc.outcome.name, startIdx, err)
				}

				got, err := repo.Get(context.Background(), testIdentity, "concept", subject)
				if err != nil {
					t.Fatalf("repo.Get after RecordOutcome: %v", err)
				}
				wantInterval := stepDurations[wantIdx]
				if got.Interval != wantInterval {
					t.Fatalf("%s from step %d: Interval = %v, want %v (step %d)", oc.outcome.name, startIdx, got.Interval, wantInterval, wantIdx)
				}
				wantDue := now.Add(wantInterval)
				if !got.DueAt.Equal(wantDue) {
					t.Fatalf("%s from step %d: DueAt = %v, want %v", oc.outcome.name, startIdx, got.DueAt, wantDue)
				}
				if !got.LastSeen.Equal(now) {
					t.Fatalf("%s from step %d: LastSeen = %v, want %v", oc.outcome.name, startIdx, got.LastSeen, now)
				}
			})
		}
	}
}

// TestRecordOutcomeCountsSuccessesAndFailures pins that Successes/
// Failures accumulate across calls (not just the step index) — a
// learner's history, not only their current interval.
func TestRecordOutcomeCountsSuccessesAndFailures(t *testing.T) {
	repo := newFakeRetrievalRepo()
	sched := apperetrieval.NewScheduler(repo, fixedClock(time.Now()))
	ctx := context.Background()

	if err := sched.RecordOutcome(ctx, testIdentity, "concept", "te-form", true, 0); err != nil {
		t.Fatal(err)
	}
	if err := sched.RecordOutcome(ctx, testIdentity, "concept", "te-form", true, 0); err != nil {
		t.Fatal(err)
	}
	if err := sched.RecordOutcome(ctx, testIdentity, "concept", "te-form", false, 0); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, testIdentity, "concept", "te-form")
	if err != nil {
		t.Fatal(err)
	}
	if got.Successes != 2 {
		t.Errorf("Successes = %d, want 2", got.Successes)
	}
	if got.Failures != 1 {
		t.Errorf("Failures = %d, want 1", got.Failures)
	}
}

// TestRecordOutcomeStoresConfidence pins storage.RetrievalItem.
// Confidence's documented "0 when unknown" contract end to end.
func TestRecordOutcomeStoresConfidence(t *testing.T) {
	repo := newFakeRetrievalRepo()
	sched := apperetrieval.NewScheduler(repo, fixedClock(time.Now()))
	ctx := context.Background()

	if err := sched.RecordOutcome(ctx, testIdentity, "expression", "積もる", true, 4); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, testIdentity, "expression", "積もる")
	if err != nil {
		t.Fatal(err)
	}
	if got.Confidence != 4 {
		t.Errorf("Confidence = %v, want 4", got.Confidence)
	}

	if err := sched.RecordOutcome(ctx, testIdentity, "expression", "積もる", true, 0); err != nil {
		t.Fatal(err)
	}
	got, err = repo.Get(ctx, testIdentity, "expression", "積もる")
	if err != nil {
		t.Fatal(err)
	}
	if got.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0 (unknown)", got.Confidence)
	}
}

// TestRecordOutcomePropagatesGetError pins that a non-ErrNotFound Get
// failure is a hard error, not silently treated as "no existing row".
func TestRecordOutcomePropagatesGetError(t *testing.T) {
	repo := newFakeRetrievalRepo()
	repo.getErr = errors.New("db down")
	sched := apperetrieval.NewScheduler(repo, fixedClock(time.Now()))

	err := sched.RecordOutcome(context.Background(), testIdentity, "concept", "te-form", true, 0)
	if err == nil {
		t.Fatal("RecordOutcome returned nil, want the propagated Get error")
	}
}

// TestRecordOutcomePropagatesUpsertError pins the same for Upsert.
func TestRecordOutcomePropagatesUpsertError(t *testing.T) {
	repo := newFakeRetrievalRepo()
	repo.upsertErr = errors.New("db down")
	sched := apperetrieval.NewScheduler(repo, fixedClock(time.Now()))

	err := sched.RecordOutcome(context.Background(), testIdentity, "concept", "te-form", true, 0)
	if err == nil {
		t.Fatal("RecordOutcome returned nil, want the propagated Upsert error")
	}
}

// TestDueSubjectsPassesClockAndArgsThrough pins DueSubjects as a thin
// passthrough to storage.RetrievalRepository.Due: identity/limit passed
// exactly, "at" taken from the injected clock (never time.Now()), and
// the repo's own result/order returned unchanged — Due's ordering
// contract lives in the repository, not re-sorted here.
func TestDueSubjectsPassesClockAndArgsThrough(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	repo := newFakeRetrievalRepo()
	repo.dueItems = []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "te-form", DueAt: now.Add(-2 * time.Hour)},
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "積もる", DueAt: now.Add(-1 * time.Hour)},
	}
	sched := apperetrieval.NewScheduler(repo, fixedClock(now))

	got, err := sched.DueSubjects(context.Background(), testIdentity, 7)
	if err != nil {
		t.Fatalf("DueSubjects returned error: %v", err)
	}
	if repo.dueCallCount != 1 {
		t.Fatalf("repo.Due called %d times, want 1", repo.dueCallCount)
	}
	if repo.dueIdentity != testIdentity {
		t.Errorf("repo.Due identity = %q, want %q", repo.dueIdentity, testIdentity)
	}
	if !repo.dueAt.Equal(now) {
		t.Errorf("repo.Due at = %v, want the injected clock's %v (never time.Now())", repo.dueAt, now)
	}
	if repo.dueLimit != 7 {
		t.Errorf("repo.Due limit = %d, want 7", repo.dueLimit)
	}
	if len(got) != 2 || got[0].Subject != "te-form" || got[1].Subject != "積もる" {
		t.Fatalf("DueSubjects result = %+v, want the repo's items in the SAME order", got)
	}
}

// TestRecordOutcomeConcurrentSameSubjectNeverLosesAnUpdate pins
// RecordOutcome's per-subject serialization (subjectLocks): N
// concurrent calls for the SAME (identity, subjectType, subject) must
// each be reflected in the final Successes+Failures count — a
// non-atomic Get-then-Upsert without locking would let two racing
// calls both read the same pre-update state and have the second
// Upsert silently clobber the first, undercounting the total. Run
// under `go test -race` (make test-race), which would also flag any
// unsynchronized concurrent map access this test's own goroutines
// might otherwise trigger.
func TestRecordOutcomeConcurrentSameSubjectNeverLosesAnUpdate(t *testing.T) {
	const n = 40
	repo := newFakeRetrievalRepo()
	sched := apperetrieval.NewScheduler(repo, fixedClock(time.Now()))

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			success := i%2 == 0
			if err := sched.RecordOutcome(context.Background(), testIdentity, "concept", "shared-subject", success, 0); err != nil {
				t.Errorf("RecordOutcome[%d] returned error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := repo.Get(context.Background(), testIdentity, "concept", "shared-subject")
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	if total := got.Successes + got.Failures; total != n {
		t.Fatalf("Successes+Failures = %d, want %d (no update lost to a race)", total, n)
	}
}

// TestDueSubjectsPropagatesError pins DueSubjects failing hard on a
// repo error rather than returning an empty/partial result.
func TestDueSubjectsPropagatesError(t *testing.T) {
	repo := newFakeRetrievalRepo()
	repo.dueErr = errors.New("db down")
	sched := apperetrieval.NewScheduler(repo, fixedClock(time.Now()))

	_, err := sched.DueSubjects(context.Background(), testIdentity, 5)
	if err == nil {
		t.Fatal("DueSubjects returned nil, want the propagated Due error")
	}
}
