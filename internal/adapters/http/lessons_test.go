package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	agentlesson "github.com/mikeyaustin/jlp/internal/agent/lesson"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeLessonRepo is an in-memory storage.LessonRepository for HTTP-layer
// tests, mirroring the identity-scoped-via-join contract the real
// postgres adapter implements — see
// application/lessons/service_test.go's own double of the same shape.
type fakeLessonRepo struct {
	byID         map[string]storage.Lesson
	observations map[string][]storage.LessonObservation
}

func newFakeLessonRepo() *fakeLessonRepo {
	return &fakeLessonRepo{byID: map[string]storage.Lesson{}, observations: map[string][]storage.LessonObservation{}}
}

func (f *fakeLessonRepo) Insert(_ context.Context, l storage.Lesson) error {
	f.byID[l.ID] = l
	return nil
}

func (f *fakeLessonRepo) List(_ context.Context, identity learner.IdentityID) ([]storage.Lesson, error) {
	var out []storage.Lesson
	for _, l := range f.byID {
		if l.IdentityID == identity {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeLessonRepo) Get(_ context.Context, identity learner.IdentityID, id string) (storage.Lesson, error) {
	l, ok := f.byID[id]
	if !ok || l.IdentityID != identity {
		return storage.Lesson{}, storage.ErrNotFound
	}
	return l, nil
}

// CompleteWithObservation mirrors the real postgres repo's atomic
// contract (Insert + status flip together): a lesson ID that doesn't
// exist, or belongs to a different identity, misses with ErrNotFound
// and mutates nothing.
func (f *fakeLessonRepo) CompleteWithObservation(_ context.Context, identity learner.IdentityID, lessonID string, o storage.LessonObservation, at time.Time) (storage.Lesson, error) {
	l, ok := f.byID[lessonID]
	if !ok || l.IdentityID != identity {
		return storage.Lesson{}, storage.ErrNotFound
	}
	l.Status = "completed"
	l.CompletedAt = at
	f.byID[lessonID] = l
	f.observations[lessonID] = append(f.observations[lessonID], o)
	return l, nil
}

func (f *fakeLessonRepo) Observations(_ context.Context, _ learner.IdentityID, lessonID string) ([]storage.LessonObservation, error) {
	return f.observations[lessonID], nil
}

// lessonsTestServer wires a real chi router with a real
// application/lessons.Service over fakeai — deterministic, no network —
// mirroring anki_test.go's ankiTestServer. The planner/vocab/grammar/
// priority/observation doubles it wires in are the same ones
// feedback_test.go's/vocabulary_test.go's/learner_test.go's own tests
// already establish in this package.
func lessonsTestServer(t *testing.T) (h http.Handler, lessons *fakeLessonRepo, events *fakeEventRepo) {
	t.Helper()
	opts := testOptions()
	events = newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())
	vocabRepo := newFakeVocabRepo()
	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, fakeGrammarRepo{}, fakePriorityRepo{}, vocabRepo, time.Now)

	lessons = newFakeLessonRepo()
	feedbackRepo := newFakeFeedbackRepo()
	lessonAgent := agentlesson.New(fakeai.New())
	lessonSvc := applessons.NewService(lessons, fakePriorityRepo{}, teachingPlanner, feedbackRepo, &fakeObservationRepo{}, lessonAgent, rec)

	opts.Lessons = lessonSvc
	opts.LessonsRepo = lessons

	srv := NewServer(opts)
	return srv.HandlerForTest(), lessons, events
}

// generateLesson drives POST /lessons and follows its redirect to
// extract the new lesson's ID from the Location header.
func generateLesson(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := postForm(t, h, "/lessons", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /lessons status = %d, body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/lessons/") {
		t.Fatalf("Location = %q, want a /lessons/{id} redirect", loc)
	}
	return strings.TrimPrefix(loc, "/lessons/")
}

