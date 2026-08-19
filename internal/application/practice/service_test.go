package practice_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	"github.com/mikeyaustin/jlp/internal/agent/drill"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	apppractice "github.com/mikeyaustin/jlp/internal/application/practice"
	appretrieval "github.com/mikeyaustin/jlp/internal/application/retrieval"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const testIdentity = learner.IdentityID("learner-a")

// --- in-memory fakes, mirroring application/feedback/service_test.go's
// established "real collaborators, fake edges" shape. ---

// fakeExerciseRepo is an in-memory storage.ExerciseRepository:
// identity-scoped Get, same miss semantics as the real postgres adapter.
type fakeExerciseRepo struct {
	byID      map[string]exercise.Exercise
	attempts  []storage.ExerciseAttempt
	createErr error
	recordErr error
}

func newFakeExerciseRepo() *fakeExerciseRepo {
	return &fakeExerciseRepo{byID: map[string]exercise.Exercise{}}
}

func (f *fakeExerciseRepo) Create(_ context.Context, ex exercise.Exercise) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.byID[ex.ID] = ex
	return nil
}

func (f *fakeExerciseRepo) Get(_ context.Context, identity learner.IdentityID, id string) (exercise.Exercise, error) {
	ex, ok := f.byID[id]
	if !ok || ex.IdentityID != identity {
		return exercise.Exercise{}, storage.ErrNotFound
	}
	return ex, nil
}

func (f *fakeExerciseRepo) RecordAttempt(_ context.Context, at storage.ExerciseAttempt) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.attempts = append(f.attempts, at)
	return nil
}

// fakeGrammarRepo is an in-memory storage.GrammarRepository: Start
// exercises both ListConcepts (the random-fallback path) and
// GetConcept (via planner.Planner.TopConcept) — every other method
// panics if called.
type fakeGrammarRepo struct {
	concepts []grammar.Concept
	bySlug   map[string]grammar.Concept
}

func (f *fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by practice service tests")
}

func (f *fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	return f.concepts, nil
}

func (f *fakeGrammarRepo) GetConcept(_ context.Context, slug string) (grammar.Concept, error) {
	c, ok := f.bySlug[slug]
	if !ok {
		return grammar.Concept{}, storage.ErrNotFound
	}
	return c, nil
}

func (f *fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by practice service tests")
}

func (f *fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by practice service tests")
}

// fakePriorityRepo is an in-memory storage.PriorityRepository double:
// a test sets top directly, mirroring feedback/service_test.go's own
// fakePriorityRepo.
type fakePriorityRepo struct {
	top []storage.Priority
}

func (f *fakePriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by practice service tests")
}

func (f *fakePriorityRepo) Top(_ context.Context, _ learner.IdentityID, limit int) ([]storage.Priority, error) {
	out := f.top
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// fakeObsRepo/fakeEventStore/fakeVocabRepo exist only to satisfy
// planner.NewPlanner's signature — TopConcept touches neither obs,
// events, nor vocab, so every method panics if actually called, same
// "needed only for the constructor" shape feedback/service_test.go's
// own fakeObsRepo uses.
type fakeObsRepo struct{}

func (f *fakeObsRepo) Upsert(context.Context, learnermodel.Observation) error {
	panic("not used by practice service tests")
}
func (f *fakeObsRepo) List(context.Context, learner.IdentityID) ([]learnermodel.Observation, error) {
	panic("not used by practice service tests")
}
func (f *fakeObsRepo) DeleteAll(context.Context, learner.IdentityID) error {
	panic("not used by practice service tests")
}

type fakeEventStore struct{}

func (f *fakeEventStore) Append(context.Context, event.LearningEvent) error {
	panic("not used by practice service tests")
}
func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	panic("not used by practice service tests")
}
func (f *fakeEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	panic("not used by practice service tests")
}

type fakeVocabRepo struct {
	recent     []vocabulary.Item
	askedSince time.Time
	askedLimit int
}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used by practice service tests")
}
func (f *fakeVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	panic("not used by practice service tests")
}
func (f *fakeVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabulary.Item, error) {
	panic("not used by practice service tests")
}
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	panic("not used by practice service tests")
}
func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	panic("not used by practice service tests")
}
func (f *fakeVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by practice service tests")
}

