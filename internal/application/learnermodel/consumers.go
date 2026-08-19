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
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
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
	// recomputeDebounce is how long HandleEvent waits, after the LAST
	// classifiable event for an identity, before actually recomputing
	// that identity's priority list. A whole feedback round publishes
	// several events in a tight burst (feedback.requested,
	// correction.presented per correction, grammar.concept.encountered
	// per resolved concept — see application/feedback.Service), each
	// synchronously through the bus on the SAME request goroutine; a
	// naive "recompute after every event" would run the planner's full
	// List+ListAll+ReplaceAll (with a GetConcept call per weakness) once
	// per event, ON the request path, with cost growing with the
	// identity's total history. Debouncing coalesces a whole burst into
	// exactly one background Recompute. See armRecompute.
	recomputeDebounce = 250 * time.Millisecond
)

// Updater is the bus-facing side of the learner model: NewUpdater's
// result is subscribed to correction.presented and
// grammar.concept.encountered via HandleEvent (an events.Handler).
type Updater struct {
	events  storage.LearningEventRepository
	obs     storage.ObservationRepository
	clock   func() time.Time
	planner *planner.Planner

	// schedule implements armRecompute's per-identity debounce: a call
	// schedule(key, d, fire) arms (or, for a key already armed,
	// re-arms/coalesces) a timer that invokes fire once, after d has
	// elapsed since the LAST call for that key. Injected so tests can
	// control firing deterministically instead of racing a real
	// 250ms timer; production uses newTimerSchedule's
	// time.AfterFunc-based implementation, installed by NewUpdater.
	schedule func(key string, d time.Duration, fire func())

	// recomputeLocks serializes Recompute calls per identity: a debounce
	// coalesces a BURST of events into one fire, but nothing stops two
	// separate bursts, close enough together, from having their fires
	// overlap in time — this ensures two goroutines can never interleave
	// one identity's Recompute (and thus its ReplaceAll transaction).
	recomputeLocks sync.Map // learner.IdentityID -> *sync.Mutex
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
	return &Updater{events: events, obs: obs, clock: clock, schedule: newTimerSchedule()}
}

// SetPlanner wires p into u: from this call on, every HandleEvent that
// classifies (see classify) arms a DEBOUNCED background Recompute for
// the event's identity (see armRecompute) — HandleEvent itself never
// blocks on it — keeping the priority list (and thus the Teacher
// agent's RecentErrors, via storage.PriorityRepository.Top) eventually
// in sync with the learner model as it changes. Optional — main.go's
// live bus consumer calls this once at startup; Rebuild's own internal
// Updater (see rebuild.go) deliberately never calls it, recomputing
// once, synchronously, at the end of a full replay instead of once per
// historical event. A nil receiver-side u.planner (the zero value)
// makes HandleEvent's arm step a no-op, so tests that don't care about
// priorities need not call this at all.
func (u *Updater) SetPlanner(p *planner.Planner) {
	u.planner = p
}

// SetSchedule overrides the default production debounce scheduler
// (time.AfterFunc-based, see newTimerSchedule) — for tests that need
// deterministic control over when a debounced Recompute actually
// fires, instead of racing real wall-clock timers. See armRecompute's
// doc comment for the contract schedule must satisfy.
func (u *Updater) SetSchedule(schedule func(key string, d time.Duration, fire func())) {
	u.schedule = schedule
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
//  3. if SetPlanner has wired a planner, ARMS a debounced background
//     Recompute for ev.IdentityID — see armRecompute. HandleEvent
//     itself returns immediately; it never blocks on Recompute.
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

	count := countOccurrences(all, subjectType, subject, ev.OccurredAt, weaknessWindow)
	if count >= weaknessThreshold {
		if err := u.upsertWeakness(ctx, ev.IdentityID, subjectType, subject, count); err != nil {
			return err
		}
	}

	if err := u.sweep(ctx, ev.IdentityID, all, ev.OccurredAt); err != nil {
		return err
	}

	u.armRecompute(ev.IdentityID)
	return nil
}

// armRecompute is the controller-ruled fix keeping the planner OFF the
// request path (a synchronous Recompute per event previously meant a
// 3-correction feedback round triggered ~6 sequential full recomputes
// — List+ListAll+a GetConcept per weakness+ReplaceAll each — before the
// HTTP response returned, with cost growing with total history). It's
// a no-op when no planner is wired (see SetPlanner); otherwise it arms
// u.schedule for identity with a recomputeDebounce delay: repeated arms
// for the SAME identity within that window coalesce into exactly one
// eventual runRecompute call, off HandleEvent's own call stack.
//
// Fire-and-forget: a process exit before a pending timer fires simply
// leaves that identity's priority list one recompute stale — the next
// qualifying event re-arms it, and `jlp rebuild-model` (which recomputes
// synchronously, unconditionally, at the end of every identity's replay
// — see rebuild.go) always recovers it. This is the same "derived,
// rebuildable from the event log" contract the rest of the learner
// model already has (PRD §14): the priority list is a cache of the
// observations, not new information of its own.
func (u *Updater) armRecompute(identity learner.IdentityID) {
	if u.planner == nil {
		return
	}
	u.schedule(string(identity), recomputeDebounce, func() {
		u.runRecompute(identity)
	})
}

