// Package practice is the application-layer drill pipeline (PRD §17.2,
// §58): it picks what to drill (the planner's top concept, or a random
// catalog concept as a fallback), asks the Drill agent to generate an
// exercise, persists it, scores a learner's answer — deterministically
// for typed answers, via the Drill agent for free production — and
// records the learning events the rest of the system reacts to.
package practice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/agent/drill"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	appretrieval "github.com/mikeyaustin/jlp/internal/application/retrieval"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// StartOptions is what the caller knows and Start cannot: which adapter
// the learner picked, and whether this slot in their run should skip the
// word queue.
type StartOptions struct {
	// ProviderOverride, when non-empty, names the AI adapter that must
	// generate this drill — the 練習 page's dropdown, for this request
	// only. Passed straight to drill.GenerateInput and never persisted.
	// Empty means "route normally", which is what every non-interactive
	// caller (the channel adapters) wants: nobody is there to pick.
	ProviderOverride string
	// SkipWords passes over the recent-word queue for this drill, so a
	// large vocabulary cannot crowd grammar out entirely. The caller
	// decides the rhythm — see the HTTP layer's conceptEvery — because
	// only it knows where in a run this drill sits.
	SkipWords bool
	// ExcludeSubject is a subject this drill must NOT be about, named by
	// the caller because only it knows what it just served.
	//
	// A word leaves the recent queue when it is ANSWERED, not when it is
	// shown — successful_productions is what the query filters on. So a
	// drill prepared while the previous one is still unanswered sees the
	// same word still queued and picks it again, and the learner gets the
	// same card twice in a row. A single subject is enough: the problem
	// is adjacency, and a word that comes back later because it was
	// answered wrong is the scheduler working, not a bug.
	ExcludeSubject string
	// WantPassage asks for a reading passage over several words the
	// learner is revising, rather than a single-word drill. Honoured only
	// when enough words are actually available (passageWordCount); a run
	// slot that cannot be filled this way falls through to the ordinary
	// order rather than failing.
	WantPassage bool
}

// ErrInvalidConfidence is returned when Answer is asked to record a
// confidence outside the documented 0 (not given) / 1..5 (valid) range.
var ErrInvalidConfidence = errors.New("practice: confidence must be 0 (not given) or between 1 and 5")

// Deterministic-path feedback copy (PRD §56: encouraging, no
// streaks/penalties — a wrong answer is an invitation to try again, not
// a scored failure). Free-production feedback instead comes from the
// Drill agent's own evaluation, whose drill.evaluate.v1 system prompt
// carries the same "teaches, never scolds" instruction.
const (
	correctFeedbackJA = "正解です！"
	correctFeedbackEN = "Correct!"
	wrongFeedbackJA   = "惜しい！もう一度挑戦しましょう。"
	wrongFeedbackEN   = "Not quite — let's try again."

	deterministicCorrectScore = 100
	deterministicWrongScore   = 0
)

// dueSubjectsScanLimit caps how many of the scheduler's most-due items
// Start scans looking for the first "concept" one (see dueConcept):
// DueSubjects can return "expression" items too (vocabulary due for
// review), which Start has no use for, so it looks a little past the
// single most-due item rather than falling back to the planner just
// because the very top of the queue happens to be an expression.
const dueSubjectsScanLimit = 5

// recentWordWindow is how far back a word counts as "recently added"
// and therefore drillable ahead of anything the scheduler has queued.
//
// Two weeks, because that is roughly how long the context that produced
// a word survives — the sentence it came from, why it was looked up.
// Past that it is just another item, and the spaced scheduler is the
// better judge of when to show it.
const recentWordWindow = 14 * 24 * time.Hour

// recentWordScanLimit caps how many recent words Start considers. It
// takes the newest and drills that one; the cap exists so a bulk import
// of 500 words is one bounded query rather than a full scan.
const recentWordScanLimit = 20

// Service wires the drill pipeline. planner and grammar are used ONLY
// by Start, to decide what concept to drill next (see TopConcept and
// randomCatalogConcept below) — Answer never touches them. retrieval is
// also Start-only (see dueConcept): a due subject takes priority over
// the planner's own top concept (PRD §54's queue is meant to actually
// resurface, not just sit unread on /learner).
type Service struct {
	repo      storage.ExerciseRepository
	agent     *drill.Agent
	planner   *planner.Planner
	grammar   storage.GrammarRepository
	rec       *learning.Recorder
	retrieval *appretrieval.Scheduler
	// vocab is Start-only, and nil-tolerant: a caller that has not wired
	// it simply never gets word drills, exactly as a nil retrieval means
	// nothing is ever due.
	vocab storage.VocabularyRepository
	now   func() time.Time
}

