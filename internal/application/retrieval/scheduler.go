// Package retrieval is the application-layer spaced-retrieval scheduler
// (PRD §54): Scheduler.RecordOutcome turns a practice/correction/
// vocabulary outcome into an updated due date via a deterministic
// interval policy, and Scheduler.DueSubjects reads the resulting queue
// back. Consumer (consumer.go) is the event-bus side, wiring outcomes
// in from the events that already exist — quiz.answered,
// correction.retried, vocabulary.produced-correctly — the same
// "constructor takes its collaborators + an injectable clock, HandleEvent
// is an events.Handler" shape application/learnermodel's Updater uses.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// steps is the interval policy's base table (PRD §54), indexed 0..5 by
// the item's current "step" — advanced by success, dropped by failure,
// never negative and never past the last entry. index i's duration is
// how long until the item comes due again after landing on step i.
var steps = []time.Duration{
	24 * time.Hour,      // 1d
	3 * 24 * time.Hour,  // 3d
	7 * 24 * time.Hour,  // 7d
	16 * 24 * time.Hour, // 16d
	35 * 24 * time.Hour, // 35d
	90 * 24 * time.Hour, // 90d
}

var maxStep = len(steps) - 1

// Scheduler implements the spaced-retrieval interval policy (PRD §54).
// It never calls time.Now() itself — clock is injected so every
// RecordOutcome/DueSubjects call, and the tests that pin the interval
// table, are reproducible regardless of wall-clock time.
type Scheduler struct {
	repo  storage.RetrievalRepository
	clock func() time.Time

	// subjectLocks serializes RecordOutcome's read-then-write (Get,
	// compute the next step, Upsert) per (identity, subjectType,
	// subject): two outcomes for the SAME subject that race — e.g. a
	// quiz.answered and a correction.retried event for the same concept
	// dispatched from two concurrent requests — would otherwise both
	// Get the same pre-update state and Upsert from it, silently losing
	// whichever write landed second (its Successes/Failures increment
	// and step advance would be computed from stale data and overwrite
	// the other's). Different subjects never contend: this is a
	// per-key lock, not a single scheduler-wide one, the same
	// per-identity granularity application/learnermodel.Updater's own
	// recomputeLocks uses for its analogous read-then-write race.
	subjectLocks sync.Map // string (subjectLockKey) -> *sync.Mutex
}

// NewScheduler builds a Scheduler over repo. clock is injectable (see
// the package doc comment) — production wiring passes time.Now, tests
// pass a fixed func() time.Time.
func NewScheduler(repo storage.RetrievalRepository, clock func() time.Time) *Scheduler {
	return &Scheduler{repo: repo, clock: clock}
}

// subjectLockKey is subjectLocks' map key for (identity, subjectType,
// subject) — a plain string join is safe here since none of the three
// components can themselves contain the separator in a way that
// creates a collision our tests care about (they're slugs/UUIDs/
// expressions, never attacker-controlled cross-identity input).
func subjectLockKey(identity learner.IdentityID, subjectType, subject string) string {
	return string(identity) + "\x00" + subjectType + "\x00" + subject
}

// lockSubject returns (and lazily creates) the *sync.Mutex guarding
// key, LOCKED — callers must defer Unlock.
func (s *Scheduler) lockSubject(key string) *sync.Mutex {
	lockAny, _ := s.subjectLocks.LoadOrStore(key, &sync.Mutex{})
	lock, _ := lockAny.(*sync.Mutex)
	lock.Lock()
	return lock
}

// RecordOutcome folds one success/failure outcome for
// (identity, subjectType, subject) into its retrieval schedule and
// upserts the result. Serialized per (identity, subjectType, subject)
// via subjectLocks (see that field's doc comment) — its own Get, then
// compute, then Upsert is NOT atomic at the storage layer, so two
// concurrent calls for the SAME subject must never interleave. The
// interval policy (PRD §54 inputs: previous
// success/failure, elapsed time via the base table, production
// frequency via Successes/Failures, confidence, importance left to a
// future task) is entirely deterministic, keyed off the item's CURRENT
// step — reverse-derived from its stored Interval via stepFor, never a
// separately persisted index, so Interval is always exactly one of
// steps' six values and the two can never drift apart:
//
//   - success, confidence NOT 1, 2, or 5: advance one step.
//   - success, confidence 1 or 2 (shaky knowledge): do NOT advance —
//     the item repeats at its CURRENT interval, floored at step 0 for
//     an item with no schedule yet.
//   - success, confidence 5: advance TWO steps.
//   - failure (confidence ignored): drop TWO steps, floored at 0 —
//     never below.
//
// Every branch clamps to [0, maxStep], so a success at the last step
// stays at the last step (never overflows past 90d) and a failure at
// or before step 1 lands at step 0 (never negative).
//
// confidence is 0 (unknown/not given) or 1..5 — the same range
// practice.Service.Answer and feedback.Service.RecordConfidence already
// validate; RecordOutcome does not re-validate it, callers are
// expected to pass through an already-valid value (or 0).
func (s *Scheduler) RecordOutcome(ctx context.Context, identity learner.IdentityID, subjectType, subject string, success bool, confidence int) error {
	lock := s.lockSubject(subjectLockKey(identity, subjectType, subject))
	defer lock.Unlock()

	now := s.clock()

	existing, err := s.repo.Get(ctx, identity, subjectType, subject)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("retrieval: get %s/%s: %w", subjectType, subject, err)
	}

	step := stepFor(existing.Interval)
	successes, failures := existing.Successes, existing.Failures

	if success {
		successes++
		switch confidence {
		case 1, 2:
			step = maxInt(step, 0)
		case 5:
			step += 2
		default:
			step++
		}
	} else {
		failures++
		step -= 2
	}
	step = clamp(step, 0, maxStep)

	interval := steps[step]
	item := storage.RetrievalItem{
		IdentityID:  identity,
		SubjectType: subjectType,
		Subject:     subject,
		Successes:   successes,
		Failures:    failures,
		LastSeen:    now,
		DueAt:       now.Add(interval),
		Interval:    interval,
		Confidence:  float64(confidence),
	}
	if err := s.repo.Upsert(ctx, item); err != nil {
		return fmt.Errorf("retrieval: upsert %s/%s: %w", subjectType, subject, err)
	}
	return nil
}

// DueSubjects returns identity's currently-due items (Scheduler.clock's
// "now"), ordered and capped exactly as storage.RetrievalRepository.Due
// documents — a thin passthrough, since Due's ordering/limit belong in
// SQL the same way storage.PriorityRepository.Top's does.
func (s *Scheduler) DueSubjects(ctx context.Context, identity learner.IdentityID, limit int) ([]storage.RetrievalItem, error) {
	items, err := s.repo.Due(ctx, identity, s.clock(), limit)
	if err != nil {
		return nil, fmt.Errorf("retrieval: due subjects: %w", err)
	}
	return items, nil
}

// stepFor reverse-derives an item's current step index from its stored
// Interval: exact equality against steps, since RecordOutcome is the
// only writer and always stores one of steps' six values verbatim.
// interval == 0 (storage.RetrievalItem's zero value — Get's ErrNotFound
// case, a subject with no schedule yet) returns -1, one below step 0,
// so a first-ever outcome still applies the normal advance/floor rules
// (a first success lands on step 0, a first failure floors at step 0
// too — see RecordOutcome).
func stepFor(interval time.Duration) int {
	for i, d := range steps {
		if d == interval {
			return i
		}
	}
	return -1
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
