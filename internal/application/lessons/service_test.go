package lessons_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	agentlesson "github.com/mikeyaustin/jlp/internal/agent/lesson"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const testIdentity = learner.IdentityID("learner-a")

// --- fakes: same "in-memory, identity-scoped, panic on unused
// methods" shape as application/anki/service_test.go. ---

type fakeLessonRepo struct {
	mu           sync.Mutex
	byID         map[string]storage.Lesson
	observations map[string][]storage.LessonObservation // key: lesson ID
	// deleted mirrors the real table's deleted_at column. Every read
	// below honours it, because a fake that ignored soft delete would
	// let the Delete tests pass while the thing they are actually
	// asserting — "it stops coming back from reads" — went untested.
	deleted map[string]time.Time
}

func newFakeLessonRepo() *fakeLessonRepo {
	return &fakeLessonRepo{
		byID:         map[string]storage.Lesson{},
		observations: map[string][]storage.LessonObservation{},
		deleted:      map[string]time.Time{},
	}
}

func (f *fakeLessonRepo) Insert(_ context.Context, l storage.Lesson) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[l.ID] = l
	return nil
}

func (f *fakeLessonRepo) List(_ context.Context, identity learner.IdentityID) ([]storage.Lesson, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storage.Lesson
	for id, l := range f.byID {
		if l.IdentityID != identity {
			continue
		}
		if _, gone := f.deleted[id]; gone {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeLessonRepo) Get(_ context.Context, identity learner.IdentityID, id string) (storage.Lesson, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.byID[id]
	if !ok || l.IdentityID != identity {
		return storage.Lesson{}, storage.ErrNotFound
	}
	if _, gone := f.deleted[id]; gone {
		return storage.Lesson{}, storage.ErrNotFound
	}
	return l, nil
}

// CompleteWithObservation mirrors the real postgres repo's atomic
// contract: the observation attach and the status flip happen together
// under the same lock, so from any other goroutine's perspective they
// are indivisible — either both are visible or neither is. An
// observation aimed at a lesson that doesn't exist, or belongs to a
// different identity, misses with ErrNotFound and mutates nothing.
func (f *fakeLessonRepo) CompleteWithObservation(_ context.Context, identity learner.IdentityID, lessonID string, o storage.LessonObservation, at time.Time) (storage.Lesson, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.byID[lessonID]
	if !ok || l.IdentityID != identity {
		return storage.Lesson{}, storage.ErrNotFound
	}
	// A deleted lesson is invisible to reads, so it must not be
	// completable either — the same test CompleteLesson applies in SQL.
	if _, gone := f.deleted[lessonID]; gone {
		return storage.Lesson{}, storage.ErrNotFound
	}
	l.Status = "completed"
	l.CompletedAt = at
	f.byID[lessonID] = l
	f.observations[lessonID] = append(f.observations[lessonID], o)
	return l, nil
}

func (f *fakeLessonRepo) Observations(_ context.Context, _ learner.IdentityID, lessonID string) ([]storage.LessonObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, gone := f.deleted[lessonID]; gone {
		return nil, nil
	}
	return f.observations[lessonID], nil
}

// SoftDelete/Restore mirror the real adapter's contract exactly:
// identity-scoped, idempotent, and ErrNotFound for both "unknown id"
// and "someone else's id" — the property the cross-identity tests in
// softdelete_test.go rely on.
func (f *fakeLessonRepo) SoftDelete(_ context.Context, identity learner.IdentityID, lessonID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.byID[lessonID]
	if !ok || l.IdentityID != identity {
		return storage.ErrNotFound
	}
	if _, already := f.deleted[lessonID]; !already {
		f.deleted[lessonID] = at
	}
	return nil
}

func (f *fakeLessonRepo) Restore(_ context.Context, identity learner.IdentityID, lessonID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.byID[lessonID]
	if !ok || l.IdentityID != identity {
		return storage.ErrNotFound
	}
	delete(f.deleted, lessonID)
	return nil
}

// isDeleted / exists let a test assert on the raw storage state rather
// than on what a read returns — the difference between "the delete was
// refused" and "the row was quietly removed anyway".
func (f *fakeLessonRepo) isDeleted(lessonID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, gone := f.deleted[lessonID]
	return gone
}

func (f *fakeLessonRepo) exists(lessonID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.byID[lessonID]
	return ok
}

// fakePriorityRepo is a minimal storage.PriorityRepository double:
// Generate only ever calls Top.
type fakePriorityRepo struct {
	top      []storage.Priority
	topCalls []int
}

func (f *fakePriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by lessons service tests")
}

func (f *fakePriorityRepo) Top(_ context.Context, _ learner.IdentityID, limit int) ([]storage.Priority, error) {
	f.topCalls = append(f.topCalls, limit)
	return f.top, nil
}

// fakeObservationRepo is a minimal storage.ObservationRepository
// double: Generate only ever calls List.
type fakeObservationRepo struct {
	list []learnermodel.Observation
}

func (f *fakeObservationRepo) Upsert(context.Context, learnermodel.Observation) error {
	panic("not used by lessons service tests")
}

func (f *fakeObservationRepo) List(context.Context, learner.IdentityID) ([]learnermodel.Observation, error) {
	return f.list, nil
}

func (f *fakeObservationRepo) DeleteAll(context.Context, learner.IdentityID) error {
	panic("not used by lessons service tests")
}

// fakeFeedbackRepo is a minimal storage.FeedbackRepository double:
// Generate only ever calls RecentCorrections.
type fakeFeedbackRepo struct {
	recent []storage.CorrectionRecord
}

func (f *fakeFeedbackRepo) InsertFeedback(context.Context, storage.FeedbackRecord, []storage.CorrectionRecord, map[string][]storage.ConceptTag) error {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) UpdateCorrectionStatus(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) GetCorrectionConcepts(context.Context, string) ([]string, error) {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) RetryCorrection(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) RevealCorrection(context.Context, learner.IdentityID, string) (storage.CorrectionRecord, error) {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) RecordConfidence(context.Context, learner.IdentityID, string, int) (storage.CorrectionRecord, error) {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) GetCorrection(context.Context, learner.IdentityID, string) (storage.CorrectionRecord, error) {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) RecentCorrections(_ context.Context, _ learner.IdentityID, limit int) ([]storage.CorrectionRecord, error) {
	if limit > 0 && len(f.recent) > limit {
		return f.recent[:limit], nil
	}
	return f.recent, nil
}
func (f *fakeFeedbackRepo) ListForSession(context.Context, learner.IdentityID, session.ID) ([]storage.FeedbackSummary, error) {
	panic("not used by lessons service tests")
}
func (f *fakeFeedbackRepo) GetFeedback(context.Context, learner.IdentityID, string) (storage.FeedbackDetail, []storage.CorrectionRecord, error) {
	panic("not used by lessons service tests")
}

// fakeVocabRepo backs the *planner.Planner's ActivationCandidates —
// mirrors planner_test.go's own fake.
type fakeVocabRepo struct {
	items []vocabulary.Item
}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used by lessons service tests")
}
func (f *fakeVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	panic("not used by lessons service tests")
}
func (f *fakeVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabulary.Item, error) {
	panic("not used by lessons service tests")
}
func (f *fakeVocabRepo) ListActivationCandidates(_ context.Context, _ learner.IdentityID, limit int) ([]vocabulary.Item, error) {
	if limit > 0 && len(f.items) > limit {
		return f.items[:limit], nil
	}
	return f.items, nil
}
func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	panic("not used by lessons service tests")
}
func (f *fakeVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by lessons service tests")
}

