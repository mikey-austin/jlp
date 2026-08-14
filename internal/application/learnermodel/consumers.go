// Package learnermodel is the event-bus consumer side of the learner
// model (PRD §13/§14/§44): Updater.HandleEvent reacts to
// correction.presented and grammar.concept.encountered events, folding
// each identity's history into learnermodel.Observation rows. Rebuild
// (rebuild.go) replays a whole event stream through the exact same
// HandleEvent logic — the detection rules live in exactly one place so
// live processing and a from-scratch rebuild can never disagree.
package learnermodel

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	// weaknessThreshold is the minimum distinct qualifying occurrences
	// within weaknessWindow before a subject is flagged a weakness.
	weaknessThreshold = 3
	weaknessWindow    = 30 * 24 * time.Hour
	// quietWindow is how long a weakness's subject must go without a
	// qualifying occurrence before it flips to emerging.
	quietWindow = 14 * 24 * time.Hour
	// confidenceDivisor: Confidence = min(1, occurrences/confidenceDivisor).
	confidenceDivisor = 5.0
)

// Updater is the bus-facing side of the learner model: NewUpdater's
// result is subscribed to correction.presented and
// grammar.concept.encountered via HandleEvent (an events.Handler).
type Updater struct {
	events  storage.LearningEventRepository
	obs     storage.ObservationRepository
	clock   func() time.Time
	planner *planner.Planner
}

// NewUpdater builds an Updater. clock is injectable so tests (and
// Rebuild, which passes its own clock through unchanged) control what
// "now" means for Observation.FirstSeen/UpdatedAt bookkeeping —
// distinct from the trailing-window math in HandleEvent, which is
// always anchored to the triggering event's own OccurredAt so live
// processing and a later replay of the same event stream compute
// identical windows regardless of wall-clock time (PRD §14
// rebuildability).
func NewUpdater(events storage.LearningEventRepository, obs storage.ObservationRepository, clock func() time.Time) *Updater {
	return &Updater{events: events, obs: obs, clock: clock}
}

// SetPlanner wires p into u: from this call on, every HandleEvent that
// classifies (see classify) also triggers p.Recompute for the event's
// identity, keeping the priority list (and thus the Teacher agent's
// RecentErrors, via storage.PriorityRepository.Top) in sync with the
// learner model as it changes. Optional — main.go's live bus consumer
// calls this once at startup; Rebuild's own internal Updater (see
// rebuild.go) deliberately never calls it, recomputing once at the end
// of a full replay instead of once per historical event. A nil
// receiver-side u.planner (the zero value) makes HandleEvent's
// recompute step a no-op, so tests that don't care about priorities
// need not call this at all.
func (u *Updater) SetPlanner(p *planner.Planner) {
	u.planner = p
}

// HandleEvent is the events.Handler registered for
// correction.presented and grammar.concept.encountered. It:
//  1. classifies ev into a (SubjectType, Subject) pair and recomputes
//     that subject's distinct qualifying-occurrence count over the
//     trailing 30 days (anchored at ev.OccurredAt); at or above
//     weaknessThreshold, it upserts a weakness observation;
//  2. sweeps every existing weakness for ev.IdentityID and flips any
//     whose subject has gone quietWindow with zero qualifying
//     occurrences to emerging;
//  3. if SetPlanner has wired a planner, recomputes ev.IdentityID's
//     priority list — see SetPlanner's doc comment.
//
// Any event type other than the two above is a no-op (defensive: only
// those two are ever subscribed to this handler in cmd/jlp/main.go).
func (u *Updater) HandleEvent(ctx context.Context, ev event.LearningEvent) error {
	subjectType, subject, ok := classify(ev)
	if !ok {
		return nil
	}

	all, err := u.events.ListAll(ctx, ev.IdentityID)
	if err != nil {
		return err
	}

	count := countOccurrences(all, ev.Type, subjectType, subject, ev.OccurredAt, weaknessWindow)
	if count >= weaknessThreshold {
		if err := u.upsertWeakness(ctx, ev.IdentityID, subjectType, subject, count); err != nil {
			return err
		}
	}

	if err := u.sweep(ctx, ev.IdentityID, all, ev.OccurredAt); err != nil {
		return err
	}

	if u.planner != nil {
		if err := u.planner.Recompute(ctx, ev.IdentityID); err != nil {
			return fmt.Errorf("learnermodel: recompute priorities: %w", err)
		}
	}
	return nil
}