// NewService wires the practice pipeline.
func NewService(repo storage.ExerciseRepository, agent *drill.Agent, plnr *planner.Planner, grammarRepo storage.GrammarRepository, rec *learning.Recorder, retrieval *appretrieval.Scheduler, vocab storage.VocabularyRepository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, agent: agent, planner: plnr, grammar: grammarRepo, rec: rec, retrieval: retrieval, vocab: vocab, now: now}
}

// Start generates and persists one new exercise for identity, picking
// what to drill in priority order:
//  1. a recently added word the learner has never produced correctly
//     (recentWord) — newest first. A word added this week still has the
//     context that produced it attached, which is when it is cheapest
//     to learn; the spaced scheduler is the better judge of everything
//     older;
//  2. a concept due for spaced review (dueConcept, PRD §54) —
//     resurfacing something the learner is about to forget beats
//     drilling a fresh weakness;
//  3. the planner's top concept (planner.Planner.TopConcept) when
//     identity has a live grammar weakness and nothing is due;
//  4. a random catalog concept otherwise (a brand-new learner, or one
//     whose top priority isn't concept-type).
//
// Step 1 is the only step that produces a word drill, and it needs no
// model call at all: the reading and meaning are already stored, so the
// exercise is built directly from the item (see wordRecall).
//
// Records quiz.started with Evidence {"concept":…, "type":…}.
func (s *Service) Start(ctx context.Context, identity learner.IdentityID, opts StartOptions) (exercise.Exercise, error) {
	ex, err := s.Build(ctx, identity, opts)
	if err != nil {
		return exercise.Exercise{}, err
	}
	return s.persist(ctx, identity, ex)
}

// Build chooses and generates a drill WITHOUT persisting it or
// recording quiz.started.
//
// Split out so a caller can generate ahead of time — the next question
// while the learner is still answering this one — and only commit it
// when it is actually shown. Persisting at generation time instead would
// leave an exercise row and a "started" event behind for every drill
// prepared and never reached, which is every abandoned run.
//
// See Start for the selection order; this is that whole function minus
// its last step.
func (s *Service) Build(ctx context.Context, identity learner.IdentityID, opts StartOptions) (exercise.Exercise, error) {
	// A recent word short-circuits everything below, including the model
	// call: nothing to generate, so nothing to wait for or pay for.
	//
	// SkipWords is what stops that from becoming a monopoly. A learner
	// who imports 500 words has 500 recent ones, and pure recency would
	// mean 500 word drills before a single grammar question — the
	// ordering the caller asked for, taken to a place nobody wants.
	// A passage reuses SEVERAL words at once, so it is tried before the
	// single-word branches — otherwise the first of those words would be
	// drilled alone and the slot spent.
	if opts.WantPassage && !opts.SkipWords {
		if ex, ok, err := s.passageDrill(ctx, identity, opts.ProviderOverride); err != nil {
			// Not fatal: a passage is the richest shape available, not a
			// required one, and a learner is better served by an ordinary
			// drill than by 練習 refusing to produce anything.
			slog.Warn("practice: passage unavailable, falling back", "identity", identity, "err", err)
		} else if ok {
			return ex, nil
		}
	}

	if item, ok, err := s.recentWord(ctx, identity, opts.ExcludeSubject); opts.SkipWords {
		_ = item
	} else if err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: recent word: %w", err)
	} else if ok {
		return s.persist(ctx, identity, s.drillFor(ctx, identity, item))
	}

	// A due WORD is drilled before a due concept is looked for, because
	// both come from the same queue and the queue is already ordered by
	// how overdue each item is. Skipped under SkipWords for the same
	// reason the recent queue is.
	if !opts.SkipWords {
		if item, ok, err := s.dueWord(ctx, identity, opts.ExcludeSubject); err != nil {
			return exercise.Exercise{}, fmt.Errorf("practice: due word: %w", err)
		} else if ok {
			return s.drillFor(ctx, identity, item), nil
		}
	}

	concept, ok, err := s.dueConcept(ctx, identity)
	if err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: due concept: %w", err)
	}
	if !ok {
		concept, ok, err = s.planner.TopConcept(ctx, identity)
		if err != nil {
			return exercise.Exercise{}, fmt.Errorf("practice: top concept: %w", err)
		}
	}
	if !ok {
		concept, err = s.randomCatalogConcept(ctx)
		if err != nil {
			return exercise.Exercise{}, err
		}
	}

	ex, _, err := s.agent.Generate(ctx, drill.GenerateInput{
		Identity:         identity,
		Concept:          concept,
		ProviderOverride: opts.ProviderOverride,
	})
	if err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: generate exercise: %w", err)
	}
	ex.SubjectType = exercise.SubjectConcept
	ex.SubjectRef = concept.Slug

	return ex, nil
}

