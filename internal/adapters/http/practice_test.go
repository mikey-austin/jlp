package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	"github.com/mikeyaustin/jlp/internal/agent/drill"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	apppractice "github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// practiceTestConcept is the single catalog entry practiceTestServer's
// grammar double offers — TopConcept always answers ok=false (no
// priorities seeded, see practicePriorityRepo below), so Start's
// random-catalog fallback always lands on this one concept,
// deterministically, without needing to inject a rand source.
var practiceTestConcept = grammar.Concept{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5}

// practiceGrammarRepo is an in-memory storage.GrammarRepository double
// distinct from feedback_test.go's fakeGrammarRepo (that one's
// GetConcept panics unconditionally — unusable for practice.Service.
// Start, which needs both ListConcepts and GetConcept to work).
type practiceGrammarRepo struct {
	concepts []grammar.Concept
	bySlug   map[string]grammar.Concept
}

func (r *practiceGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by practice http tests")
}

func (r *practiceGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	return r.concepts, nil
}

func (r *practiceGrammarRepo) GetConcept(_ context.Context, slug string) (grammar.Concept, error) {
	c, ok := r.bySlug[slug]
	if !ok {
		return grammar.Concept{}, storage.ErrNotFound
	}
	return c, nil
}

func (r *practiceGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by practice http tests")
}

func (r *practiceGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by practice http tests")
}

// practicePriorityRepo is an in-memory storage.PriorityRepository
// double, distinct from feedback_test.go's fakePriorityRepo: Top always
// answers empty, so practice.Service.Start's TopConcept call always
// reports ok=false and falls through to the random-catalog fallback.
type practicePriorityRepo struct{}

func (practicePriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by practice http tests")
}

func (practicePriorityRepo) Top(context.Context, learner.IdentityID, int) ([]storage.Priority, error) {
	return nil, nil
}

// practiceExerciseRepo is an in-memory storage.ExerciseRepository,
// identity-scoped like the real postgres adapter.
type practiceExerciseRepo struct {
	byID     map[string]exercise.Exercise
	attempts []storage.ExerciseAttempt
}

func newPracticeExerciseRepo() *practiceExerciseRepo {
	return &practiceExerciseRepo{byID: map[string]exercise.Exercise{}}
}

func (r *practiceExerciseRepo) Create(_ context.Context, ex exercise.Exercise) error {
	r.byID[ex.ID] = ex
	return nil
}

func (r *practiceExerciseRepo) Get(_ context.Context, identity learner.IdentityID, id string) (exercise.Exercise, error) {
	ex, ok := r.byID[id]
	if !ok || ex.IdentityID != identity {
		return exercise.Exercise{}, storage.ErrNotFound
	}
	return ex, nil
}

func (r *practiceExerciseRepo) RecordAttempt(_ context.Context, at storage.ExerciseAttempt) error {
	r.attempts = append(r.attempts, at)
	return nil
}

// practiceTestServer wires a real chi router with a real
// practice.Service (a real drill.Agent over fakeai — deterministic, no
// network) over in-memory repos, mirroring feedback_test.go's
// feedbackTestServer. It returns the handler plus the exercise repo and
// event repo so tests can inspect persisted state directly.
func practiceTestServer(t *testing.T) (http.Handler, *practiceExerciseRepo, *fakeEventRepo) {
	t.Helper()
	opts := testOptions()
	exerciseRepo := newPracticeExerciseRepo()
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())
	grammarRepo := &practiceGrammarRepo{
		concepts: []grammar.Concept{practiceTestConcept},
		bySlug:   map[string]grammar.Concept{practiceTestConcept.Slug: practiceTestConcept},
	}
	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, grammarRepo, practicePriorityRepo{}, &fakeVocabRepo{}, time.Now)
	opts.Practice = apppractice.NewService(exerciseRepo, drill.New(fakeai.New()), teachingPlanner, grammarRepo, rec, nil, nil, nil)

	srv := NewServer(opts)
	return srv.HandlerForTest(), exerciseRepo, events
}

// extractExerciseID pulls the id out of the first
// `data-exercise-id="XXXX"` attribute in body, the way a browser's own
// form submit (targeting /practice/{id}/answer) would rely on it.
func extractExerciseID(t *testing.T, body string) string {
	t.Helper()
	const marker = `data-exercise-id="`
	idx := strings.Index(body, marker)
	if idx == -1 {
		t.Fatalf("body missing exercise id marker %q: %s", marker, body)
	}
	rest := body[idx+len(marker):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatalf("malformed exercise id attribute: %s", body)
	}
	return rest[:end]
}