// upsertWeakness (re)asserts a weakness observation for subject. It
// always builds a fresh Observation (new ID, FirstSeen = now); when a
// row already exists for (identity, subjectType, subject), obs.Upsert
// keeps that row's original ID/FirstSeen and replaces only
// Kind/Confidence/Evidence/UpdatedAt — see
// internal/ports/storage/observations.go.
func (u *Updater) upsertWeakness(ctx context.Context, identity learner.IdentityID, subjectType learnermodel.SubjectType, subject string, count int) error {
	now := u.clock()
	confidence := float64(count) / confidenceDivisor
	if confidence > 1 {
		confidence = 1
	}
	return u.obs.Upsert(ctx, learnermodel.Observation{
		ID:          uuid.NewString(),
		IdentityID:  identity,
		Kind:        learnermodel.KindWeakness,
		SubjectType: subjectType,
		Subject:     subject,
		Confidence:  confidence,
		Evidence:    map[string]any{"count": count, "window_days": 30},
		FirstSeen:   now,
		UpdatedAt:   now,
	})
}

// sweep flips every existing weakness for identity whose subject has
// had zero qualifying occurrences in the trailing quietWindow (anchored
// at now) to emerging. Confidence and Evidence are carried over
// unchanged — the sweep is a read on recency, not a re-detection.
func (u *Updater) sweep(ctx context.Context, identity learner.IdentityID, all []event.LearningEvent, now time.Time) error {
	existing, err := u.obs.List(ctx, identity)
	if err != nil {
		return err
	}
	for _, o := range existing {
		if o.Kind != learnermodel.KindWeakness {
			continue
		}
		evType := eventTypeFor(o.SubjectType)
		if countOccurrences(all, evType, o.SubjectType, o.Subject, now, quietWindow) > 0 {
			continue
		}
		updated := o
		updated.Kind = learnermodel.KindEmerging
		updated.UpdatedAt = now
		if err := u.obs.Upsert(ctx, updated); err != nil {
			return err
		}
	}
	return nil
}

// classify reports the (SubjectType, Subject) an event contributes to,
// or ok=false for an event type/shape this consumer doesn't react to.
func classify(ev event.LearningEvent) (subjectType learnermodel.SubjectType, subject string, ok bool) {
	switch ev.Type {
	case event.TypeCorrectionPresented:
		t, _ := ev.Evidence["type"].(string)
		if t == "" {
			return "", "", false
		}
		return learnermodel.SubjectCorrectionType, t, true
	case event.TypeGrammarConceptEncountered:
		if ev.Subject == "" {
			return "", "", false
		}
		return learnermodel.SubjectConcept, ev.Subject, true
	default:
		return "", "", false
	}
}

// eventTypeFor is classify's inverse for the subject-type half: which
// event type carries occurrences of a given kind of subject. Used by
// sweep, which starts from a stored Observation (SubjectType, Subject)
// rather than a live event.
func eventTypeFor(st learnermodel.SubjectType) event.Type {
	if st == learnermodel.SubjectConcept {
		return event.TypeGrammarConceptEncountered
	}
	return event.TypeCorrectionPresented
}

// qualifyingSeverity reports whether sev counts toward weakness
// detection: only "incorrect" and "unnatural" do — "style" and any
// other severity never contribute, however often they occur.
func qualifyingSeverity(sev string) bool {
	return sev == "incorrect" || sev == "unnatural"
}

// dedupKey is the identity of "one occurrence" for counting purposes.
// Per the controller ruling, occurrences are counted as DISTINCT
// evidence.correction_id values per subject, not raw event count, so a
// retried publish of the same underlying correction never inflates a
// count:
//   - correction.presented: Subject IS the correction ID, so it's
//     already the dedup key.
//   - grammar.concept.encountered: Evidence["correction_id"] names the
//     correction that produced it.
func dedupKey(ev event.LearningEvent) string {
	if ev.Type == event.TypeCorrectionPresented {
		return ev.Subject
	}
	if cid, ok := ev.Evidence["correction_id"].(string); ok && cid != "" {
		return cid
	}
	// Malformed evidence (should never happen for events this consumer
	// produces): fall back to the event's own ID so it still counts as
	// exactly one occurrence rather than being silently dropped.
	return ev.ID
}

// countOccurrences returns the number of distinct (per dedupKey)
// qualifying occurrences of (subjectType, subject) among evType events
// in all, within window trailing back from now (inclusive, and never
// counting events after now — see NewUpdater's doc comment on why "now"
// is always the triggering event's OccurredAt, never wall-clock time).
func countOccurrences(all []event.LearningEvent, evType event.Type, subjectType learnermodel.SubjectType, subject string, now time.Time, window time.Duration) int {
	seen := map[string]struct{}{}
	for _, e := range all {
		if e.Type != evType {
			continue
		}
		st, subj, ok := classify(e)
		if !ok || st != subjectType || subj != subject {
			continue
		}
		sev, _ := e.Evidence["severity"].(string)
		if !qualifyingSeverity(sev) {
			continue
		}
		if e.OccurredAt.After(now) || now.Sub(e.OccurredAt) > window {
			continue
		}
		seen[dedupKey(e)] = struct{}{}
	}
	return len(seen)
}
