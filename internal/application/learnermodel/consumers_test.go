package learnermodel_test

import (
	"context"
	"testing"
	"time"

	applearnermodel "github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

const testIdentity = learner.IdentityID("learner-consumers-test")

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeEventStore is an in-memory storage.LearningEventRepository,
// identity-scoped, that keeps events in append order (ListAll is
// therefore already oldest-first, matching the real adapter's
// occurred_at ASC guarantee since tests append in chronological order).
type fakeEventStore struct {
	byIdentity map[learner.IdentityID][]event.LearningEvent
}

func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{byIdentity: map[learner.IdentityID][]event.LearningEvent{}}
}

func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	f.byIdentity[ev.IdentityID] = append(f.byIdentity[ev.IdentityID], ev)
	return nil
}

func (f *fakeEventStore) ListRecent(_ context.Context, identity learner.IdentityID, sid *session.ID, limit int) ([]event.LearningEvent, error) {
	all := f.byIdentity[identity]
	out := make([]event.LearningEvent, 0, len(all))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		ev := all[i]
		if sid != nil && (ev.SessionID == nil || *ev.SessionID != *sid) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func (f *fakeEventStore) ListAll(_ context.Context, identity learner.IdentityID) ([]event.LearningEvent, error) {
	all := f.byIdentity[identity]
	out := make([]event.LearningEvent, len(all))
	copy(out, all)
	return out, nil
}

// fakeObsRepo is an in-memory storage.ObservationRepository that
// reproduces the real postgres adapter's ON CONFLICT (identity,
// subject_type, subject) semantics: an Upsert against an existing row
// keeps that row's ID and FirstSeen, replacing only Kind, Confidence,
// Evidence, and UpdatedAt — see internal/ports/storage/observations.go.
type fakeObsRepo struct {
	rows map[string]learnermodel.Observation // key: obsKey(...)
}

func newFakeObsRepo() *fakeObsRepo {
	return &fakeObsRepo{rows: map[string]learnermodel.Observation{}}
}

func obsKey(identity learner.IdentityID, st learnermodel.SubjectType, subject string) string {
	return string(identity) + "|" + string(st) + "|" + subject
}

func (f *fakeObsRepo) Upsert(_ context.Context, o learnermodel.Observation) error {
	key := obsKey(o.IdentityID, o.SubjectType, o.Subject)
	if existing, ok := f.rows[key]; ok {
		o.ID = existing.ID
		o.FirstSeen = existing.FirstSeen
	}
	f.rows[key] = o
	return nil
}

func (f *fakeObsRepo) List(_ context.Context, identity learner.IdentityID) ([]learnermodel.Observation, error) {
	var out []learnermodel.Observation
	for _, o := range f.rows {
		if o.IdentityID == identity {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeObsRepo) DeleteAll(_ context.Context, identity learner.IdentityID) error {
	for k, o := range f.rows {
		if o.IdentityID == identity {
			delete(f.rows, k)
		}
	}
	return nil
}

func (f *fakeObsRepo) find(identity learner.IdentityID, st learnermodel.SubjectType, subject string) (learnermodel.Observation, bool) {
	o, ok := f.rows[obsKey(identity, st, subject)]
	return o, ok
}

// correctionEvent builds a correction.presented event: Subject is the
// correction ID, Evidence carries type/severity — see
// internal/application/feedback/service.go's Record call.
func correctionEvent(correctionID, corrType, severity string, at time.Time) event.LearningEvent {
	return event.LearningEvent{
		ID:         "ev-corr-" + correctionID,
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionPresented,
		Subject:    correctionID,
		Evidence:   map[string]any{"type": corrType, "severity": severity},
		OccurredAt: at,
	}
}

// conceptEvent builds a grammar.concept.encountered event: Subject is
// the concept slug, Evidence carries the originating correction_id
// (used for dedup) plus type/severity.
func conceptEvent(eventID, slug, correctionID, corrType, severity string, at time.Time) event.LearningEvent {
	return event.LearningEvent{
		ID:         eventID,
		IdentityID: testIdentity,
		Type:       event.TypeGrammarConceptEncountered,
		Subject:    slug,
		Evidence:   map[string]any{"correction_id": correctionID, "type": corrType, "severity": severity},
		OccurredAt: at,
	}
}

func fireAll(t *testing.T, u *applearnermodel.Updater, store *fakeEventStore, evs ...event.LearningEvent) {
	t.Helper()
	ctx := context.Background()
	for _, ev := range evs {
		if err := store.Append(ctx, ev); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := u.HandleEvent(ctx, ev); err != nil {
			t.Fatalf("HandleEvent: %v", err)
		}
	}
}

func TestThreeIncorrectCorrectionsUpsertWeakness(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })

	fireAll(t, u, store,
		correctionEvent("c1", "conjugation", "incorrect", baseTime),
		correctionEvent("c2", "conjugation", "incorrect", baseTime.Add(time.Hour)),
		correctionEvent("c3", "conjugation", "incorrect", baseTime.Add(2*time.Hour)),
	)

	o, ok := obs.find(testIdentity, learnermodel.SubjectCorrectionType, "conjugation")
	if !ok {
		t.Fatal("expected a weakness observation for conjugation, found none")
	}
	if o.Kind != learnermodel.KindWeakness {
		t.Errorf("Kind = %q, want %q", o.Kind, learnermodel.KindWeakness)
	}
	if count, _ := o.Evidence["count"].(int); count != 3 {
		t.Errorf("Evidence[count] = %v, want 3", o.Evidence["count"])
	}
	if o.Evidence["window_days"] != 30 {
		t.Errorf("Evidence[window_days] = %v, want 30", o.Evidence["window_days"])
	}
	wantConfidence := 3.0 / 5.0
	if o.Confidence != wantConfidence {
		t.Errorf("Confidence = %v, want %v", o.Confidence, wantConfidence)
	}
}

func TestTwoIncorrectCorrectionsProduceNoObservation(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })

	fireAll(t, u, store,
		correctionEvent("c1", "conjugation", "incorrect", baseTime),
		correctionEvent("c2", "conjugation", "incorrect", baseTime.Add(time.Hour)),
	)

	list, err := obs.List(context.Background(), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("observations = %+v, want none (only 2 qualifying occurrences, threshold is 3)", list)
	}
}