// Serve commits a drill produced by Build: it persists the exercise and
// records quiz.started, exactly as Start's last step does.
func (s *Service) Serve(ctx context.Context, identity learner.IdentityID, ex exercise.Exercise) (exercise.Exercise, error) {
	return s.persist(ctx, identity, ex)
}

// persist stamps, stores and announces a freshly chosen exercise —
// shared by the word path and the generated path so the two cannot
// drift on what a started drill records.
func (s *Service) persist(ctx context.Context, identity learner.IdentityID, ex exercise.Exercise) (exercise.Exercise, error) {
	// ID/CreatedAt are assigned here, at persistence time — not by the
	// Drill agent (see drill.Agent.Generate's own doc comment on why).
	ex.ID = uuid.New().String()
	ex.IdentityID = identity
	ex.CreatedAt = s.now().UTC()

	if err := s.repo.Create(ctx, ex); err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: persist exercise: %w", err)
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeQuizStarted,
		Subject:    ex.ID,
		Evidence: map[string]any{
			"concept":      ex.ConceptSlug,
			"type":         ex.Type,
			"subject_type": ex.SubjectType,
			"subject_ref":  ex.SubjectRef,
		},
	}); err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: record %s: %w", event.TypeQuizStarted, err)
	}

	return ex, nil
}

// passageWordCount is how many words a passage revisits. Three is
// enough for a paragraph to have a subject rather than being three
// unrelated clauses, and few enough that a learner meets each one in a
// context they can still hold in mind.
const passageWordCount = 3

// passageDrill builds a reading passage over the words most worth
// revisiting: the recent ones first, then whatever is due.
//
// ok is false — with no error — when there are not enough words to make
// a passage worth writing. That is the common case for a new learner,
// and it is a fall-through, not a failure.
func (s *Service) passageDrill(ctx context.Context, identity learner.IdentityID, providerOverride string) (exercise.Exercise, bool, error) {
	items, err := s.passageCandidates(ctx, identity)
	if err != nil {
		return exercise.Exercise{}, false, err
	}
	if len(items) < passageWordCount {
		return exercise.Exercise{}, false, nil
	}
	items = items[:passageWordCount]

	words := make([]drill.WordRef, 0, len(items))
	for _, item := range items {
		words = append(words, drill.WordRef{
			Expression: item.Expression,
			Reading:    item.Reading,
			Meaning:    item.Meaning,
		})
	}

	ex, _, err := s.agent.GeneratePassage(ctx, drill.PassageInput{
		Identity:         identity,
		Words:            words,
		ProviderOverride: providerOverride,
	})
	if err != nil {
		return exercise.Exercise{}, false, err
	}
	// The subject is the FIRST word: a passage revisits several, but an
	// exercise records one, and the retrieval schedule needs a single
	// subject to move. The others are still met in context, which is the
	// point of the shape — they simply are not what this attempt scores.
	ex.SubjectType = exercise.SubjectWord
	ex.SubjectRef = items[0].ID
	return ex, true, nil
}

