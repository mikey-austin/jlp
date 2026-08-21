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
	// deleted mirrors the real table's deleted_at column: every read
	// below honours it, so a handler test can assert a deleted lesson
	// really stops reaching the page rather than merely that the delete
	// route returned 303.
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
	f.byID[l.ID] = l
	return nil
}

func (f *fakeLessonRepo) List(_ context.Context, identity learner.IdentityID) ([]storage.Lesson, error) {
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
// contract (Insert + status flip together): a lesson ID that doesn't
// exist, or belongs to a different identity, misses with ErrNotFound
// and mutates nothing.
func (f *fakeLessonRepo) CompleteWithObservation(_ context.Context, identity learner.IdentityID, lessonID string, o storage.LessonObservation, at time.Time) (storage.Lesson, error) {
	l, ok := f.byID[lessonID]
	if !ok || l.IdentityID != identity {
		return storage.Lesson{}, storage.ErrNotFound
	}
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
	if _, gone := f.deleted[lessonID]; gone {
		return nil, nil
	}
	return f.observations[lessonID], nil
}

// SoftDelete/Restore mirror the real adapter's contract exactly:
// identity-scoped, idempotent, and ErrNotFound for both "unknown id"
// and "someone else's id".
func (f *fakeLessonRepo) SoftDelete(_ context.Context, identity learner.IdentityID, lessonID string, at time.Time) error {
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
	l, ok := f.byID[lessonID]
	if !ok || l.IdentityID != identity {
		return storage.ErrNotFound
	}
	delete(f.deleted, lessonID)
	return nil
}

// isDeleted / exists let a handler test assert on the raw storage state
// rather than on a status code.
func (f *fakeLessonRepo) isDeleted(lessonID string) bool {
	_, gone := f.deleted[lessonID]
	return gone
}

func (f *fakeLessonRepo) exists(lessonID string) bool {
	_, ok := f.byID[lessonID]
	return ok
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
	// The status, in the language the rest of the page is written in.
	// It used to render the raw storage enum, which is how a learner
	// came to be shown the word "prepared" on a Japanese page.
	if !strings.Contains(body, "準備済み") {
		t.Error("detail page missing the prepared status")
	}
	if strings.Contains(body, ">prepared<") {
		t.Errorf("the raw status enum is on the page:\n%s", body)
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
	if !strings.Contains(body, "完了") {
		t.Error("detail page missing the completed status")
	}
	if strings.Contains(body, ">completed<") {
		t.Errorf("the raw status enum is on the page:\n%s", body)
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

// The guide is the page that had drifted furthest out of line with the
// rest of the site: ten <h2>s and ten browser-default <ul>s inside one
// card, with a single CSS rule to its name. It is .panel sections now,
// each carrying a count, and the sections are OPEN — this is a document
// a tutor reads top to bottom and prints, and a closed <details> does
// not print its contents.
func TestTheLessonGuideIsBuiltFromPanelsThatAreOpen(t *testing.T) {
	h, _, _ := lessonsTestServer(t)
	id := generateLesson(t, h)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lessons/"+id, nil))
	body := rec.Body.String()

	panels := strings.Count(body, `<details class="panel" open>`)
	if panels < 5 {
		t.Fatalf("only %d open guide panels rendered; the sections are not panels:\n%s", panels, body)
	}
	// Closed would mean a printed guide loses those sections entirely.
	if strings.Contains(body, `<details class="panel">`) {
		t.Errorf("a guide section is closed; it would be missing from a printed guide:\n%s", body)
	}
	// Counts, so a section that came back empty is visibly empty rather
	// than a heading with nothing under it.
	if !strings.Contains(body, `class="panel__meta"`) {
		t.Errorf("guide sections carry no item count:\n%s", body)
	}
	// The short-label sections are chips, not a single-file column of
	// two-word bullets.
	if !strings.Contains(body, `class="chip-set"`) {
		t.Errorf("語彙/文法項目 did not render as chips:\n%s", body)
	}
}

// An empty section renders nothing at all. A heading with no list under
// it reads as a section that failed to load rather than one the plan had
// no entries for.
func TestAnEmptyGuideSectionIsNotRendered(t *testing.T) {
	if got := guideSections(lessonPlanDTO{Focus: []string{"て-form"}}); len(got) == 0 {
		t.Fatal("guideSections returned nothing")
	}
	// The template drops empties; this pins that the data still offers
	// them so the template is the only place that decides.
	var empties int
	for _, s := range guideSections(lessonPlanDTO{Focus: []string{"て-form"}}) {
		if len(s.Items) == 0 {
			empties++
		}
	}
	if empties == 0 {
		t.Fatal("fixture produced no empty sections, so this proves nothing")
	}

	h, _, _ := lessonsTestServer(t)
	id := generateLesson(t, h)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lessons/"+id, nil))
	body := rec.Body.String()

	// Every rendered panel has a non-zero count.
	if strings.Contains(body, ">0件<") {
		t.Errorf("a section rendered with zero items:\n%s", body)
	}
}

// One line per guide on the list, expanding — the same shape every other
// list on the site uses. 削除 is destructive and belongs one disclosure
// deep, not beside every row.
func TestALessonsControlsAreBehindTheDisclosureNotOnTheLine(t *testing.T) {
	h, _, _ := lessonsTestServer(t)
	generateLesson(t, h)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lessons", nil))
	body := rec.Body.String()

	start := strings.Index(body, `class="list__line"`)
	if start < 0 {
		t.Fatalf("no lesson rows rendered:\n%s", body)
	}
	end := strings.Index(body[start:], "</summary>")
	if end < 0 {
		t.Fatalf("the row has no summary to close:\n%s", body[start:])
	}
	line := body[start : start+end]
	if !strings.Contains(line, `href="/lessons/`) {
		t.Fatalf("sliced the wrong element; it does not link to a lesson:\n%s", line)
	}
	if !strings.Contains(line, "準備済み") {
		t.Errorf("the line does not say what state the guide is in:\n%s", line)
	}
	if strings.Contains(line, "削除") {
		t.Errorf("the delete button is on the summary line:\n%s", line)
	}
	if !strings.Contains(body, "削除") {
		t.Errorf("the delete button was dropped entirely, not just moved:\n%s", body)
	}
}

// A status this page has not been taught about is worth seeing, not
// worth hiding behind a blank badge — an empty badge is
// indistinguishable from a guide with no status at all. Mirrors
// TestAnUnknownSourceFallsThroughAsItself in anki_test.go.
func TestAnUnknownLessonStatusFallsThroughAsItself(t *testing.T) {
	got := toLessonView(storage.Lesson{ID: "l1", Status: "in-progress"})
	if got.StatusLabel != "in-progress" {
		t.Errorf("StatusLabel = %q, want the raw value passed through", got.StatusLabel)
	}
	// A zero CompletedAt is "not completed", not 0001-01-01.
	if got.Completed != "" {
		t.Errorf("Completed = %q for a zero time, want empty", got.Completed)
	}
}