func TestStyleSeverityCorrectionsDoNotCount(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })

	fireAll(t, u, store,
		correctionEvent("c1", "conjugation", "style", baseTime),
		correctionEvent("c2", "conjugation", "style", baseTime.Add(time.Hour)),
		correctionEvent("c3", "conjugation", "style", baseTime.Add(2*time.Hour)),
	)

	list, err := obs.List(context.Background(), testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("observations = %+v, want none (style severity never qualifies)", list)
	}
}

// TestWeaknessFlipsToEmergingAfterFourteenQuietDays pins the sweep
// rule: an existing weakness whose subject has had zero qualifying
// occurrences in the trailing 14 days flips to emerging. The sweep
// runs inside HandleEvent after every update, so it takes a wholly
// unrelated event arriving 15 days later to trigger it here — nothing
// about "particle" and "conjugation" interact directly.
func TestWeaknessFlipsToEmergingAfterFourteenQuietDays(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })

	fireAll(t, u, store,
		correctionEvent("c1", "conjugation", "incorrect", baseTime),
		correctionEvent("c2", "conjugation", "incorrect", baseTime.Add(time.Hour)),
		correctionEvent("c3", "conjugation", "incorrect", baseTime.Add(2*time.Hour)),
	)
	if o, ok := obs.find(testIdentity, learnermodel.SubjectCorrectionType, "conjugation"); !ok || o.Kind != learnermodel.KindWeakness {
		t.Fatalf("precondition failed: expected a conjugation weakness, got %+v (ok=%v)", o, ok)
	}

	quietUntil := baseTime.Add(15 * 24 * time.Hour)
	fireAll(t, u, store, correctionEvent("c4", "particle", "incorrect", quietUntil))

	o, ok := obs.find(testIdentity, learnermodel.SubjectCorrectionType, "conjugation")
	if !ok {
		t.Fatal("conjugation observation disappeared, want it to remain (as emerging)")
	}
	if o.Kind != learnermodel.KindEmerging {
		t.Errorf("Kind = %q after 15 quiet days, want %q", o.Kind, learnermodel.KindEmerging)
	}

	// The unrelated "particle" event is a single occurrence — nowhere
	// near the weakness threshold — so it must not itself have produced
	// an observation.
	if _, ok := obs.find(testIdentity, learnermodel.SubjectCorrectionType, "particle"); ok {
		t.Error("a single particle occurrence must not create an observation")
	}
}