func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used by lessons service tests")
}
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used by lessons service tests")
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to
// satisfy the port.
func (f *fakeVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	panic("not used by lessons service tests")
}
func (f *fakeVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	panic("not used by lessons service tests")
}

// fakeGrammarRepo/fakeEventRepo satisfy planner.NewPlanner's
// constructor but are never actually exercised by Generate (which only
// calls ActivationCandidates, a thin passthrough to vocab).
type fakeGrammarRepo struct{}

func (fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by lessons service tests")
}
func (fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	panic("not used by lessons service tests")
}
func (fakeGrammarRepo) GetConcept(context.Context, string) (grammar.Concept, error) {
	panic("not used by lessons service tests")
}
func (fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by lessons service tests")
}
func (fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by lessons service tests")
}

type fakeEventRepo struct{}

func (fakeEventRepo) Append(context.Context, event.LearningEvent) error {
	panic("not used by lessons service tests")
}
func (fakeEventRepo) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	panic("not used by lessons service tests")
}
func (fakeEventRepo) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	panic("not used by lessons service tests")
}

// fakeCapturingEventStore actually records every Append call — mirrors
// application/anki/service_test.go's double of the same name.
type fakeCapturingEventStore struct {
	appended  []event.LearningEvent
	appendErr error
}

func (f *fakeCapturingEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appended = append(f.appended, ev)
	return nil
}
func (f *fakeCapturingEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	panic("not used by lessons service tests")
}
func (f *fakeCapturingEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	panic("not used by lessons service tests")
}