func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used by practice service tests")
}
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used by practice service tests")
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	return nil
}

// --- test harness ---

var iAdjectivePastConcept = grammar.Concept{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5}

type testHarness struct {
	svc     *apppractice.Service
	repo    *fakeExerciseRepo
	grammar *fakeGrammarRepo
	prios   storage.PriorityRepository
	events  *fakeCapturingEventStore
}

// fakeCapturingEventStore is a storage.LearningEventRepository double
// that actually records every Append call (unlike fakeEventStore above,
// which panics) — wired into the harness's REAL learning.Recorder so
// tests can assert exactly which events Start/Answer recorded.
type fakeCapturingEventStore struct {
	appended []event.LearningEvent
}

func (f *fakeCapturingEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	f.appended = append(f.appended, ev)
	return nil
}
func (f *fakeCapturingEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	panic("not used by practice service tests")
}
func (f *fakeCapturingEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	panic("not used by practice service tests")
}

// newTestHarness wires a real practice.Service over in-memory repos, a
// real drill.Agent over fakeai (deterministic, no network), a real
// learning.Recorder over a capturing event store + a real in-process
// bus, and a real planner.Planner over prios/grammarRepo (TopConcept's
// only two collaborators) — the same "real collaborators, fake edges"
// shape application/feedback/service_test.go uses.
func newTestHarness(gen ai.StructuredGenerator, prios storage.PriorityRepository, grammarRepo *fakeGrammarRepo) *testHarness {
	repo := newFakeExerciseRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, &fakeEventStore{}, grammarRepo, prios, &fakeVocabRepo{}, time.Now)
	agent := drill.New(gen)
	// retrieval is deliberately nil here: dueConcept treats a nil
	// scheduler as "nothing due" (see its own doc comment), so every
	// existing Start test in this file keeps exercising the
	// planner/random-fallback path unchanged. TestStartPrefersDueConcept
	// below builds its own Service directly with a real scheduler
	// instead of going through this harness.
	svc := apppractice.NewService(repo, agent, teachingPlanner, grammarRepo, rec, nil, nil, nil)
	return &testHarness{svc: svc, repo: repo, grammar: grammarRepo, prios: prios, events: events}
}

func defaultHarness() *testHarness {
	return newTestHarness(
		fakeai.New(),
		&fakePriorityRepo{top: []storage.Priority{
			{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5},
		}},
		&fakeGrammarRepo{bySlug: map[string]grammar.Concept{"i-adjective-past": iAdjectivePastConcept}},
	)
}

// --- Start ---

// TestStartUsesPlannerTopConcept pins the brief's Step 2 scenario: the
// planner's top concept (i-adjective-past) drives Generate, the result
// is persisted, and quiz.started records concept/type Evidence.
func TestStartUsesPlannerTopConcept(t *testing.T) {
	h := defaultHarness()

	ex, err := h.svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if ex.ConceptSlug != "i-adjective-past" {
		t.Fatalf("ConceptSlug = %q, want i-adjective-past", ex.ConceptSlug)
	}
	if ex.ID == "" {
		t.Fatal("Start did not assign an ID")
	}
	if ex.CreatedAt.IsZero() {
		t.Fatal("Start did not assign CreatedAt")
	}
	if ex.IdentityID != testIdentity {
		t.Fatalf("IdentityID = %q, want %q", ex.IdentityID, testIdentity)
	}

	persisted, ok := h.repo.byID[ex.ID]
	if !ok {
		t.Fatal("Start did not persist the exercise")
	}
	if persisted.Prompt != ex.Prompt {
		t.Fatalf("persisted.Prompt = %q, want %q", persisted.Prompt, ex.Prompt)
	}

	if len(h.events.appended) != 1 {
		t.Fatalf("appended events = %d, want 1: %+v", len(h.events.appended), h.events.appended)
	}
	ev := h.events.appended[0]
	if ev.Type != event.TypeQuizStarted {
		t.Fatalf("event type = %q, want %q", ev.Type, event.TypeQuizStarted)
	}
	if ev.Subject != ex.ID {
		t.Fatalf("event subject = %q, want %q", ev.Subject, ex.ID)
	}
	if ev.Evidence["concept"] != "i-adjective-past" || ev.Evidence["type"] != ex.Type {
		t.Fatalf("event evidence = %+v, want concept/type matching the exercise", ev.Evidence)
	}
}