// runRecompute performs one identity's debounced Recompute. It's
// serialized against any other in-flight Recompute for the SAME
// identity via a per-identity mutex (recomputeLocks) — two overlapping
// fires must never interleave their planner.Recompute calls, which
// would mean interleaving the underlying ReplaceAll transactions.
// Errors are logged, never panicked: this runs on a background
// goroutine (production: inside a time.AfterFunc callback) with no
// caller left to hand an error back to.
func (u *Updater) runRecompute(identity learner.IdentityID) {
	lockAny, _ := u.recomputeLocks.LoadOrStore(identity, &sync.Mutex{})
	lock, _ := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if err := u.planner.Recompute(context.Background(), identity); err != nil {
		slog.Error("learnermodel: recompute priorities", "identity", identity, "err", err)
	}
}

// newTimerSchedule is the production schedule implementation NewUpdater
// installs by default: one time.AfterFunc-backed timer per key, kept in
// a map guarded by a mutex. The first arm for a key creates the timer;
// every later arm for the SAME key — while it's still pending, or even
// after it has already fired once — calls Reset, which for an
// AfterFunc timer either pushes the pending fire out by d, or (if the
// timer had already fired) restarts it to fire again after d. Either
// way, a burst of arms for one key collapses to exactly one fire per
// quiet period, which is the whole point of debouncing HandleEvent's
// recompute trigger.
func newTimerSchedule() func(key string, d time.Duration, fire func()) {
	var mu sync.Mutex
	timers := map[string]*time.Timer{}
	return func(key string, d time.Duration, fire func()) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := timers[key]; ok {
			t.Reset(d)
			return
		}
		timers[key] = time.AfterFunc(d, fire)
	}
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
		if countOccurrences(all, o.SubjectType, o.Subject, now, quietWindow) > 0 {
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
	case event.TypeQuizAnswered:
		// A drilled CONCEPT contributes to that concept, exactly as an
		// encountered one does — getting the same grammar wrong in
		// practice is the same evidence as getting it wrong in writing,
		// and 練習 was previously invisible to this entirely.
		//
		// A drilled WORD contributes too, as its own subject type: a word
		// the learner keeps failing is a weakness in exactly the sense
		// this package means, and leaving it out made 練習's word half
		// invisible to the planner.
		ref, _ := ev.Evidence["subject_ref"].(string)
		if ref == "" {
			return "", "", false
		}
		switch st, _ := ev.Evidence["subject_type"].(string); st {
		case exercise.SubjectConcept:
			return learnermodel.SubjectConcept, ref, true
		case exercise.SubjectWord:
			return learnermodel.SubjectWord, ref, true
		}
		return "", "", false
	default:
		return "", "", false
	}
}

// qualifies reports whether ev counts toward weakness detection.
//
// What "counts" means depends on where the evidence came from. A
// correction counts when it was serious enough to matter: only
// "incorrect" and "unnatural" do — "style" never contributes, however
// often it occurs. A drill counts when it was answered WRONG; a correct
// answer is evidence of the opposite and must never accumulate toward a
// weakness.
func qualifies(ev event.LearningEvent) bool {
	if ev.Type == event.TypeQuizAnswered {
		correct, _ := ev.Evidence["correct"].(bool)
		return !correct
	}
	sev, _ := ev.Evidence["severity"].(string)
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
	if ev.Type == event.TypeQuizAnswered {
		// The exercise id: one drill is one occurrence, and answering the
		// same exercise twice is one piece of evidence about the concept,
		// not two.
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
// qualifying occurrences of (subjectType, subject) in all, within window
// trailing back from now.
//
// Counted by SUBJECT, not by event type. It used to take an evType and
// filter on it, which quietly assumed one subject type had exactly one
// source of evidence — true until 練習 started contributing concept
// failures. Under that assumption sweep would have retired a weakness
// kept alive only by drills, because it counted the other event type and
// found nothing. classify already says which subject an event belongs
// to; that is the only filter needed (inclusive, and never
// counting events after now — see NewUpdater's doc comment on why "now"
// is always the triggering event's OccurredAt, never wall-clock time).
func countOccurrences(all []event.LearningEvent, subjectType learnermodel.SubjectType, subject string, now time.Time, window time.Duration) int {
	seen := map[string]struct{}{}
	for _, e := range all {
		st, subj, ok := classify(e)
		if !ok || st != subjectType || subj != subject {
			continue
		}
		if !qualifies(e) {
			continue
		}
		if e.OccurredAt.After(now) || now.Sub(e.OccurredAt) > window {
			continue
		}
		seen[dedupKey(e)] = struct{}{}
	}
	return len(seen)
}