type testHarness struct {
	svc      *applessons.Service
	lessons  *fakeLessonRepo
	prios    *fakePriorityRepo
	obs      *fakeObservationRepo
	feedback *fakeFeedbackRepo
	vocab    *fakeVocabRepo
	events   *fakeCapturingEventStore
}

func newTestHarness() *testHarness {
	return newTestHarnessWithGenerator(fakeai.New())
}

// newTestHarnessWithGenerator is newTestHarness with the lesson agent's
// underlying ai.StructuredGenerator swappable — TestGenerateFilters
// GatedCorrectionsFromLessonContext wires in a spyGen (below) so it can
// inspect exactly what RecentCorrections text reached the prompt,
// rather than relying on fakeai's fixed canned response (which doesn't
// echo its input back).
func newTestHarnessWithGenerator(gen ai.StructuredGenerator) *testHarness {
	lessonRepo := newFakeLessonRepo()
	prios := &fakePriorityRepo{}
	obs := &fakeObservationRepo{}
	feedback := &fakeFeedbackRepo{}
	vocab := &fakeVocabRepo{}
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	plnr := planner.NewPlanner(obs, fakeEventRepo{}, fakeGrammarRepo{}, prios, vocab, time.Now)
	agent := agentlesson.New(gen)
	svc := applessons.NewService(lessonRepo, prios, plnr, feedback, obs, agent, rec)
	return &testHarness{svc: svc, lessons: lessonRepo, prios: prios, obs: obs, feedback: feedback, vocab: vocab, events: events}
}

// spyGen is a local ai.StructuredGenerator test double that records the
// last ai.StructuredRequest it was called with and answers with a fixed,
// schema-valid lesson_plan.v1 payload — mirrors
// internal/agent/lesson/lesson_test.go's own spyGen.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	return ai.StructuredResponse{
		JSON: []byte(`{"level_summary":"s","strengths":["a"],"weaknesses":["b"],"focus":["c"],` +
			`"vocabulary":["d"],"grammar_concepts":["e"],"conversation_prompts":["f"],` +
			`"exercises":["g"],"recent_examples":["h"],"questions_for_tutor":["i"]}`),
		Provider: "spy",
		Model:    "spy-1",
	}, nil
}

// --- Generate ---

// TestGenerateGathersContextAndPersistsPreparedLesson pins the brief's
// Step 1 scenario: Top(8) priorities, ActivationCandidates(8),
// RecentCorrections(8), and every observation are all fetched (captured
// via the stub repos above) and formatted into the agent's
// GenerateInput; the fakeai-generated plan is persisted Status
// "prepared", and tutor.lesson.created fires.
func TestGenerateGathersContextAndPersistsPreparedLesson(t *testing.T) {
	h := newTestHarness()
	h.prios.top = []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 2.5, Reason: "recurring weakness: 5 occurrences in 30d"},
	}
	h.vocab.items = []vocabulary.Item{
		{Expression: "それはそれとして", Reading: "それはそれとして", Meaning: "話題を切り替える際に使う表現", Lookups: 3},
	}
	h.feedback.recent = []storage.CorrectionRecord{
		{ID: "corr-1", Original: "面白いでした", Replacement: "面白かったです", Type: "conjugation"},
	}
	h.obs.list = []learnermodel.Observation{
		{IdentityID: testIdentity, Kind: learnermodel.KindWeakness, SubjectType: learnermodel.SubjectConcept, Subject: "i-adjective-past", Confidence: 0.8},
	}

	lesson, err := h.svc.Generate(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if lesson.Status != "prepared" {
		t.Fatalf("Status = %q, want prepared", lesson.Status)
	}
	if lesson.IdentityID != testIdentity {
		t.Fatalf("IdentityID = %q, want %q", lesson.IdentityID, testIdentity)
	}
	if lesson.ID == "" {
		t.Fatal("Generate did not assign an ID")
	}
	if lesson.CreatedAt.IsZero() {
		t.Fatal("Generate did not assign CreatedAt")
	}
	if !lesson.CompletedAt.IsZero() {
		t.Fatal("CompletedAt should be zero for a freshly-prepared lesson")
	}
	if !strings.Contains(string(lesson.Plan), "i-adjective-past") {
		t.Fatalf("Plan missing the canned i-adjective-past content: %s", lesson.Plan)
	}

	// Context was actually gathered at the capped limits the brief
	// specifies.
	if len(h.prios.topCalls) != 1 || h.prios.topCalls[0] != 8 {
		t.Fatalf("Top calls = %v, want exactly one call with limit 8", h.prios.topCalls)
	}

	persisted, ok := h.lessons.byID[lesson.ID]
	if !ok {
		t.Fatal("Generate did not persist the lesson")
	}
	if string(persisted.Plan) != string(lesson.Plan) {
		t.Fatalf("persisted.Plan = %s, want %s", persisted.Plan, lesson.Plan)
	}

	if len(h.events.appended) != 1 {
		t.Fatalf("appended events = %d, want 1: %+v", len(h.events.appended), h.events.appended)
	}
	ev := h.events.appended[0]
	if ev.Type != event.TypeTutorLessonCreated {
		t.Fatalf("event type = %q, want %q", ev.Type, event.TypeTutorLessonCreated)
	}
	if ev.Subject != lesson.ID {
		t.Fatalf("event subject = %q, want %q", ev.Subject, lesson.ID)
	}
}