// TestStartFallsBackToCatalogWhenNoPriorities pins the other half of
// Step 2: a learner with no concept-type priority (TopConcept's ok=false)
// gets a random catalog concept instead — a single-concept catalog
// fixture keeps the "random" choice deterministic for this test.
func TestStartFallsBackToCatalogWhenNoPriorities(t *testing.T) {
	h := newTestHarness(
		fakeai.New(),
		&fakePriorityRepo{}, // no priorities at all -> TopConcept ok=false
		&fakeGrammarRepo{
			concepts: []grammar.Concept{iAdjectivePastConcept},
			bySlug:   map[string]grammar.Concept{"i-adjective-past": iAdjectivePastConcept},
		},
	)

	ex, err := h.svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if ex.ConceptSlug != "i-adjective-past" {
		t.Fatalf("ConceptSlug = %q, want i-adjective-past (the catalog's only entry)", ex.ConceptSlug)
	}
	if len(h.events.appended) != 1 || h.events.appended[0].Type != event.TypeQuizStarted {
		t.Fatalf("appended events = %+v, want exactly one quiz.started", h.events.appended)
	}
}

// TestStartPropagatesTopConceptError: a planner.TopConcept failure must
// surface, not be swallowed.
func TestStartPropagatesTopConceptError(t *testing.T) {
	h := newTestHarness(fakeai.New(), &fakePriorityRepoErr{err: errBoom}, &fakeGrammarRepo{})

	if _, err := h.svc.Start(context.Background(), testIdentity, ""); err == nil {
		t.Fatal("expected an error when TopConcept fails, got nil")
	}
}

var errBoom = errors.New("boom")

// fakePriorityRepoErr is a storage.PriorityRepository double whose Top
// always fails — used only by TestStartPropagatesTopConceptError.
type fakePriorityRepoErr struct{ err error }

func (f *fakePriorityRepoErr) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used")
}
func (f *fakePriorityRepoErr) Top(context.Context, learner.IdentityID, int) ([]storage.Priority, error) {
	return nil, f.err
}

// --- Answer ---

func startExercise(t *testing.T, h *testHarness) exercise.Exercise {
	t.Helper()
	ex, err := h.svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	return ex
}

// TestAnswerCorrectChoiceNeverCallsAI pins the brief's deterministic
// path: a correct multiple-choice answer is scored without any AI call
// — provable here because the harness's generator (deniedGen) fails
// outright if GenerateStructured is ever invoked again after Start.
func TestAnswerCorrectChoiceNeverCallsAI(t *testing.T) {
	h := defaultHarness()
	ex := startExercise(t, h)

	deny := &denyAfterStartGen{}
	h.svc = apppractice.NewService(h.repo, drill.New(deny), planner.NewPlanner(&fakeObsRepo{}, &fakeEventStore{}, h.grammar, h.prios, &fakeVocabRepo{}, time.Now), h.grammar, learning.NewRecorder(h.events, inprocbus.New()), nil, nil, nil)

	eval, err := h.svc.Answer(context.Background(), testIdentity, ex.ID, ex.Answer, 4)
	if err != nil {
		t.Fatalf("Answer returned error: %v", err)
	}
	if !eval.Correct {
		t.Fatalf("Correct = false, want true (response %q matches Answer %q)", ex.Answer, ex.Answer)
	}
	if deny.called {
		t.Fatal("Answer called the AI generator for a deterministic (multiple-choice) exercise")
	}
}

// denyAfterStartGen is an ai.StructuredGenerator that records whether it
// was ever called — used to prove Answer's deterministic path makes no
// AI call at all.
type denyAfterStartGen struct{ called bool }

func (d *denyAfterStartGen) GenerateStructured(context.Context, ai.StructuredRequest) (ai.StructuredResponse, error) {
	d.called = true
	return ai.StructuredResponse{}, errors.New("deterministic path must not call the AI generator")
}