// passageCandidates gathers words worth revisiting, recent before due,
// without repeats.
func (s *Service) passageCandidates(ctx context.Context, identity learner.IdentityID) ([]vocabulary.Item, error) {
	if s.vocab == nil {
		return nil, nil
	}
	recent, err := s.vocab.ListRecentUnpracticed(ctx, identity, s.now().UTC().Add(-recentWordWindow), recentWordScanLimit)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(recent))
	out := make([]vocabulary.Item, 0, passageWordCount)
	add := func(item vocabulary.Item) {
		if seen[item.ID] || !hasCardBack(item) {
			return
		}
		seen[item.ID] = true
		out = append(out, item)
	}
	for _, item := range recent {
		add(item)
	}
	if len(out) >= passageWordCount {
		return out, nil
	}

	// Not enough fresh words: top up from the review queue, which is
	// where everything older lives.
	if s.retrieval == nil {
		return out, nil
	}
	due, err := s.retrieval.DueSubjects(ctx, identity, dueSubjectsScanLimit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range due {
		if d.SubjectType == "expression" && !seen[d.Subject] {
			ids = append(ids, d.Subject)
		}
	}
	items, err := s.vocab.GetByIDs(ctx, identity, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]vocabulary.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	for _, id := range ids {
		if item, found := byID[id]; found {
			add(item)
		}
	}
	return out, nil
}

// drillFor chooses the shape of a word's drill, fetching its example
// sentence to decide.
//
// A failed example lookup degrades to a flip card rather than failing
// the drill: the sentence is an enrichment, and no learner is served by
// 練習 refusing to run because one optional query did not answer.
func (s *Service) drillFor(ctx context.Context, identity learner.IdentityID, item vocabulary.Item) exercise.Exercise {
	if s.vocab == nil {
		return wordRecall(item)
	}
	examples, err := s.vocab.LatestExamples(ctx, identity, []string{item.ID})
	if err != nil {
		return wordRecall(item)
	}
	return wordDrill(item, examples[item.ID])
}

// recentWord returns the newest word identity added inside
// recentWordWindow and has never produced correctly.
//
// ok is false — with no error — when vocab is nil (a caller that hasn't
// wired it) or nothing qualifies, exactly as dueConcept reports "nothing
// due".
func (s *Service) recentWord(ctx context.Context, identity learner.IdentityID, exclude string) (vocabulary.Item, bool, error) {
	if s.vocab == nil {
		return vocabulary.Item{}, false, nil
	}
	items, err := s.vocab.ListRecentUnpracticed(ctx, identity, s.now().UTC().Add(-recentWordWindow), recentWordScanLimit)
	if err != nil {
		return vocabulary.Item{}, false, err
	}
	for _, item := range items {
		if item.ID != exclude && hasCardBack(item) {
			return item, true, nil
		}
	}
	return vocabulary.Item{}, false, nil
}

// hasCardBack reports whether item has anything to reveal. A flip card
// that turns over to nothing teaches nothing, so such a word is skipped
// rather than shown — by both the recent queue and the due queue, from
// one definition so the two cannot disagree about what is drillable.
func hasCardBack(item vocabulary.Item) bool {
	return strings.TrimSpace(item.Reading) != "" ||
		strings.TrimSpace(item.Meaning) != "" ||
		strings.TrimSpace(item.MeaningEN) != ""
}

