// Package exercise holds the drill engine's core types (PRD §17.2,
// §58): a generated Exercise and the Evaluation of a learner's response
// to one.
//
// This is DOMAIN, not application — deliberately, to dodge an import
// cycle: internal/agent/drill (Rule 3: agents import ports/ai + prompts
// + schemas + domain ONLY, never application) must be able to return an
// Exercise/Evaluation from Generate/Evaluate, and
// internal/application/practice (the drill pipeline: authorization,
// persistence, events) must import internal/agent/drill to call it. If
// Exercise/Evaluation lived in application/practice instead, drill would
// have to import practice for the return types while practice imports
// drill for the agent itself — a cycle. Living in domain/exercise, both
// sides import downward, exactly as internal/domain/correction already
// does for internal/agent/teacher and internal/application/feedback.
package exercise

import (
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// Exercise is one generated drill (PRD §17.2): a single targeted
// question for ConceptSlug, in one of four shapes (see Type's values
// below). ID/CreatedAt are assigned by application/practice.Service.
// Start at persistence time, not by the agent that generates the
// content — internal/agent/drill.Agent.Generate returns an Exercise with
// both left zero.
type Exercise struct {
	ID         string
	IdentityID learner.IdentityID
	// SessionID is nil for every exercise today: practice rounds are not
	// scoped to a writing session (application/practice.Service.Start
	// takes only an identity). The column/field exist for a future
	// session-scoped drill mode.
	SessionID *session.ID
	// ConceptSlug is the grammar.Concept.Slug this exercise drills. Empty
	// for a word drill, which has no concept.
	ConceptSlug string
	// SubjectType/SubjectRef name what this exercise drills:
	// "concept" + a grammar.Concept.Slug, or "word" + a
	// vocabulary.Item.ID.
	//
	// ConceptSlug is left meaning exactly what it always meant rather
	// than being widened to hold a word id, so every existing reader is
	// unaffected. Rows written before this pair existed unmarshal with
	// both empty and are normalised on read to ("concept", ConceptSlug).
	SubjectType string
	SubjectRef  string
	// Type is one of the Type* constants below. The four AI-generated
	// shapes must stay in sync with schemas/defs/exercise.v1.json's own
	// enum, which is their single source of truth; TypeWordRecall is
	// deliberately NOT in that enum, because no model generates it — see
	// its constant below.
	Type string
	// InstructionsJA/InstructionsEN are the learner-facing task
	// description ("choose the correct past tense form"), not the
	// exercise Prompt itself.
	InstructionsJA, InstructionsEN string
	// Prompt is the exercise text itself. For TypeFillInBlank it
	// contains "___" marking the blank to fill.
	Prompt string
	// Choices is set (len >= 2) only for TypeMultipleChoice; nil
	// otherwise.
	Choices []string
	// Answer is the canonical correct response. Set for every type
	// except TypeFreeProduction, which has no single correct answer and
	// is scored by internal/agent/drill.Agent.Evaluate instead.
	Answer string
	// Definition is the word's meaning, for a word drill. Carried
	// separately from Acceptable because a cloze with choices has no
	// acceptable alternatives to hide it in, and the answer card shows
	// it either way: getting a word right without recalling what it
	// means is not knowing the word.
	Definition string
	// Example is the sentence the learner met this word in, for a word
	// drill — kept whole here even when the prompt shows it blanked, so
	// the intact sentence can be shown once the answer is in.
	//
	// Empty for concept drills and for words with no recorded example.
	Example string
	// Acceptable holds extra correct answers besides Answer (e.g.
	// orthographic variants) — application/practice.Service.Answer's
	// deterministic check accepts a rune-equal match against ANY of
	// Answer or Acceptable, not Answer alone.
	Acceptable []string
	CreatedAt  time.Time
}

// The four exercise shapes schemas/defs/exercise.v1.json's "type" enum
// allows — kept as named constants so callers never hand-type the
// string literal in more than one place.
const (
	TypeFillInBlank    = "fill-in-blank"
	TypeMultipleChoice = "multiple-choice"
	TypeTransformation = "transformation"
	TypeFreeProduction = "free-production"
	// TypeWordRecall is a flip card over one vocabulary item: the
	// expression on the front, its reading and meaning on the back,
	// graded by the learner.
	//
	// Built directly from the stored item, with NO model call — the
	// reading and the meaning are already known, so asking a model to
	// restate them would add latency, cost and a chance of being wrong
	// about the learner's own vocabulary. That is what makes a word drill
	// cheap enough to do daily, and why this type is absent from
	// exercise.v1.json: nothing generates it.
	TypeWordRecall = "word-recall"
	// TypeWordCloze is the learner's OWN example sentence with the target
	// word blanked out — the sentence they met the word in, from their
	// reading, not one invented for the drill.
	//
	// Like TypeWordRecall it is built from stored data with no model
	// call, and is absent from exercise.v1.json for the same reason. It
	// is preferred over a flip card when an example exists: producing the
	// word in its own context is a stronger test than recognising it
	// alone, and the context is the part that makes it stick.
	TypeWordCloze = "word-cloze"
	// TypePassageChoice is a short generated passage that reuses several
	// words the learner is revising, followed by a multiple-choice
	// question about what it CONVEYS.
	//
	// Model-generated, unlike the other two word shapes, because writing
	// natural prose that uses a given set of words is exactly what a
	// model is for. It is absent from exercise.v1.json because it has its
	// own schema (passage.v1) — a passage and a question are not the same
	// shape as a single-concept drill.
	TypePassageChoice = "passage-choice"
)

// Subject types for SubjectType.
const (
	SubjectConcept = "concept"
	SubjectWord    = "word"
)

// Self-grade tokens. A TypeWordRecall response carries one of these
// instead of typed text: a flip card is graded by the learner, because
// only they know whether they actually recalled it before turning the
// card over. Anything else counts as SelfGradeAgain — an unrecognised
// token is not a reason to credit a recall that may not have happened.
const (
	SelfGradeKnew  = "knew"
	SelfGradeAgain = "again"
)

// Evaluation is the result of grading a learner's response to an
// Exercise — either computed deterministically (a typed answer compared
// against Answer/Acceptable) or by internal/agent/drill.Agent.Evaluate
// (TypeFreeProduction, which has no single correct string to compare
// against). Score is 0-100, holistic for free production; deterministic
// paths use 100/0.
type Evaluation struct {
	Correct                bool
	Score                  int
	FeedbackJA, FeedbackEN string
}