// TestAnswerWrongChoiceIsEncouragingNotPenalizing pins PRD §56: a wrong
// deterministic answer is still Correct=false, but its feedback copy is
// an encouraging retry invitation, never scolding or streak/penalty
// language.
func TestAnswerWrongChoiceIsEncouragingNotPenalizing(t *testing.T) {
	h := defaultHarness()
	ex := startExercise(t, h)

	wrong := "面白いでした" // the canned MCQ's own wrong distractor
	eval, err := h.svc.Answer(context.Background(), testIdentity, ex.ID, wrong, 0)
	if err != nil {
		t.Fatalf("Answer returned error: %v", err)
	}
	if eval.Correct {
		t.Fatal("Correct = true, want false")
	}
	if eval.FeedbackJA == "" || eval.FeedbackEN == "" {
		t.Fatalf("feedback = %+v, want both JA and EN populated", eval)
	}
	for _, bad := range []string{"失敗", "penalty", "streak", "wrong", "incorrect"} {
		if strings.Contains(eval.FeedbackJA, bad) || strings.Contains(strings.ToLower(eval.FeedbackEN), strings.ToLower(bad)) {
			t.Fatalf("feedback = %+v, must not contain scolding/penalty language %q", eval, bad)
		}
	}
}

// TestAnswerAcceptableListHitCountsAsCorrect pins the "or any
// Acceptable" half of the deterministic match.
func TestAnswerAcceptableListHitCountsAsCorrect(t *testing.T) {
	h := newTestHarness(fakeai.New(), &fakePriorityRepo{}, &fakeGrammarRepo{concepts: []grammar.Concept{iAdjectivePastConcept}, bySlug: map[string]grammar.Concept{"i-adjective-past": iAdjectivePastConcept}})
	ex := startExercise(t, h)
	// Directly seed an exercise with an Acceptable variant, since the
	// canned fakeai MCQ has none — this test is about Answer's own
	// comparison logic, not about what fakeai happens to generate.
	ex.Type = exercise.TypeFillInBlank
	ex.Answer = "面白かったです"
	ex.Acceptable = []string{"おもしろかったです"}
	h.repo.byID[ex.ID] = ex

	eval, err := h.svc.Answer(context.Background(), testIdentity, ex.ID, "  おもしろかったです  ", 0)
	if err != nil {
		t.Fatalf("Answer returned error: %v", err)
	}
	if !eval.Correct {
		t.Fatal("Correct = false, want true (response matches an Acceptable variant, trimmed)")
	}
}

// TestAnswerFreeProductionCallsEvaluate pins the brief's AI-eval path:
// a free-production exercise's Answer calls the Drill agent's Evaluate,
// returning the canned fakeai evaluation.
func TestAnswerFreeProductionCallsEvaluate(t *testing.T) {
	h := newTestHarness(fakeai.New(), &fakePriorityRepo{}, &fakeGrammarRepo{concepts: []grammar.Concept{iAdjectivePastConcept}, bySlug: map[string]grammar.Concept{"i-adjective-past": iAdjectivePastConcept}})
	ex := startExercise(t, h)
	ex.Type = exercise.TypeFreeProduction
	ex.Answer = ""
	h.repo.byID[ex.ID] = ex

	eval, err := h.svc.Answer(context.Background(), testIdentity, ex.ID, "映画はとても面白かったです。", 0)
	if err != nil {
		t.Fatalf("Answer returned error: %v", err)
	}
	if !eval.Correct || eval.Score != 85 {
		t.Fatalf("eval = %+v, want the canned fakeai evaluation (correct, score 85)", eval)
	}
}