// dueWord returns the most overdue word in identity's review queue.
//
// The queue schedules words as ("expression", vocabulary id) — the type
// handleVocabularyProducedCorrectly has always used and the one a
// drilled word now gets too, so a word has ONE schedule however it was
// practised. Before this, an expression coming due could never be
// drilled: dueConcept skipped everything that was not a concept, so the
// queue displayed words on 学習 that nothing would ever resurface.
//
// ok is false — with no error — when retrieval or vocab is nil, nothing
// is due, or no due expression still resolves to a word worth showing.
func (s *Service) dueWord(ctx context.Context, identity learner.IdentityID, exclude string) (vocabulary.Item, bool, error) {
	if s.retrieval == nil || s.vocab == nil {
		return vocabulary.Item{}, false, nil
	}
	due, err := s.retrieval.DueSubjects(ctx, identity, dueSubjectsScanLimit)
	if err != nil {
		return vocabulary.Item{}, false, err
	}
	// Collected in queue order, then resolved in ONE query — the due list
	// is mixed (concepts are slugs, words are ids) and a lookup per entry
	// would be a round trip per non-match.
	var ids []string
	for _, item := range due {
		if item.SubjectType == "expression" {
			ids = append(ids, item.Subject)
		}
	}
	items, err := s.vocab.GetByIDs(ctx, identity, ids)
	if err != nil {
		return vocabulary.Item{}, false, err
	}
	byID := make(map[string]vocabulary.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	// Walk ids, not items: the queue's order is the whole point, and a
	// map has none. A due word whose row is gone (deleted since it was
	// scheduled) simply falls through, like a stale concept slug does.
	for _, id := range ids {
		if id == exclude {
			continue
		}
		if item, found := byID[id]; found && hasCardBack(item) {
			return item, true, nil
		}
	}
	return vocabulary.Item{}, false, nil
}

// clozeBlank is what replaces the target word in a cloze prompt. Three
// full-width underscores, matching what the drill prompt template
// already uses for TypeFillInBlank so both shapes read the same.
const clozeBlank = "＿＿＿"

// wordDrill builds the best drill available for item: a cloze over the
// learner's own example sentence when there is one containing the word,
// and a flip card otherwise.
//
// Cloze is preferred because producing a word in its own context is a
// stronger test than recognising it alone, and because the context is
// the part that makes it stick. The sentence has to actually CONTAIN
// the expression — an example that mentions the word without using it,
// or one recorded against a different surface form, would blank nothing
// and leave the learner staring at an unmodified sentence.
func wordDrill(item vocabulary.Item, example string) exercise.Exercise {
	example = strings.TrimSpace(example)
	if example != "" && strings.Contains(example, item.Expression) {
		return wordCloze(item, example)
	}
	ex := wordRecall(item)
	// Carried even though it cannot be blanked: a sentence that mentions
	// the word without using it is still context worth reading, and the
	// card's back is exactly where context belongs.
	ex.Example = example
	return ex
}

// wordCloze blanks the target word out of the sentence it came from.
func wordCloze(item vocabulary.Item, example string) exercise.Exercise {
	meaning := strings.TrimSpace(item.Meaning)
	if meaning == "" {
		meaning = strings.TrimSpace(item.MeaningEN)
	}
	instructionsJA := "空欄に入る語を入力してください。"
	if meaning != "" {
		instructionsJA = "空欄に入る語を入力してください。（意味：" + meaning + "）"
	}
	return exercise.Exercise{
		SubjectType:    exercise.SubjectWord,
		SubjectRef:     item.ID,
		Type:           exercise.TypeWordCloze,
		InstructionsJA: instructionsJA,
		InstructionsEN: "Type the word that belongs in the blank.",
		// Replaced everywhere it occurs: leaving a second, unblanked copy
		// in the sentence would hand over the answer.
		Prompt: strings.ReplaceAll(example, item.Expression, clozeBlank),
		// The intact sentence travels alongside the blanked one, so the
		// result can show what it actually said.
		Example: example,
		Answer:  item.Expression,
		// The reading is accepted too. A learner who recalls the word but
		// types it in kana has produced it; failing them on orthography
		// would test something the drill never asked about.
		Acceptable: acceptableForms(item),
	}
}

// acceptableForms is the alternatives a cloze answer may take: the
// reading, when it differs from the expression itself.
func acceptableForms(item vocabulary.Item) []string {
	reading := strings.TrimSpace(item.Reading)
	if reading == "" || reading == item.Expression {
		return nil
	}
	return []string{reading}
}

// wordRecall builds a flip card from a stored vocabulary item.
//
// No model call: the reading and meaning are already known, so asking
// one to restate them would add latency, cost, and a chance of being
// wrong about the learner's own vocabulary. Answer holds the reading so
// the typed-answer path can still grade it deterministically; the card
// itself is self-graded (see Answer's word-recall branch).
func wordRecall(item vocabulary.Item) exercise.Exercise {
	back := strings.TrimSpace(item.Meaning)
	if back == "" {
		back = strings.TrimSpace(item.MeaningEN)
	}
	return exercise.Exercise{
		SubjectType:    exercise.SubjectWord,
		SubjectRef:     item.ID,
		Type:           exercise.TypeWordRecall,
		InstructionsJA: "この語の読みと意味を思い出してください。",
		InstructionsEN: "Recall this word's reading and meaning, then check yourself.",
		Prompt:         item.Expression,
		Answer:         strings.TrimSpace(item.Reading),
		Acceptable:     []string{back},
	}
}

// dueConcept looks for a concept Start should drill because it's due
// for spaced review (PRD §54): the identity's most-due items
// (retrieval.Scheduler.DueSubjects, scanned up to dueSubjectsScanLimit
// deep), returning the first one whose SubjectType is "concept",
// resolved to a grammar.Concept the same way TopConcept resolves its
// own subject. ok is false — with no error — when retrieval is nil (a
// caller that hasn't wired the scheduler, e.g. some existing tests),
// nothing is due, only "expression" items are due, or the due
// concept's slug no longer resolves (storage.ErrNotFound from
// GetConcept — the same stale-slug tolerance TopConcept already has).
// Any other DueSubjects or GetConcept failure propagates as an error.
func (s *Service) dueConcept(ctx context.Context, identity learner.IdentityID) (grammar.Concept, bool, error) {
	if s.retrieval == nil {
		return grammar.Concept{}, false, nil
	}
	due, err := s.retrieval.DueSubjects(ctx, identity, dueSubjectsScanLimit)
	if err != nil {
		return grammar.Concept{}, false, err
	}
	for _, item := range due {
		if item.SubjectType != "concept" {
			continue
		}
		concept, err := s.grammar.GetConcept(ctx, item.Subject)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return grammar.Concept{}, false, fmt.Errorf("get concept %q: %w", item.Subject, err)
		}
		return concept, true, nil
	}
	return grammar.Concept{}, false, nil
}

