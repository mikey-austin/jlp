package httpx

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// exerciseView is a generated exercise as the "exercise" partial
// renders it: the question form, with the input shape chosen by Type
// (radio choices for multiple-choice, a text input for
// fill-in-blank/transformation, a textarea for free-production — see
// the template's own {{if eq .Type ...}} branches).
type exerciseView struct {
	ID                             string
	Type                           string
	InstructionsJA, InstructionsEN string
	Prompt                         string
	Choices                        []string
}

func toExerciseView(ex exercise.Exercise) exerciseView {
	return exerciseView{
		ID:             ex.ID,
		Type:           ex.Type,
		InstructionsJA: ex.InstructionsJA,
		InstructionsEN: ex.InstructionsEN,
		Prompt:         ex.Prompt,
		Choices:        ex.Choices,
	}
}

// exerciseResultView is the result of one Answer call, as the
// "exercise_result" partial renders it: 正解/もう一度挑戦しましょう plus
// feedback copy — PRD §56's encouraging-not-penalizing tone applies to
// BOTH branches (see practice.Service's own doc comment on its fixed
// deterministic-path copy).
type exerciseResultView struct {
	Correct                bool
	Score                  int
	FeedbackJA, FeedbackEN string
}

func toExerciseResultView(eval exercise.Evaluation) exerciseResultView {
	return exerciseResultView{
		Correct:    eval.Correct,
		Score:      eval.Score,
		FeedbackJA: eval.FeedbackJA,
		FeedbackEN: eval.FeedbackEN,
	}
}

// practicePage renders the /practice page shell: a 練習する button posts
// /practice/start into #exercise-area.
func (s *Server) practicePage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	s.render(w, r, "practice", map[string]any{
		"Title":    "練習",
		"Identity": ident,
	})
}

// practiceStart handles the 練習する button (and the result partial's
// own 次の問題へ button, which posts here too): generates and persists
// one new exercise for the caller, rendering the "exercise" partial into
// #exercise-area.
func (s *Server) practiceStart(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	ex, err := s.opts.Practice.Start(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not start practice", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "exercise", toExerciseView(ex))
}

// practiceAnswer handles an exercise form's submit: form field response
// is the learner's answer; confidence is optional (0 = not given, 1..5
// otherwise — see practice.Service.Answer's own doc comment). Renders
// the "exercise_result" partial into #exercise-area, replacing the
// question form.
func (s *Server) practiceAnswer(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	confidence := 0
	if v := r.FormValue("confidence"); v != "" {
		c, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "confidence must be an integer", http.StatusBadRequest)
			return
		}
		confidence = c
	}

	eval, err := s.opts.Practice.Answer(r.Context(), ident.ID, id, r.FormValue("response"), confidence)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, practice.ErrInvalidConfidence) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not submit answer", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "exercise_result", toExerciseResultView(eval))
}