// TestAnswerRecordsQuizAnsweredAndCompleted pins the events contract:
// both quiz.answered and quiz.completed fire, each carrying
// concept/type/correct/confidence Evidence.
func TestAnswerRecordsQuizAnsweredAndCompleted(t *testing.T) {
	h := defaultHarness()
	ex := startExercise(t, h)
	h.events.appended = nil // discard the quiz.started event from Start

	if _, err := h.svc.Answer(context.Background(), testIdentity, ex.ID, ex.Answer, 4); err != nil {
		t.Fatalf("Answer returned error: %v", err)
	}

	if len(h.events.appended) != 2 {
		t.Fatalf("appended events = %d, want 2: %+v", len(h.events.appended), h.events.appended)
	}
	wantTypes := []event.Type{event.TypeQuizAnswered, event.TypeQuizCompleted}
	for i, want := range wantTypes {
		ev := h.events.appended[i]
		if ev.Type != want {
			t.Fatalf("appended[%d].Type = %q, want %q", i, ev.Type, want)
		}
		if ev.Evidence["concept"] != ex.ConceptSlug {
			t.Fatalf("appended[%d].Evidence[concept] = %v, want %q", i, ev.Evidence["concept"], ex.ConceptSlug)
		}
		if ev.Evidence["correct"] != true {
			t.Fatalf("appended[%d].Evidence[correct] = %v, want true", i, ev.Evidence["correct"])
		}
		if ev.Evidence["confidence"] != 4 {
			t.Fatalf("appended[%d].Evidence[confidence] = %v, want 4", i, ev.Evidence["confidence"])
		}
	}
}

// TestAnswerConfidenceZeroMeansNotGiven pins the brief's confidence
// sentinel: 0 records with a nil Confidence, not a literal 0.
func TestAnswerConfidenceZeroMeansNotGiven(t *testing.T) {
	h := defaultHarness()
	ex := startExercise(t, h)

	if _, err := h.svc.Answer(context.Background(), testIdentity, ex.ID, ex.Answer, 0); err != nil {
		t.Fatalf("Answer returned error: %v", err)
	}
	if len(h.repo.attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(h.repo.attempts))
	}
	if h.repo.attempts[0].Confidence != nil {
		t.Fatalf("Confidence = %v, want nil (0 means not given)", *h.repo.attempts[0].Confidence)
	}
}

// TestAnswerInvalidConfidenceErrors pins the brief's validation: 0 is
// valid (not given), 1..5 is valid, anything else is ErrInvalidConfidence.
func TestAnswerInvalidConfidenceErrors(t *testing.T) {
	h := defaultHarness()
	ex := startExercise(t, h)

	for _, bad := range []int{-1, 6, 100} {
		if _, err := h.svc.Answer(context.Background(), testIdentity, ex.ID, ex.Answer, bad); !errors.Is(err, apppractice.ErrInvalidConfidence) {
			t.Fatalf("Answer(confidence=%d) err = %v, want ErrInvalidConfidence", bad, err)
		}
	}
}

// TestAnswerCrossIdentityMisses pins the brief's cross-identity
// contract: an exercise ID that belongs to a different identity misses
// with storage.ErrNotFound, exactly like every other identity-scoped
// lookup in this codebase.
func TestAnswerCrossIdentityMisses(t *testing.T) {
	h := defaultHarness()
	ex := startExercise(t, h)

	_, err := h.svc.Answer(context.Background(), learner.IdentityID("someone-else"), ex.ID, ex.Answer, 0)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Answer across identities err = %v, want storage.ErrNotFound", err)
	}
}

// TestAnswerUnknownExerciseIDMisses pins the plain not-found case.
func TestAnswerUnknownExerciseIDMisses(t *testing.T) {
	h := defaultHarness()
	_, err := h.svc.Answer(context.Background(), testIdentity, "does-not-exist", "x", 0)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Answer unknown id err = %v, want storage.ErrNotFound", err)
	}
}

// --- Start / spaced retrieval (PRD §54) ---

// fakeRetrievalRepoForPractice is a minimal storage.RetrievalRepository
// double: only Due is exercised by practice.Service.dueConcept (via
// retrieval.Scheduler.DueSubjects) — every other method panics, the
// same "only what this package uses" convention fakeGrammarRepo etc.
// above follow.
type fakeRetrievalRepoForPractice struct {
	due []storage.RetrievalItem
}

func (f *fakeRetrievalRepoForPractice) Upsert(context.Context, storage.RetrievalItem) error {
	panic("not used by practice service tests")
}
func (f *fakeRetrievalRepoForPractice) Get(context.Context, learner.IdentityID, string, string) (storage.RetrievalItem, error) {
	panic("not used by practice service tests")
}