// TestLessonsGenerateThenDetailShowsAllSections pins the brief's Step 2
// scenario: /lessons -> generate -> the detail page shows all ten PRD
// §18 sections with the canned fakeai content (i-adjective-past and
// それはそれとして), plus tutor.lesson.created recorded.
func TestLessonsGenerateThenDetailShowsAllSections(t *testing.T) {
	h, lessons, events := lessonsTestServer(t)

	id := generateLesson(t, h)
	if _, ok := lessons.byID[id]; !ok {
		t.Fatal("Generate did not persist the lesson")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lessons/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /lessons/%s status = %d, body=%s", id, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, heading := range []string{
		"レベル概要", "強み", "弱み", "重点分野", "語彙", "文法項目",
		"会話プロンプト", "練習問題", "最近の例", "講師への質問",
	} {
		if !strings.Contains(body, heading) {
			t.Errorf("detail page missing section heading %q", heading)
		}
	}
	if !strings.Contains(body, "i-adjective-past") {
		t.Error("detail page missing the canned i-adjective-past content")
	}
	if !strings.Contains(body, "それはそれとして") {
		t.Error("detail page missing the canned それはそれとして content")
	}
	if !strings.Contains(body, "prepared") {
		t.Error("detail page missing the prepared status")
	}
	// The completion form is present (not yet completed).
	if !strings.Contains(body, `action="/lessons/`+id+`/complete"`) {
		t.Error("detail page missing the completion form")
	}

	var found bool
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == "tutor.lesson.created" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a tutor.lesson.created event, found none: %+v", events.byIdentity["dev"])
	}
}

// TestLessonsListShowsGeneratedLesson pins /lessons' list rendering.
func TestLessonsListShowsGeneratedLesson(t *testing.T) {
	h, _, _ := lessonsTestServer(t)
	id := generateLesson(t, h)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lessons", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /lessons status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/lessons/"+id) {
		t.Fatalf("/lessons list missing a link to the generated lesson %s: %s", id, rec.Body.String())
	}
}

// TestLessonsCompleteRecordsObservationAndEvents pins the brief's
// completion scenario exactly: notes 「助詞の復習が必要」, subject
// i-adjective-past → status completed, observation listed, and
// tutor.lesson.completed recorded.
func TestLessonsCompleteRecordsObservationAndEvents(t *testing.T) {
	h, lessons, events := lessonsTestServer(t)
	id := generateLesson(t, h)

	form := url.Values{}
	form.Set("author", "tutor-a")
	form.Set("notes", "助詞の復習が必要")
	form.Set("subjects", "i-adjective-past")
	rec := postForm(t, h, "/lessons/"+id+"/complete", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /lessons/%s/complete status = %d, body=%s", id, rec.Code, rec.Body.String())
	}

	if lessons.byID[id].Status != "completed" {
		t.Fatalf("Status = %q, want completed", lessons.byID[id].Status)
	}

	detailRec := httptest.NewRecorder()
	h.ServeHTTP(detailRec, httptest.NewRequest(http.MethodGet, "/lessons/"+id, nil))
	body := detailRec.Body.String()
	if !strings.Contains(body, "completed") {
		t.Error("detail page missing the completed status")
	}
	if !strings.Contains(body, "助詞の復習が必要") {
		t.Error("detail page missing the recorded observation notes")
	}
	if !strings.Contains(body, "i-adjective-past") {
		t.Error("detail page missing the recorded observation subject")
	}
	if !strings.Contains(body, "tutor-a") {
		t.Error("detail page missing the observation's author")
	}

	var sawCreated, sawCompleted bool
	for _, ev := range events.byIdentity["dev"] {
		switch ev.Type {
		case "tutor.lesson.created":
			sawCreated = true
		case "tutor.lesson.completed":
			sawCompleted = true
			subjects, ok := ev.Evidence["subjects"].([]string)
			if !ok || len(subjects) != 1 || subjects[0] != "i-adjective-past" {
				t.Errorf("tutor.lesson.completed evidence subjects = %+v, want [i-adjective-past]", ev.Evidence["subjects"])
			}
		}
	}
	if !sawCreated || !sawCompleted {
		t.Fatalf("expected both tutor.lesson.created and tutor.lesson.completed events, got: %+v", events.byIdentity["dev"])
	}
}

// TestLessonsDetailUnknownIDReturnsNotFound pins the not-found contract
// at the HTTP layer.
func TestLessonsDetailUnknownIDReturnsNotFound(t *testing.T) {
	h, _, _ := lessonsTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lessons/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestLessonsCompleteUnknownIDReturnsNotFound mirrors the above for the
// completion route.
func TestLessonsCompleteUnknownIDReturnsNotFound(t *testing.T) {
	h, _, _ := lessonsTestServer(t)
	form := url.Values{}
	form.Set("author", "tutor-a")
	form.Set("notes", "n")
	rec := postForm(t, h, "/lessons/does-not-exist/complete", form)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}