// TestEmergingFlipsBackToWeaknessAfterThreeFreshOccurrences pins a
// Phase 2 review carryover: upsertWeakness unconditionally sets
// Kind=weakness whenever the threshold is met — it has no special
// case for "this subject was already emerging" — so a subject that
// quieted down into emerging and then accrues 3 FRESH qualifying
// occurrences (distinct correction IDs) flips straight back to
// weakness. This is existing, intentional behavior (per the
// controller ruling); this test only pins it against a future
// regression, it does not change it.
//
// The gap to day 40 (vs. the 15-day gap in
// TestWeaknessFlipsToEmergingAfterFourteenQuietDays) is deliberate:
// it pushes c1-c3 (day 0) outside the 30-day trailing weaknessWindow
// as measured from the fresh occurrences (day 40+), so the flip-back
// is driven ONLY by the 3 fresh occurrences below, not by the old
// ones still being in-window.
func TestEmergingFlipsBackToWeaknessAfterThreeFreshOccurrences(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })

	fireAll(t, u, store,
		correctionEvent("c1", "conjugation", "incorrect", baseTime),
		correctionEvent("c2", "conjugation", "incorrect", baseTime.Add(time.Hour)),
		correctionEvent("c3", "conjugation", "incorrect", baseTime.Add(2*time.Hour)),
	)
	if o, ok := obs.find(testIdentity, learnermodel.SubjectCorrectionType, "conjugation"); !ok || o.Kind != learnermodel.KindWeakness {
		t.Fatalf("precondition failed: expected a conjugation weakness, got %+v (ok=%v)", o, ok)
	}

	quietUntil := baseTime.Add(40 * 24 * time.Hour)
	fireAll(t, u, store, correctionEvent("c-particle", "particle", "incorrect", quietUntil))
	if o, ok := obs.find(testIdentity, learnermodel.SubjectCorrectionType, "conjugation"); !ok || o.Kind != learnermodel.KindEmerging {
		t.Fatalf("precondition failed: expected conjugation to have flipped to emerging, got %+v (ok=%v)", o, ok)
	}

	fireAll(t, u, store,
		correctionEvent("c5", "conjugation", "incorrect", quietUntil.Add(time.Hour)),
		correctionEvent("c6", "conjugation", "incorrect", quietUntil.Add(2*time.Hour)),
		correctionEvent("c7", "conjugation", "incorrect", quietUntil.Add(3*time.Hour)),
	)

	o, ok := obs.find(testIdentity, learnermodel.SubjectCorrectionType, "conjugation")
	if !ok {
		t.Fatal("conjugation observation disappeared, want it flipped back to weakness")
	}
	if o.Kind != learnermodel.KindWeakness {
		t.Errorf("Kind = %q after 3 fresh occurrences, want %q (flip-back)", o.Kind, learnermodel.KindWeakness)
	}
	if count, _ := o.Evidence["count"].(int); count != 3 {
		t.Errorf("Evidence[count] = %v, want 3 (only the 3 fresh occurrences — c1-c3 are now outside the 30-day window)", o.Evidence["count"])
	}
}

// TestDuplicateCorrectionIDCountsOnce pins the controller ruling:
// weakness counting dedupes by DISTINCT evidence.correction_id per
// subject, not raw event count, so a retried grammar.concept.encountered
// publish (same correction_id, two events) never double-counts.
func TestDuplicateCorrectionIDCountsOnce(t *testing.T) {
	store := newFakeEventStore()
	obs := newFakeObsRepo()
	u := applearnermodel.NewUpdater(store, obs, func() time.Time { return baseTime })

	fireAll(t, u, store,
		// c1 "presented" twice — a retry duplicate with the same
		// correction_id — must still count as ONE occurrence.
		conceptEvent("ev-1", "i-adjective-past", "c1", "conjugation", "incorrect", baseTime),
		conceptEvent("ev-1-retry", "i-adjective-past", "c1", "conjugation", "incorrect", baseTime.Add(time.Minute)),
	)
	if _, ok := obs.find(testIdentity, learnermodel.SubjectConcept, "i-adjective-past"); ok {
		t.Fatal("a duplicated single correction_id must not reach the weakness threshold")
	}

	fireAll(t, u, store,
		conceptEvent("ev-2", "i-adjective-past", "c2", "conjugation", "incorrect", baseTime.Add(time.Hour)),
	)
	if _, ok := obs.find(testIdentity, learnermodel.SubjectConcept, "i-adjective-past"); ok {
		t.Fatal("only 2 distinct correction_ids so far, must not reach the weakness threshold")
	}

	fireAll(t, u, store,
		conceptEvent("ev-3", "i-adjective-past", "c3", "conjugation", "incorrect", baseTime.Add(2*time.Hour)),
	)
	o, ok := obs.find(testIdentity, learnermodel.SubjectConcept, "i-adjective-past")
	if !ok {
		t.Fatal("expected a weakness observation for i-adjective-past, found none")
	}
	// 4 raw grammar.concept.encountered events were fired, but only 3
	// distinct correction_ids (c1, c2, c3): the count must reflect the
	// dedup, not the raw event total.
	if count, _ := o.Evidence["count"].(int); count != 3 {
		t.Errorf("Evidence[count] = %v, want 3 (deduped by correction_id, not 4 raw events)", o.Evidence["count"])
	}
}