// TestGenerateEmptyContextStillSucceeds pins the brand-new-identity
// path: no priorities/vocabulary/corrections/observations yet still
// produces a valid lesson (fakeai's canned plan doesn't depend on the
// prompt's content).
func TestGenerateEmptyContextStillSucceeds(t *testing.T) {
	h := newTestHarness()
	lesson, err := h.svc.Generate(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if lesson.Status != "prepared" {
		t.Fatalf("Status = %q, want prepared", lesson.Status)
	}
}

// TestGenerateSurvivesEventRecordFailure pins the log-and-continue
// contract: the lesson is already persisted by the time the event
// record call runs, so a Recorder failure must not turn a successful
// Generate into a reported error.
func TestGenerateSurvivesEventRecordFailure(t *testing.T) {
	h := newTestHarness()
	h.events.appendErr = errors.New("event store unavailable")

	lesson, err := h.svc.Generate(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("Generate returned error: %v, want nil (event-record failures must be logged, not surfaced)", err)
	}
	if _, ok := h.lessons.byID[lesson.ID]; !ok {
		t.Fatal("Generate did not persist the lesson despite the event-record failure")
	}
}

// TestGenerateFiltersGatedCorrectionsFromLessonContext pins the
// final-review fix (Finding 2): RecentCorrections is fed to the lesson
// agent unfiltered before this fix, and formatCorrections' "original →
// replacement" line spells out the answer to a still-gated socratic
// correction — visible to the learner on the very next /lessons/{id}
// guide. A tutor guide loses nothing by skipping a correction the
// learner hasn't resolved or revealed yet, so Generate must drop any
// storage.CorrectionRecord for which IsGated() is true (the exact PRD
// §9/§53 predicate the HTML/JSON gate and application/anki's own fix
// share) before formatting, leaving the ungated ones untouched.
//
// This asserts on the captured agent input (via spyGen's req.User, the
// rendered prompt — the only place GenerateInput's formatted strings are
// observable from outside internal/agent/lesson), not merely on the
// persisted/rendered plan: fakeai's canned lesson_plan.v1 response
// doesn't echo its input, so a rendered-page-only assertion couldn't
// catch a regression here.
func TestGenerateFiltersGatedCorrectionsFromLessonContext(t *testing.T) {
	gen := &spyGen{}
	h := newTestHarnessWithGenerator(gen)
	h.feedback.recent = []storage.CorrectionRecord{
		{
			ID: "corr-gated", Original: "面白いでした", Replacement: "面白かったです", Type: "conjugation",
			Status: "presented", HintJA: "ヒント：形容詞の活用を確認してください。", HintEN: "Hint: check the adjective conjugation.",
			Revealed: false,
		},
		{
			ID: "corr-ungated", Original: "食べる", Replacement: "食べます", Type: "conjugation",
			Status: "accepted",
		},
	}

	if _, err := h.svc.Generate(context.Background(), testIdentity); err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	if strings.Contains(gen.req.User, "面白いでした → 面白かったです") {
		t.Fatalf("prompt leaked the gated correction's answer: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "食べる → 食べます") {
		t.Fatalf("prompt missing the ungated correction: %s", gen.req.User)
	}
}

// --- Complete ---

func preparedLesson(t *testing.T, h *testHarness) storage.Lesson {
	t.Helper()
	l, err := h.svc.Generate(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	return l
}

// TestCompleteRecordsObservationAndCompletesLesson pins the brief's
// completion flow: notes 「助詞の復習が必要」, subject i-adjective-past →
// status completed, observation recorded, tutor.lesson.completed fires
// with Evidence.subjects.
func TestCompleteRecordsObservationAndCompletesLesson(t *testing.T) {
	h := newTestHarness()
	l := preparedLesson(t, h)
	h.events.appended = nil // discard the tutor.lesson.created event

	completed, err := h.svc.Complete(context.Background(), testIdentity, l.ID, "tutor-a", "助詞の復習が必要", []string{"i-adjective-past"})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if completed.Status != "completed" {
		t.Fatalf("Status = %q, want completed", completed.Status)
	}
	if completed.CompletedAt.IsZero() {
		t.Fatal("CompletedAt is zero, want it set")
	}

	obsList, err := h.lessons.Observations(context.Background(), testIdentity, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obsList) != 1 {
		t.Fatalf("Observations = %d, want 1", len(obsList))
	}
	if obsList[0].Author != "tutor-a" || obsList[0].Notes != "助詞の復習が必要" {
		t.Fatalf("obsList[0] = %+v, want Author=tutor-a Notes=助詞の復習が必要", obsList[0])
	}
	if len(obsList[0].Subjects) != 1 || obsList[0].Subjects[0] != "i-adjective-past" {
		t.Fatalf("Subjects = %+v, want [i-adjective-past]", obsList[0].Subjects)
	}

	if len(h.events.appended) != 1 {
		t.Fatalf("appended events = %d, want 1: %+v", len(h.events.appended), h.events.appended)
	}
	ev := h.events.appended[0]
	if ev.Type != event.TypeTutorLessonCompleted {
		t.Fatalf("event type = %q, want %q", ev.Type, event.TypeTutorLessonCompleted)
	}
	if ev.Subject != l.ID {
		t.Fatalf("event subject = %q, want %q", ev.Subject, l.ID)
	}
	subjects, ok := ev.Evidence["subjects"].([]string)
	if !ok || len(subjects) != 1 || subjects[0] != "i-adjective-past" {
		t.Fatalf("event evidence subjects = %+v, want [i-adjective-past]", ev.Evidence["subjects"])
	}
}

// TestCompleteCrossIdentityMisses pins the brief's cross-identity
// contract: an observation aimed at another identity's lesson must not
// attach, and Complete must not flip that lesson's status either.
func TestCompleteCrossIdentityMisses(t *testing.T) {
	h := newTestHarness()
	l := preparedLesson(t, h)

	_, err := h.svc.Complete(context.Background(), learner.IdentityID("someone-else"), l.ID, "tutor-x", "should not attach", nil)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}

	// Neither the observation nor the status change actually happened.
	stillPrepared := h.lessons.byID[l.ID]
	if stillPrepared.Status != "prepared" {
		t.Fatalf("Status = %q, want still prepared (cross-identity Complete must not mutate it)", stillPrepared.Status)
	}
	obsList, err := h.lessons.Observations(context.Background(), testIdentity, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obsList) != 0 {
		t.Fatalf("Observations = %+v, want empty", obsList)
	}
}

// TestCompleteUnknownLessonMisses pins the plain not-found case.
func TestCompleteUnknownLessonMisses(t *testing.T) {
	h := newTestHarness()
	_, err := h.svc.Complete(context.Background(), testIdentity, "does-not-exist", "tutor-a", "n", nil)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
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

// ListRecentUnpracticed is 練習's word-drill source; no test in this
// package drills words, so reaching it means a wiring mistake.
func (r *fakeVocabRepo) ListRecentUnpracticed(context.Context, learner.IdentityID, time.Time, int) ([]vocabulary.Item, error) {
	panic("not used by these tests")
}

// GetByIDs resolves due words for 練習; the practice double below is the
// only one that needs real behaviour.
func (r *fakeVocabRepo) GetByIDs(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by these tests")
}

// LatestExamples backs 練習's cloze drills.
func (r *fakeVocabRepo) LatestExamples(context.Context, learner.IdentityID, []string) (map[string]string, error) {
	panic("not used by these tests")
}

// RecordExample stores a generated example sentence.
func (r *fakeVocabRepo) RecordExample(context.Context, learner.IdentityID, string, string, storage.ExampleOrigin, time.Time) error {
	panic("not used by these tests")
}
