package learnermodel_test

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	applearnermodel "github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
)

// buildSyntheticStream is a realistic, out-of-the-box event stream for
// one identity: three "conjugation" corrections that create a
// weakness, a concept stream that never reaches the threshold (2
// distinct correction_ids), then 15 quiet days, then an unrelated
// "particle" occurrence — duplicated once via a same-correction_id
// retry — that both (a) proves dedup still holds during a rebuild and
// (b) via the sweep, flips "conjugation" to emerging.
func buildSyntheticStream() []event.LearningEvent {
	return []event.LearningEvent{
		correctionEvent("c1", "conjugation", "incorrect", baseTime),
		correctionEvent("c2", "conjugation", "incorrect", baseTime.Add(time.Hour)),
		correctionEvent("c3", "conjugation", "incorrect", baseTime.Add(2*time.Hour)),
		conceptEvent("ev-i1", "i-adjective-past", "c1", "conjugation", "incorrect", baseTime),
		conceptEvent("ev-i2", "i-adjective-past", "c2", "conjugation", "incorrect", baseTime.Add(time.Hour)),
		correctionEvent("c4", "particle", "incorrect", baseTime.Add(15*24*time.Hour)),
		// A retried publish: same correction_id "c4" but a distinct
		// event (as a real retry would be — the Recorder mints a fresh
		// event ID each call) — must not double count toward "particle".
		retriedCorrectionEvent("c4", "particle", "incorrect", baseTime.Add(15*24*time.Hour+time.Minute)),
		correctionEvent("c5", "particle", "incorrect", baseTime.Add(15*24*time.Hour+2*time.Minute)),
		correctionEvent("c6", "particle", "incorrect", baseTime.Add(15*24*time.Hour+3*time.Minute)),
	}
}

// retriedCorrectionEvent is correctionEvent with a distinct event ID —
// simulating a retried publish of the same underlying correction
// (same correction_id/Subject, different event row).
func retriedCorrectionEvent(correctionID, corrType, severity string, at time.Time) event.LearningEvent {
	ev := correctionEvent(correctionID, corrType, severity, at)
	ev.ID += "-retry"
	return ev
}

// TestRebuildIsByteIdenticalToLiveProcessing is PRD §14's
// rebuildability contract: replaying a whole event stream through
// Rebuild must land on the exact same observations (Kind, SubjectType,
// Subject, Confidence, Evidence) as processing the same stream live,
// one event at a time through HandleEvent — IDs and timestamps are
// bookkeeping, not part of the fold, so they're excluded from the
// comparison.
func TestRebuildIsByteIdenticalToLiveProcessing(t *testing.T) {
	stream := buildSyntheticStream()

	// Live run: append-then-handle each event in order, exactly as
	// learning.Recorder does in production.
	liveStore := newFakeEventStore()
	liveObs := newFakeObsRepo()
	liveClock := baseTime
	liveUpdater := applearnermodel.NewUpdater(liveStore, liveObs, func() time.Time { return liveClock })
	ctx := context.Background()
	for _, ev := range stream {
		if err := liveStore.Append(ctx, ev); err != nil {
			t.Fatalf("live append: %v", err)
		}
		liveClock = ev.OccurredAt
		if err := liveUpdater.HandleEvent(ctx, ev); err != nil {
			t.Fatalf("live HandleEvent: %v", err)
		}
	}

	// Rebuild run: the SAME events already fully durable in a fresh
	// store (as they'd be for a real `jlp rebuild-model`), replayed
	// oldest-first through Rebuild. Uses a deliberately different clock
	// (a fixed "rebuild ran later" instant) to prove the fold result
	// doesn't depend on wall-clock time, only on the stored OccurredAt
	// values.
	rebuildStore := newFakeEventStore()
	for _, ev := range stream {
		if err := rebuildStore.Append(ctx, ev); err != nil {
			t.Fatalf("rebuild seed append: %v", err)
		}
	}
	rebuildObs := newFakeObsRepo()
	rebuildClock := baseTime.Add(365 * 24 * time.Hour)
	if err := applearnermodel.Rebuild(ctx, testIdentity, rebuildStore, rebuildObs, func() time.Time { return rebuildClock }); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	liveList, err := liveObs.List(ctx, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	rebuildList, err := rebuildObs.List(ctx, testIdentity)
	if err != nil {
		t.Fatal(err)
	}

	got := normalizeObservations(rebuildList)
	want := normalizeObservations(liveList)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuild observations differ from live-processed observations:\n live    = %+v\n rebuild = %+v", want, got)
	}
	// Sanity: the stream was built to actually exercise the interesting
	// cases (weakness, emerging, sub-threshold) — fail loudly if a
	// future edit to buildSyntheticStream silently degenerates to "no
	// observations at all", which would make the equality check above
	// vacuous.
	if len(want) == 0 {
		t.Fatal("test setup produced zero observations; synthetic stream no longer exercises anything")
	}
}

// comparableObservation drops ID/FirstSeen/UpdatedAt/IdentityID (the
// latter is constant across this test) so reflect.DeepEqual compares
// only what the fold is actually responsible for reproducing.
type comparableObservation struct {
	Kind        learnermodel.ObservationKind
	SubjectType learnermodel.SubjectType
	Subject     string
	Confidence  float64
	Evidence    map[string]any
}

func normalizeObservations(obs []learnermodel.Observation) []comparableObservation {
	out := make([]comparableObservation, 0, len(obs))
	for _, o := range obs {
		out = append(out, comparableObservation{
			Kind:        o.Kind,
			SubjectType: o.SubjectType,
			Subject:     o.Subject,
			Confidence:  o.Confidence,
			Evidence:    o.Evidence,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SubjectType != out[j].SubjectType {
			return out[i].SubjectType < out[j].SubjectType
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}
