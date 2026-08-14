// Package learnermodel holds the deterministic, event-sourced model of
// what a learner is struggling with or improving on. An Observation is
// never produced by hand — it is always the output of folding a
// learner's learning_events stream through the detection rules in
// internal/application/learnermodel (PRD §13/§14/§44): the same event
// stream must always fold to the same observations, whether processed
// live (one event at a time, as it happens) or replayed wholesale by
// `jlp rebuild-model`.
package learnermodel

import (
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// ObservationKind is the learner-model's current read on a subject: a
// standing weakness, or a weakness that has gone quiet long enough to
// be trending better.
type ObservationKind string

const (
	KindWeakness ObservationKind = "weakness"
	// KindEmerging marks a subject that WAS a weakness but has had no
	// qualifying occurrences in the last 14 days — "improving", not
	// "resolved": it flips straight back to KindWeakness the moment the
	// subject reoccurs three times again.
	KindEmerging ObservationKind = "emerging"
)

// SubjectType distinguishes what Observation.Subject names.
type SubjectType string

const (
	// SubjectConcept: Subject is a grammar concept slug (see
	// internal/domain/grammar), e.g. "i-adjective-past".
	SubjectConcept SubjectType = "concept"
	// SubjectCorrectionType: Subject is a correction type string as
	// produced by the teacher agent, e.g. "conjugation".
	SubjectCorrectionType SubjectType = "correction-type"
)

// Observation is one row of the learner model: identity's standing on
// one (SubjectType, Subject) pair. Uniquely keyed by (IdentityID,
// SubjectType, Subject) — there is at most one Observation per subject
// per learner, and later detections replace it in place rather than
// appending a new row.
type Observation struct {
	ID          string
	IdentityID  learner.IdentityID
	Kind        ObservationKind
	SubjectType SubjectType
	Subject     string
	// Confidence is min(1, occurrences/5) at the time the observation
	// was (re)detected as a weakness — how sure the model is, not how
	// severe the problem is.
	Confidence float64
	// Evidence records what produced this observation, e.g.
	// {"count": 3, "window_days": 30} for a weakness detection.
	Evidence  map[string]any
	FirstSeen time.Time
	UpdatedAt time.Time
}