// Due truncates to limit (mirroring the real postgres adapter's
// LIMIT — see storage.RetrievalRepository.Due's doc comment): a fake
// that ignored limit would make dueSubjectsScanLimit's actual value
// untestable, since dueConcept's own loop never re-truncates what Due
// returns.
func (f *fakeRetrievalRepoForPractice) Due(_ context.Context, _ learner.IdentityID, _ time.Time, limit int) ([]storage.RetrievalItem, error) {
	out := f.due
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeRetrievalRepoForPractice) List(context.Context, learner.IdentityID, int) ([]storage.RetrievalItem, error) {
	panic("not used by practice service tests")
}

// TestStartPrefersDueConceptOverPlannerTopConcept pins the brief's
// ordering: "practice picks a due subject before falling back to the
// planner's top priority" — a concept due for spaced review wins even
// though the planner has its own (different) top concept.
func TestStartPrefersDueConceptOverPlannerTopConcept(t *testing.T) {
	teFormConcept := grammar.Concept{Slug: "te-form", Name: "て-form", JLPTLevel: 5}
	grammarRepo := &fakeGrammarRepo{
		bySlug: map[string]grammar.Concept{
			"te-form":          teFormConcept,
			"i-adjective-past": iAdjectivePastConcept,
		},
	}
	prios := &fakePriorityRepo{top: []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5},
	}}
	retrievalRepo := &fakeRetrievalRepoForPractice{due: []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "te-form"},
	}}
	sched := appretrieval.NewScheduler(retrievalRepo, time.Now)

	repo := newFakeExerciseRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, &fakeEventStore{}, grammarRepo, prios, &fakeVocabRepo{}, time.Now)
	agent := drill.New(fakeai.New())
	svc := apppractice.NewService(repo, agent, teachingPlanner, grammarRepo, rec, sched, nil, nil)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if ex.ConceptSlug != "te-form" {
		t.Fatalf("ConceptSlug = %q, want the DUE concept %q, not the planner's top concept", ex.ConceptSlug, "te-form")
	}
}

// TestStartFallsBackToPlannerWhenNothingDueIsConceptType pins that a
// due queue containing ONLY expression-type items (vocabulary due for
// review, not a grammar concept) does not block the planner's own top
// concept from driving Start.
func TestStartFallsBackToPlannerWhenNothingDueIsConceptType(t *testing.T) {
	grammarRepo := &fakeGrammarRepo{bySlug: map[string]grammar.Concept{"i-adjective-past": iAdjectivePastConcept}}
	prios := &fakePriorityRepo{top: []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5},
	}}
	retrievalRepo := &fakeRetrievalRepoForPractice{due: []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "積もる"},
	}}
	sched := appretrieval.NewScheduler(retrievalRepo, time.Now)

	repo := newFakeExerciseRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, &fakeEventStore{}, grammarRepo, prios, &fakeVocabRepo{}, time.Now)
	agent := drill.New(fakeai.New())
	svc := apppractice.NewService(repo, agent, teachingPlanner, grammarRepo, rec, sched, nil, nil)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if ex.ConceptSlug != "i-adjective-past" {
		t.Fatalf("ConceptSlug = %q, want the planner's top concept %q (only an expression was due)", ex.ConceptSlug, "i-adjective-past")
	}
}

// TestStartFallsBackToPlannerWhenDueConceptSlugIsStale pins the
// stale-slug tolerance dueConcept documents: a due concept subject the
// catalog no longer resolves is skipped, not treated as a hard error.
func TestStartFallsBackToPlannerWhenDueConceptSlugIsStale(t *testing.T) {
	grammarRepo := &fakeGrammarRepo{bySlug: map[string]grammar.Concept{"i-adjective-past": iAdjectivePastConcept}}
	prios := &fakePriorityRepo{top: []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5},
	}}
	retrievalRepo := &fakeRetrievalRepoForPractice{due: []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "stale-slug-no-longer-in-catalog"},
	}}
	sched := appretrieval.NewScheduler(retrievalRepo, time.Now)

	repo := newFakeExerciseRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, &fakeEventStore{}, grammarRepo, prios, &fakeVocabRepo{}, time.Now)
	agent := drill.New(fakeai.New())
	svc := apppractice.NewService(repo, agent, teachingPlanner, grammarRepo, rec, sched, nil, nil)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if ex.ConceptSlug != "i-adjective-past" {
		t.Fatalf("ConceptSlug = %q, want the planner's top concept %q (stale due slug must be skipped, not fatal)", ex.ConceptSlug, "i-adjective-past")
	}
}