// TestPracticeStartRendersMultipleChoiceExercise pins the brief's Step 4
// browser scenario: POST /practice/start renders the canned MCQ with its
// 3 choices.
func TestPracticeStartRendersMultipleChoiceExercise(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/practice/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "昨日の映画はとても＿＿＿。") {
		t.Fatalf("body missing the canned prompt: %s", body)
	}
	for _, choice := range []string{"面白いでした", "面白かったです", "面白いだった"} {
		if !strings.Contains(body, choice) {
			t.Fatalf("body missing choice %q: %s", choice, body)
		}
	}
	if !strings.Contains(body, `data-exercise-id="`) {
		t.Fatalf("body missing the exercise id marker: %s", body)
	}
}

// TestPracticeAnswerCorrectChoiceRendersResult pins the brief's happy
// path: the correct choice, with confidence, renders 正解 and feedback.
func TestPracticeAnswerCorrectChoiceRendersResult(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/practice/start", nil))
	id := extractExerciseID(t, start.Body.String())

	rec := postForm(t, h, "/practice/"+id+"/answer", url.Values{
		"response":   {"面白かったです"},
		"confidence": {"4"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "正解") {
		t.Fatalf("body missing 正解: %s", body)
	}
	if !strings.Contains(body, `data-correct="true"`) {
		t.Fatalf("body missing data-correct=true: %s", body)
	}
}

// TestPracticeAnswerWrongChoiceIsEncouraging pins PRD §56: a wrong
// choice still renders 200 with encouraging retry copy, never
// scolding/penalty/streak language.
func TestPracticeAnswerWrongChoiceIsEncouraging(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/practice/start", nil))
	id := extractExerciseID(t, start.Body.String())

	rec := postForm(t, h, "/practice/"+id+"/answer", url.Values{"response": {"面白いでした"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "もう一度挑戦しましょう") {
		t.Fatalf("body missing the encouraging retry copy: %s", body)
	}
	if !strings.Contains(body, `data-correct="false"`) {
		t.Fatalf("body missing data-correct=false: %s", body)
	}
	for _, bad := range []string{"失敗", "ペナルティ", "penalty", "streak"} {
		if strings.Contains(body, bad) {
			t.Fatalf("body contains scolding/penalty language %q: %s", bad, body)
		}
	}
}

// TestPracticeAnswerRecordsQuizEvents pins the events contract: Start
// records quiz.started, Answer records quiz.answered + quiz.completed,
// each carrying concept Evidence.
func TestPracticeAnswerRecordsQuizEvents(t *testing.T) {
	h, _, events := practiceTestServer(t)

	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/practice/start", nil))
	id := extractExerciseID(t, start.Body.String())

	postForm(t, h, "/practice/"+id+"/answer", url.Values{"response": {"面白かったです"}, "confidence": {"3"}})

	got := events.byIdentity["dev"]
	if len(got) != 3 {
		t.Fatalf("recorded events = %d, want 3 (started, answered, completed): %+v", len(got), got)
	}
	wantTypes := []event.Type{event.TypeQuizStarted, event.TypeQuizAnswered, event.TypeQuizCompleted}
	for i, want := range wantTypes {
		if got[i].Type != want {
			t.Fatalf("events[%d].Type = %q, want %q", i, got[i].Type, want)
		}
		if got[i].Evidence["concept"] != "i-adjective-past" {
			t.Fatalf("events[%d].Evidence[concept] = %v, want i-adjective-past", i, got[i].Evidence["concept"])
		}
	}
}

// TestPracticeAnswerInvalidConfidenceReturnsBadRequest pins the
// confidence validation contract at the HTTP layer.
func TestPracticeAnswerInvalidConfidenceReturnsBadRequest(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/practice/start", nil))
	id := extractExerciseID(t, start.Body.String())

	rec := postForm(t, h, "/practice/"+id+"/answer", url.Values{"response": {"x"}, "confidence": {"6"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestPracticeAnswerUnknownExerciseReturns404 pins the not-found
// contract at the HTTP layer.
func TestPracticeAnswerUnknownExerciseReturns404(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	rec := postForm(t, h, "/practice/does-not-exist/answer", url.Values{"response": {"x"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestPracticePageRenders pins the plain page render.
func TestPracticePageRenders(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/practice", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "練習する") {
		t.Fatalf("body missing the 練習する button: %s", rec.Body.String())
	}
}

// failingDrillGen is an ai.StructuredGenerator that always fails, so a
// test can exercise the path where generation breaks — the path that
// used to leave no trace anywhere.
type failingDrillGen struct{}

func (failingDrillGen) GenerateStructured(context.Context, ai.StructuredRequest) (ai.StructuredResponse, error) {
	return ai.StructuredResponse{}, errors.New("provider exploded")
}

// A drill that cannot be generated must answer non-2xx. htmx does not
// swap a non-2xx response, so the learner sees an unchanged page — which
// is only survivable because the server logs the cause. A handler that
// answered 200 with an empty body would be worse: a blank card and no
// error anywhere.
func TestPracticeStartFailsLoudlyWhenGenerationFails(t *testing.T) {
	opts := testOptions()
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())
	grammarRepo := &practiceGrammarRepo{
		concepts: []grammar.Concept{practiceTestConcept},
		bySlug:   map[string]grammar.Concept{practiceTestConcept.Slug: practiceTestConcept},
	}
	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, grammarRepo, practicePriorityRepo{}, &fakeVocabRepo{}, time.Now)
	opts.Practice = apppractice.NewService(newPracticeExerciseRepo(), drill.New(failingDrillGen{}), teachingPlanner, grammarRepo, rec, nil, nil, nil)
	h := NewServer(opts).HandlerForTest()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/practice/start", nil))

	if w.Code == http.StatusOK {
		t.Fatalf("a failed generation answered 200 — htmx would swap that in as the exercise: %s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// The dropdown always posts a value, so an unknown one is a stale or
// tampered form rather than a routing hint. Same contract the workspace's
// feedback route already enforces.
func TestPracticeStartRejectsAnUnknownProvider(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	form := url.Values{"provider_override": {"not-a-configured-provider"}}
	req := httptest.NewRequest(http.MethodPost, "/practice/start", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unconfigured adapter must be refused, not routed around", w.Code)
	}
}

// The page has to render the dropdown and the progress hooks, or the
// button is back to looking dead during a 30-second model call.
func TestPracticePageOffersAdapterChoiceAndProgress(t *testing.T) {
	h, _, _ := practiceTestServer(t)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/practice", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, want := range []string{
		`id="provider-override"`,
		`data-progress-into="#exercise-area"`,
		`data-progress-label-from="#provider-override"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}

	// The select must sit OUTSIDE the swap target, or the first question
	// destroys it and takes the adapter choice with it.
	selectIdx := strings.Index(body, `id="provider-override"`)
	areaIdx := strings.Index(body, `id="exercise-area"`)
	if selectIdx == -1 || areaIdx == -1 || selectIdx > areaIdx {
		t.Error("the adapter select is inside #exercise-area, so the first swap will delete it")
	}
}

// The sentence is split server-side into before/word/after so the
// template escapes all three normally. Building "<strong>" in the
// handler would mean handing the template pre-trusted HTML made from a
// learner's own sentence — which is how an escaping bug becomes an
// injection.
func TestHighlightSplitsAroundTheWord(t *testing.T) {
	got := highlight("この二つの記号は紛らわしいので注意", "紛らわしい")
	if got == nil {
		t.Fatal("no highlight for a sentence that contains the word")
	}
	if got.Before != "この二つの記号は" || got.Word != "紛らわしい" || got.After != "ので注意" {
		t.Errorf("split = %q | %q | %q", got.Before, got.Word, got.After)
	}
	// Whole is what a speak button reads: the emphasis is visual and has
	// no business in the audio.
	if got.Whole != "この二つの記号は紛らわしいので注意" {
		t.Errorf("Whole = %q, want the unsplit sentence", got.Whole)
	}
}

// A highlight that highlights nothing is just a sentence, and the
// template renders it only when there is something to emphasise.
func TestHighlightIsAbsentWhenThereIsNothingToMark(t *testing.T) {
	for _, tc := range []struct{ name, sentence, word string }{
		{"no sentence", "", "紛らわしい"},
		{"no word", "文があります", ""},
		{"word not present", "この文には入っていません", "紛らわしい"},
	} {
		if got := highlight(tc.sentence, tc.word); got != nil {
			t.Errorf("%s: got %+v, want nil", tc.name, got)
		}
	}
}

// A learner's sentence is data, never markup — pinned through the
// RENDERED partial, because that is where it would actually go wrong.
// The whole reason for splitting the sentence server-side is that the
// alternative, building HTML in the handler, works silently until
// someone's vocabulary contains a "<".
func TestAnExampleSentenceIsEscapedNotInjected(t *testing.T) {
	view := exerciseView{
		ID: "ex-1", Type: exercise.TypeWordRecall, IsWord: true,
		Prompt: "危ない", Reading: "あぶない",
		Position: 1, Total: runLength, Percent: 10,
		ExampleParts: highlight("<script>alert(1)</script>は危ないです", "危ない"),
	}
	if view.ExampleParts == nil {
		t.Fatal("no highlight; the fixture does not exercise the path")
	}

	w := httptest.NewRecorder()
	RenderPartial(w, httptest.NewRequest(http.MethodGet, "/practice", nil), "exercise", view)
	body := w.Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("a learner's sentence was rendered as live markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the sentence was not escaped into the output at all:\n%s", body)
	}
	// And the emphasis itself IS markup, deliberately.
	if !strings.Contains(body, "<strong>危ない</strong>") {
		t.Errorf("the target word was not emphasised:\n%s", body)
	}
}
