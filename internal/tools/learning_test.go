package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // port-shaped test double; see fakes_test.go's package doc comment
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // port-shaped test double; see fakes_test.go's package doc comment
	agentanki "github.com/mikeyaustin/jlp/internal/agent/anki"
	agentdrill "github.com/mikeyaustin/jlp/internal/agent/drill"
	agentlesson "github.com/mikeyaustin/jlp/internal/agent/lesson"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	appractice "github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// catalogConcept is the one grammar concept every learning_test.go
// fixture's planner falls back to (no priorities seeded, so
// practice.Service.Start's TopConcept always misses and
// randomCatalogConcept picks this — the only entry — deterministically).
var catalogConcept = grammar.Concept{Slug: "i-adjective-past", Name: "i-adjective past tense", JLPTLevel: 4, Description: "d", Examples: []string{"e"}}

func TestRecordLearningEventDelegatesToRecorder(t *testing.T) {
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())

	tool := findTool(t, tools.LearningTools(rec, nil, nil, nil), "record_learning_event")
	out, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"type":"vocabulary.looked-up","subject":"面白い"}`))
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	if out != `{"recorded":true}` {
		t.Fatalf("Handler() = %q, want {\"recorded\":true}", out)
	}

	got := events.snapshot()
	if len(got) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(got))
	}
	if got[0].IdentityID != testIdentity || got[0].Type != event.TypeVocabularyLookedUp || got[0].Subject != "面白い" {
		t.Fatalf("recorded event = %+v, want identity=%s type=%s subject=面白い", got[0], testIdentity, event.TypeVocabularyLookedUp)
	}
}

func TestRecordLearningEventRefusesUnknownType(t *testing.T) {
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	tool := findTool(t, tools.LearningTools(rec, nil, nil, nil), "record_learning_event")

	_, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"type":"not.a.real.type"}`))
	if err == nil {
		t.Fatal("Handler() err = nil, want an error for an unknown event type")
	}
}

// newPracticeService wires a real application/practice.Service against
// fakes plus a fakeai-backed drill.Agent: no priorities are seeded, so
// Start always falls back to catalogConcept via randomCatalogConcept.
func newPracticeService() *appractice.Service {
	grammarRepo := newFakeGrammarRepo([]grammar.Concept{catalogConcept})
	plnr := planner.NewPlanner(newFakeObservationRepo(), newFakeEventRepo(), grammarRepo, newFakePriorityRepo(), &fakeVocabRepo{}, time.Now)
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	return appractice.NewService(newFakeExerciseRepo(), agentdrill.New(fakeai.New()), plnr, grammarRepo, rec, nil, nil, nil)
}

func TestCreateExerciseDelegatesToPracticeService(t *testing.T) {
	svc := newPracticeService()
	tool := findTool(t, tools.LearningTools(learning.NewRecorder(newFakeEventRepo(), inprocbus.New()), svc, nil, nil), "create_exercise")

	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["id"] == "" || got["id"] == nil {
		t.Fatalf("got %v, want a non-empty id (the exercise was persisted)", got)
	}
	if got["concept_slug"] != catalogConcept.Slug {
		t.Fatalf("concept_slug = %v, want %q", got["concept_slug"], catalogConcept.Slug)
	}
}

// newLessonsService wires a real application/lessons.Service against
// fakes plus a fakeai-backed lesson.Agent.
func newLessonsService(lessonRepo *fakeLessonRepo, feedback storage.FeedbackRepository) *applessons.Service {
	obs := newFakeObservationRepo()
	prios := newFakePriorityRepo()
	plnr := planner.NewPlanner(obs, newFakeEventRepo(), newFakeGrammarRepo(nil), prios, &fakeVocabRepo{}, time.Now)
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	return applessons.NewService(lessonRepo, prios, plnr, feedback, obs, agentlesson.New(fakeai.New()), rec)
}

func TestCreateLessonPlanDelegatesAndPersists(t *testing.T) {
	lessonRepo := newFakeLessonRepo()
	svc := newLessonsService(lessonRepo, newFakeFeedbackRepo())
	tool := findTool(t, tools.LearningTools(learning.NewRecorder(newFakeEventRepo(), inprocbus.New()), nil, svc, nil), "create_lesson_plan")

	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["status"] != "prepared" {
		t.Fatalf("status = %v, want prepared", got["status"])
	}
	if _, ok := lessonRepo.byID[got["id"].(string)]; !ok {
		t.Fatalf("lesson %v was not persisted", got["id"])
	}
	if _, hasPlan := got["plan"]; !hasPlan {
		t.Fatalf("output has no plan field: %s", out)
	}
}

// newAnkiService wires a real application/anki.Service against fakes
// plus a fakeai-backed anki.Agent.
func newAnkiService(cardRepo *fakeAnkiCardRepo, feedback storage.FeedbackRepository) *appanki.Service {
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	return appanki.NewService(cardRepo, feedback, agentanki.New(fakeai.New()), rec)
}

func TestCreateAnkiCardDelegatesAndPersists(t *testing.T) {
	feedback := newFakeFeedbackRepo()
	feedback.seed(testIdentity, storage.CorrectionRecord{ID: "c-ungated", Original: "面白いでした", Replacement: "面白かったです", Status: "presented"})
	cardRepo := newFakeAnkiCardRepo()
	svc := newAnkiService(cardRepo, feedback)
	tool := findTool(t, tools.LearningTools(learning.NewRecorder(newFakeEventRepo(), inprocbus.New()), nil, nil, svc), "create_anki_card")

	out, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"correction_id":"c-ungated"}`))
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["front"] == "" || got["front"] == nil {
		t.Fatalf("got %v, want a non-empty front", got)
	}
	if _, ok := cardRepo.byID[got["id"].(string)]; !ok {
		t.Fatalf("card %v was not persisted", got["id"])
	}
}

// TestCreateAnkiCardRefusesGatedCorrection pins the task brief's
// explicit requirement: a correction still gated behind its own
// socratic hint (HasHint true, Status "presented", not yet Revealed —
// see storage.CorrectionRecord.IsGated) must yield a tool error, never
// a card.
func TestCreateAnkiCardRefusesGatedCorrection(t *testing.T) {
	feedback := newFakeFeedbackRepo()
	feedback.seed(testIdentity, storage.CorrectionRecord{
		ID: "c-gated", Original: "面白いでした", Replacement: "面白かったです",
		Status: "presented", HintEN: "recall the past tense rule", Revealed: false,
	})
	cardRepo := newFakeAnkiCardRepo()
	svc := newAnkiService(cardRepo, feedback)
	tool := findTool(t, tools.LearningTools(learning.NewRecorder(newFakeEventRepo(), inprocbus.New()), nil, nil, svc), "create_anki_card")

	_, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"correction_id":"c-gated"}`))
	if err == nil {
		t.Fatal("Handler() err = nil, want an error for a gated correction")
	}
	if len(cardRepo.byID) != 0 {
		t.Fatalf("cardRepo has %d cards, want 0 — a gated correction must never produce a card", len(cardRepo.byID))
	}
}

func TestCreateAnkiCardRequiresCorrectionID(t *testing.T) {
	svc := newAnkiService(newFakeAnkiCardRepo(), newFakeFeedbackRepo())
	tool := findTool(t, tools.LearningTools(learning.NewRecorder(newFakeEventRepo(), inprocbus.New()), nil, nil, svc), "create_anki_card")

	_, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Handler() err = nil, want an error when correction_id is missing")
	}
}