// TestStartFindsDueConceptBehindLeadingExpressionItems pins
// dueSubjectsScanLimit's actual value (not just its existence): the due
// queue's four MOST-due items are all expression-type (vocabulary due
// for review), with the concept only 5th — dueConcept must still find
// it. A scan capped at 1 (the pre-fix value) would see only the first
// expression item, conclude nothing concept-type is due, and fall back
// to the planner instead — this test fails under that reversion.
func TestStartFindsDueConceptBehindLeadingExpressionItems(t *testing.T) {
	grammarRepo := &fakeGrammarRepo{bySlug: map[string]grammar.Concept{"te-form": {Slug: "te-form", Name: "て-form", JLPTLevel: 5}, "i-adjective-past": iAdjectivePastConcept}}
	prios := &fakePriorityRepo{top: []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5},
	}}
	retrievalRepo := &fakeRetrievalRepoForPractice{due: []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "expr-1"},
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "expr-2"},
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "expr-3"},
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "expr-4"},
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "te-form"},
	}}
	sched := appretrieval.NewScheduler(retrievalRepo, time.Now)

	repo := newFakeExerciseRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, &fakeEventStore{}, grammarRepo, prios, &fakeVocabRepo{}, time.Now)
	agent := drill.New(fakeai.New())
	svc := apppractice.NewService(repo, agent, teachingPlanner, grammarRepo, rec, sched, nil, nil)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if ex.ConceptSlug != "te-form" {
		t.Fatalf("ConceptSlug = %q, want the DUE concept %q (5th in the queue, behind 4 due expressions) — the planner's top concept must NOT win here", ex.ConceptSlug, "te-form")
	}
}

// ListPage delegates to List and truncates. This package does not page;
// implemented rather than stubbed so it returns real rows if it ever
// starts. See storage.VocabularyRepository.ListPage.
func (f *fakeVocabRepo) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
	all, err := f.List(ctx, identity, filter)
	if err != nil || len(all) <= limit {
		return all, storage.VocabularyCursor{}, err
	}
	page := all[:limit]
	last := page[len(page)-1]
	return page, storage.VocabularyCursor{LastEvent: last.LastEvent, ID: last.ID}, nil
}

// ListRecentUnpracticed IS exercised here — it is the first branch of
// Start's selection order. recent is what the query would have matched;
// the fake records the cutoff it was asked for so a test can assert the
// window rather than trusting it.
func (r *fakeVocabRepo) ListRecentUnpracticed(_ context.Context, _ learner.IdentityID, addedSince time.Time, limit int) ([]vocabulary.Item, error) {
	r.askedSince = addedSince
	r.askedLimit = limit
	return r.recent, nil
}

// ---------------------------------------------------------------------
// Selection order: a recent word outranks everything, including the
// spaced scheduler. See Service.Start's doc comment for why.
// ---------------------------------------------------------------------

// wordSelectionHarness builds a Service whose every OTHER source would
// happily supply a concept, so a test that gets a word back has proved
// the word won on order rather than by being the only option.
func wordSelectionHarness(t *testing.T, recent []vocabulary.Item, now time.Time) (*apppractice.Service, *fakeVocabRepo, *fakeExerciseRepo) {
	t.Helper()
	repo := newFakeExerciseRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	grammarRepo := &fakeGrammarRepo{
		concepts: []grammar.Concept{iAdjectivePastConcept},
		bySlug:   map[string]grammar.Concept{iAdjectivePastConcept.Slug: iAdjectivePastConcept},
	}
	vocab := &fakeVocabRepo{recent: recent}
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, events, grammarRepo, &fakePriorityRepo{}, &fakeVocabRepo{}, func() time.Time { return now })
	svc := apppractice.NewService(repo, drill.New(fakeai.New()), teachingPlanner, grammarRepo, rec, nil, vocab,
		func() time.Time { return now })
	return svc, vocab, repo
}

