package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// lessonView is one storage.Lesson as the "lessons" list page renders
// it.
type lessonView struct {
	ID, Status             string
	CreatedAt, CompletedAt time.Time
	// Preformatted, because the list renders each date in two places and
	// a template that formats the same value twice is a template that
	// eventually formats it two ways.
	Created, Completed string
	StatusLabel        string
	DeleteAction       string
}

// lessonStatusLabels renders storage.Lesson.Status for a learner. An
// unknown status falls through as itself rather than as a blank badge:
// a status this page has not been taught about is worth seeing, not
// worth hiding.
var lessonStatusLabels = map[string]string{
	"prepared":  "準備済み",
	"completed": "完了",
}

func toLessonView(l storage.Lesson) lessonView {
	v := lessonView{
		ID: l.ID, Status: l.Status,
		CreatedAt: l.CreatedAt, CompletedAt: l.CompletedAt,
		Created:      l.CreatedAt.Format("2006-01-02 15:04"),
		StatusLabel:  l.Status,
		DeleteAction: "/lessons/" + l.ID + "/delete",
	}
	if label, ok := lessonStatusLabels[l.Status]; ok {
		v.StatusLabel = label
	}
	// Zero means "not completed" (storage.Lesson's documented
	// convention); formatting it would print 0001-01-01, the same trap
	// /grammar's 未遭遇 avoids.
	if !l.CompletedAt.IsZero() {
		v.Completed = l.CompletedAt.Format("2006-01-02 15:04")
	}
	return v
}

func toLessonViews(lessons []storage.Lesson) []lessonView {
	views := make([]lessonView, 0, len(lessons))
	for _, l := range lessons {
		views = append(views, toLessonView(l))
	}
	return views
}

// guideSection is one titled part of a lesson plan as the detail page
// renders it.
//
// The template used to name all ten sections by hand, each an <h2> and a
// bare <ul>. That is why the page drifted out of line with the rest of
// the site: there was nothing to style, only raw markup, and adding a
// section to the schema meant editing the template. Building the list
// here means one rendering for all of them.
//
// AsChips marks the sections whose items are short labels — a word, a
// grammar point, a focus area — rather than sentences. As bullets they
// were a tall single-file column of two-word lines; as chips they wrap.
type guideSection struct {
	Title   string
	Items   []string
	AsChips bool
}

// guideSections lists the plan in the order a tutor reads it: what to
// work on, then the material, then what to do with it, then what to ask.
// Empty sections are dropped by the template rather than rendering a
// heading with nothing under it.
func guideSections(plan lessonPlanDTO) []guideSection {
	return []guideSection{
		{Title: "重点分野", Items: plan.Focus},
		{Title: "強み", Items: plan.Strengths},
		{Title: "弱み", Items: plan.Weaknesses},
		{Title: "語彙", Items: plan.Vocabulary, AsChips: true},
		{Title: "文法項目", Items: plan.GrammarConcepts, AsChips: true},
		{Title: "会話プロンプト", Items: plan.ConversationPrompts},
		{Title: "練習問題", Items: plan.Exercises},
		{Title: "最近の例", Items: plan.RecentExamples},
		{Title: "講師への質問", Items: plan.QuestionsForTutor},
	}
}

// lessonPlanDTO mirrors schemas/defs/lesson_plan.v1.json field-for-
// field, so unmarshaling a schema-valid storage.Lesson.Plan can never
// silently drop or misname a section — the same convention
// internal/adapters/http/anki.go's cardDTO-shaped views use.
type lessonPlanDTO struct {
	LevelSummary        string   `json:"level_summary"`
	Strengths           []string `json:"strengths"`
	Weaknesses          []string `json:"weaknesses"`
	Focus               []string `json:"focus"`
	Vocabulary          []string `json:"vocabulary"`
	GrammarConcepts     []string `json:"grammar_concepts"`
	ConversationPrompts []string `json:"conversation_prompts"`
	Exercises           []string `json:"exercises"`
	RecentExamples      []string `json:"recent_examples"`
	QuestionsForTutor   []string `json:"questions_for_tutor"`
}

// observationView is one storage.LessonObservation as the detail
// page's completed-lesson view renders it.
type observationView struct {
	Author, Notes string
	Subjects      []string
	CreatedAt     time.Time
}