// randomCatalogConcept picks a uniformly random concept from the full
// grammar catalog — Start's fallback when TopConcept has nothing to
// offer. Using math/rand's global source (not injected) is deliberate:
// unlike planner.Planner's injectable clock, Start's tests pin this path
// with a single-concept catalog fixture (rand.Intn(1) is always 0), so
// there's no need to make the choice itself deterministic to test it.
func (s *Service) randomCatalogConcept(ctx context.Context) (grammar.Concept, error) {
	concepts, err := s.grammar.ListConcepts(ctx)
	if err != nil {
		return grammar.Concept{}, fmt.Errorf("practice: list grammar concepts: %w", err)
	}
	if len(concepts) == 0 {
		return grammar.Concept{}, errors.New("practice: no grammar concepts available to drill")
	}
	return concepts[rand.Intn(len(concepts))], nil //nolint:gosec // not a security-sensitive choice
}

// Answer scores identity's response to a previously generated exercise,
// persists the attempt, and records quiz.answered + quiz.completed
// (Evidence {"concept":…, "type":…, "correct":…, "confidence":…} on
// both — see this package's tests for why they carry the same shape).
//
// exerciseID is looked up via repo.Get, which is identity-scoped: an
// exercise ID that exists but belongs to a different identity misses
// with storage.ErrNotFound, propagated unwrapped so callers can
// errors.Is against it exactly like every other identity-scoped lookup
// in this codebase.
//
// confidence is the learner's optional self-rating: 0 means "not
// given" (never persisted as a literal 0 — see
// storage.ExerciseAttempt.Confidence's *int/nil shape), 1..5 is valid,
// anything else is ErrInvalidConfidence.
// For TypeWordRecall, response carries a self-grade token
// (exercise.SelfGradeKnew / SelfGradeAgain) rather than typed text — see
// selfGradedEvaluation.
func (s *Service) Answer(ctx context.Context, identity learner.IdentityID, exerciseID, response string, confidence int) (exercise.Evaluation, error) {
	if confidence != 0 && (confidence < 1 || confidence > 5) {
		return exercise.Evaluation{}, fmt.Errorf("%w: got %d", ErrInvalidConfidence, confidence)
	}

	ex, err := s.repo.Get(ctx, identity, exerciseID)
	if err != nil {
		return exercise.Evaluation{}, err
	}

	var eval exercise.Evaluation
	switch {
	case ex.Type == exercise.TypeWordRecall:
		eval = selfGradedEvaluation(response)
	case ex.Type == exercise.TypeFreeProduction:
		eval, _, err = s.agent.Evaluate(ctx, identity, ex, response)
		if err != nil {
			return exercise.Evaluation{}, fmt.Errorf("practice: evaluate: %w", err)
		}
	default:
		eval = deterministicEvaluation(response, ex)
	}

	// A word drill has to move the word's own counters, or the drill
	// never ends: recentWord selects on successful_productions = 0, so a
	// word answered correctly but never recorded stays at the front of
	// the queue and every subsequent drill is the same card.
	if ex.SubjectType == exercise.SubjectWord && ex.SubjectRef != "" && s.vocab != nil {
		if err := s.vocab.RecordProduction(ctx, identity, ex.SubjectRef, eval.Correct, s.now().UTC()); err != nil {
			return exercise.Evaluation{}, fmt.Errorf("practice: record production: %w", err)
		}
	}

	var confidencePtr *int
	if confidence != 0 {
		c := confidence
		confidencePtr = &c
	}
	if err := s.repo.RecordAttempt(ctx, storage.ExerciseAttempt{
		ID:         uuid.New().String(),
		ExerciseID: exerciseID,
		Response:   response,
		Correct:    eval.Correct,
		Score:      eval.Score,
		FeedbackJA: eval.FeedbackJA,
		FeedbackEN: eval.FeedbackEN,
		Confidence: confidencePtr,
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		return exercise.Evaluation{}, fmt.Errorf("practice: record attempt: %w", err)
	}

	evidence := map[string]any{
		"concept": ex.ConceptSlug,
		"type":    ex.Type,
		"correct": eval.Correct,
		// subject_type/subject_ref, not just concept: a word drill has no
		// ConceptSlug, so a consumer reading "concept" alone treats every
		// word drill as having no subject and silently does nothing with
		// it. That is what kept drilled words out of the review schedule
		// entirely. "concept" stays for events written before this pair
		// existed — see retrieval's handleQuizAnswered.
		"subject_type": ex.SubjectType,
		"subject_ref":  ex.SubjectRef,
		"confidence":   confidence,
	}
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeQuizAnswered,
		Subject:    exerciseID,
		Evidence:   evidence,
	}); err != nil {
		return exercise.Evaluation{}, fmt.Errorf("practice: record %s: %w", event.TypeQuizAnswered, err)
	}
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeQuizCompleted,
		Subject:    exerciseID,
		Evidence:   evidence,
	}); err != nil {
		return exercise.Evaluation{}, fmt.Errorf("practice: record %s: %w", event.TypeQuizCompleted, err)
	}

	return eval, nil
}