func recentItem(id, expr, reading, meaning string) vocabulary.Item {
	return vocabulary.Item{ID: id, Expression: expr, Reading: reading, Meaning: meaning}
}

func TestARecentWordIsDrilledBeforeAnythingElse(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	svc, _, _ := wordSelectionHarness(t, []vocabulary.Item{
		recentItem("w1", "紛らわしい", "まぎらわしい", "confusing, easily mixed up"),
	}, now)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ex.SubjectType != exercise.SubjectWord {
		t.Fatalf("subject type = %q, want %q — the concept path won despite a fresh word being available",
			ex.SubjectType, exercise.SubjectWord)
	}
	if ex.Type != exercise.TypeWordRecall {
		t.Errorf("type = %q, want %q", ex.Type, exercise.TypeWordRecall)
	}
	if ex.SubjectRef != "w1" {
		t.Errorf("subject ref = %q, want the vocabulary item id", ex.SubjectRef)
	}
	if ex.Prompt != "紛らわしい" {
		t.Errorf("prompt = %q, want the expression on the front of the card", ex.Prompt)
	}
	if ex.Answer != "まぎらわしい" {
		t.Errorf("answer = %q, want the stored reading", ex.Answer)
	}
}

// The window is the whole reason recency can lead: without it, one old
// word blocks the scheduler forever.
func TestTheRecentWordQueryIsBoundedToTheWindow(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	svc, vocab, _ := wordSelectionHarness(t, nil, now)

	if _, err := svc.Start(context.Background(), testIdentity, ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	want := now.Add(-14 * 24 * time.Hour)
	if !vocab.askedSince.Equal(want) {
		t.Errorf("asked for words added since %s, want %s (a 14-day window)", vocab.askedSince, want)
	}
	if vocab.askedLimit <= 0 {
		t.Errorf("asked for limit %d — an unbounded scan over a bulk-imported vocabulary", vocab.askedLimit)
	}
}

// With nothing fresh, the scheduler and planner get their turn exactly
// as before. This is what stops the word branch from being a takeover.
func TestNoRecentWordFallsThroughToTheConceptPath(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	svc, _, _ := wordSelectionHarness(t, nil, now)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ex.SubjectType != exercise.SubjectConcept {
		t.Fatalf("subject type = %q, want %q", ex.SubjectType, exercise.SubjectConcept)
	}
	if ex.SubjectRef == "" {
		t.Error("a concept drill recorded no subject ref")
	}
}

// A card whose back is blank teaches nothing, so the word is skipped
// rather than shown. Pinning it because the obvious implementation —
// take items[0] — shows it.
func TestAWordWithNothingOnTheBackIsSkipped(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	svc, _, _ := wordSelectionHarness(t, []vocabulary.Item{
		recentItem("empty", "謎", "", ""),
		recentItem("w2", "紛らわしい", "まぎらわしい", "confusing"),
	}, now)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ex.SubjectRef != "w2" {
		t.Errorf("drilled %q, want w2 — a card with no reading and no meaning reveals nothing", ex.SubjectRef)
	}
}

// A caller that has not wired vocabulary must behave exactly as before
// rather than failing, the same nil-tolerance retrieval already has.
func TestAServiceWithNoVocabularyStillDrillsConcepts(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	repo := newFakeExerciseRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	grammarRepo := &fakeGrammarRepo{
		concepts: []grammar.Concept{iAdjectivePastConcept},
		bySlug:   map[string]grammar.Concept{iAdjectivePastConcept.Slug: iAdjectivePastConcept},
	}
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, events, grammarRepo, &fakePriorityRepo{}, &fakeVocabRepo{}, func() time.Time { return now })
	svc := apppractice.NewService(repo, drill.New(fakeai.New()), teachingPlanner, grammarRepo, rec, nil, nil, nil)

	ex, err := svc.Start(context.Background(), testIdentity, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ex.SubjectType != exercise.SubjectConcept {
		t.Errorf("subject type = %q, want %q", ex.SubjectType, exercise.SubjectConcept)
	}
}