func toObservationViews(obs []storage.LessonObservation) []observationView {
	views := make([]observationView, 0, len(obs))
	for _, o := range obs {
		views = append(views, observationView{Author: o.Author, Notes: o.Notes, Subjects: o.Subjects, CreatedAt: o.CreatedAt})
	}
	return views
}

// lessonsList renders the /lessons page: every lesson identity has
// generated, newest first, plus the 「レッスンガイド作成」 button that
// posts /lessons.
func (s *Server) lessonsList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	list, err := s.opts.LessonsRepo.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load lessons", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "lessons", map[string]any{
		"Title":         "レッスン",
		"Identity":      ident,
		"Lessons":       toLessonViews(list),
		"RestoreAction": undoRestoreAction(r, "/lessons"),
	})
}

// lessonsDelete handles POST /lessons/{id}/delete: the learner's
// confirmed 削除 of one lesson guide. It stops appearing on /lessons,
// its detail page 404s, and its tutor observations go with it. Nothing
// is erased — the tutor.lesson.created/completed events stay, so
// /learner and /outcomes are unchanged.
//
// The identity comes from the request context and NOTHING else. The id
// in the path is the only caller-supplied input, and the repository's
// WHERE pairs it with this identity, so another learner's lesson
// answers 404 exactly like an id that never existed and is left
// untouched.
func (s *Server) lessonsDelete(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := s.opts.Lessons.Delete(r.Context(), ident.ID, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not delete lesson", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/lessons?"+url.Values{"undo": {id}}.Encode(), http.StatusSeeOther)
}

// lessonsRestore handles POST /lessons/{id}/restore — the undo
// affordance's target. Same identity-from-context rule and same
// 404-for-someone-else's-lesson contract as lessonsDelete.
func (s *Server) lessonsRestore(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := s.opts.Lessons.Restore(r.Context(), ident.ID, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not restore lesson", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/lessons", http.StatusSeeOther)
}

// lessonsGenerate handles the 「レッスンガイド作成」 button: POST /lessons,
// no form fields. Redirects to the new lesson's detail page — the same
// POST-then-303-redirect shape sessionsCreate uses.
func (s *Server) lessonsGenerate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	lesson, err := s.opts.Lessons.Generate(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not generate lesson guide", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/lessons/"+lesson.ID, http.StatusSeeOther)
}

// lessonsDetail renders one lesson guide's PRD §18 sections. A
// "prepared" lesson also shows the completion form (author, notes,
// subjects); a "completed" lesson shows its recorded observation(s)
// instead.
func (s *Server) lessonsDetail(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")

	lesson, err := s.opts.LessonsRepo.Get(r.Context(), ident.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load lesson", http.StatusInternalServerError)
		return
	}

	var plan lessonPlanDTO
	if err := json.Unmarshal(lesson.Plan, &plan); err != nil {
		http.Error(w, "could not read lesson plan", http.StatusInternalServerError)
		return
	}

	observations, err := s.opts.LessonsRepo.Observations(r.Context(), ident.ID, id)
	if err != nil {
		http.Error(w, "could not load observations", http.StatusInternalServerError)
		return
	}

	view := toLessonView(lesson)
	s.render(w, r, "lesson_detail", map[string]any{
		"Title":        "レッスンガイド",
		"Identity":     ident,
		"Lesson":       view,
		"StatusLabel":  view.StatusLabel,
		"Created":      view.Created,
		"Completed":    view.Completed,
		"Plan":         plan,
		"Sections":     guideSections(plan),
		"Observations": toObservationViews(observations),
	})
}

// lessonsComplete handles the completion form's submit: POST
// /lessons/{id}/complete, form fields author, notes, and subjects (a
// comma-separated list of concept slugs or free tags — see
// splitSubjects). Redirects back to the lesson's detail page, now
// showing "completed" and the recorded observation.
func (s *Server) lessonsComplete(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	subjects := splitSubjects(r.FormValue("subjects"))
	_, err := s.opts.Lessons.Complete(r.Context(), ident.ID, id, r.FormValue("author"), r.FormValue("notes"), subjects)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not complete lesson", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/lessons/"+id, http.StatusSeeOther)
}

// splitSubjects parses the completion form's comma-input subjects
// field into a clean []string: entries are trimmed, and empty entries
// (a leading/trailing/doubled comma, or a wholly blank field) are
// dropped rather than kept as blank subjects.
func splitSubjects(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