// selfGradedEvaluation grades a flip card from the learner's own verdict.
//
// Only they know whether they recalled it before turning the card over,
// so there is nothing to compare and nothing for a model to judge.
// Anything that is not an explicit "knew" counts as "again": an
// unrecognised token is not a reason to credit a recall that may never
// have happened, and the cost of being wrong in that direction is one
// extra sighting of a word.
func selfGradedEvaluation(response string) exercise.Evaluation {
	if strings.TrimSpace(response) == exercise.SelfGradeKnew {
		return exercise.Evaluation{Correct: true, Score: deterministicCorrectScore, FeedbackJA: correctFeedbackJA, FeedbackEN: correctFeedbackEN}
	}
	return exercise.Evaluation{Correct: false, Score: deterministicWrongScore, FeedbackJA: wrongFeedbackJA, FeedbackEN: wrongFeedbackEN}
}

// Get returns one of identity's exercises. The HTTP layer needs it to
// render an answer's diff against the canonical answer, which the
// Evaluation alone does not carry.
func (s *Service) Get(ctx context.Context, identity learner.IdentityID, exerciseID string) (exercise.Exercise, error) {
	return s.repo.Get(ctx, identity, exerciseID)
}

// deterministicEvaluation implements the brief's deterministic scoring
// path for fill-in-blank/multiple-choice/transformation exercises:
// TrimSpace rune-equal comparison of response against ex.Answer or any
// ex.Acceptable — never the AI. Feedback copy is fixed and encouraging
// either way (PRD §56): no streak language, no penalty language, a
// wrong answer just invites another attempt.
func deterministicEvaluation(response string, ex exercise.Exercise) exercise.Evaluation {
	trimmed := strings.TrimSpace(response)
	correct := trimmed == strings.TrimSpace(ex.Answer)
	if !correct {
		for _, a := range ex.Acceptable {
			if trimmed == strings.TrimSpace(a) {
				correct = true
				break
			}
		}
	}
	if correct {
		return exercise.Evaluation{Correct: true, Score: deterministicCorrectScore, FeedbackJA: correctFeedbackJA, FeedbackEN: correctFeedbackEN}
	}
	return exercise.Evaluation{Correct: false, Score: deterministicWrongScore, FeedbackJA: wrongFeedbackJA, FeedbackEN: wrongFeedbackEN}
}
