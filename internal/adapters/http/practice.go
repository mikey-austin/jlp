package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/domain/diff"
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
	// IsWord selects the flip-card branch. A word RECALL drill has no
	// input to type: the learner turns the card over and grades
	// themselves. A word CLOZE is typed like any other answer.
	IsWord bool
	// IsPassage renders the prompt as a reading block above the choices,
	// rather than as the question itself: for this shape the question is
	// the instruction and the prompt is a paragraph to read.
	IsPassage bool
	// LiveCheck is the expected answer for the client's as-you-type
	// check, set ONLY for a cloze over the learner's own vocabulary.
	//
	// This does put the answer in the DOM. That is acceptable for a word
	// the learner wrote down themselves — its flip-card sibling is
	// self-graded, so honesty is already assumed — and unacceptable
	// anywhere else. It is never set for a generated exercise, whose
	// Answer stays server-side, and never for anything under the
	// socratic gate.
	LiveCheck string
	// Reading/Meaning are the back of the flip card, and are sent ONLY
	// for a word drill. They are the learner's own stored vocabulary, so
	// putting them in the DOM reveals nothing they did not write — unlike
	// a generated exercise's Answer, which is never sent.
	Reading, Meaning string
	// Position/Total/Streak drive the run header. They round-trip through
	// hx-vals rather than living server-side: a run's position is a
	// property of the exchange, not of the learner, and storing it would
	// mean deciding when an abandoned run expires.
	Position, Total, Streak int
	// Percent is Position/Total as a whole number, computed here because
	// html/template cannot divide.
	Percent int
}

// runLength is how many questions one practice run is. Ten is long
// enough to feel like a session and short enough to finish in a sitting.
const runLength = 10

// conceptEvery is how often a run insists on a grammar drill regardless
// of how many fresh words are queued. Every third: words still lead, and
// still come freshest-first, but a 500-word import no longer means 500
// drills before a single grammar question.
const conceptEvery = 3

// passageEvery is how often a run asks for a reading passage instead of
// a single-word drill. Every fifth, so a ten-question run gets two —
// enough to be a change of pace, rare enough that a run is still mostly
// quick questions. Falls through when there are not enough words.
//
// Chosen not to collide with conceptEvery: positions 3/6/9 are grammar,
// 5/10 are passages, the rest are single words.
const passageEvery = 5

func toExerciseView(ex exercise.Exercise, position, streak int) exerciseView {
	if position < 1 {
		position = 1
	}
	v := exerciseView{
		ID:             ex.ID,
		Type:           ex.Type,
		InstructionsJA: ex.InstructionsJA,
		InstructionsEN: ex.InstructionsEN,
		Prompt:         ex.Prompt,
		Choices:        ex.Choices,
		IsWord:         ex.Type == exercise.TypeWordRecall,
		IsPassage:      ex.Type == exercise.TypePassageChoice,
		Position:       position,
		Total:          runLength,
		Streak:         streak,
		Percent:        position * 100 / runLength,
	}
	if v.IsWord {
		v.Reading = ex.Answer
		if len(ex.Acceptable) > 0 {
			v.Meaning = ex.Acceptable[0]
		}
	}
	if ex.Type == exercise.TypeWordCloze {
		v.LiveCheck = ex.Answer
	}
	return v
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
	// Spans is the character-level diff from what the learner typed to
	// the canonical answer, so a near-miss shows WHERE it missed instead
	// of just "not quite". Rendered with the same .d-ins/.d-del the
	// correction card uses. Empty when there is nothing to diff: a
	// correct answer, a self-graded card, or free production, which has
	// no single right answer to diff against.
	Spans []diffSpanView
	// Answer is the canonical answer, shown alongside the diff. Only ever
	// set once the attempt is over, never while the question is open.
	Answer string
	// Position/Total/Streak/Percent carry the run forward — see
	// exerciseView. Done marks the end of a run.
	Position, Total, Streak, Percent int
	Done                             bool
	// NextPosition is where 次の問題へ resumes. Computed here because
	// html/template cannot add.
	NextPosition int
}

func toExerciseResultView(eval exercise.Evaluation, ex exercise.Exercise, response string, position, streak int) exerciseResultView {
	v := exerciseResultView{
		Correct:      eval.Correct,
		Score:        eval.Score,
		FeedbackJA:   eval.FeedbackJA,
		FeedbackEN:   eval.FeedbackEN,
		Position:     position,
		Total:        runLength,
		Streak:       streak,
		Percent:      position * 100 / runLength,
		Done:         position >= runLength,
		NextPosition: position + 1,
	}
	// A diff only means something when there is one canonical answer the
	// learner tried to type. Free production has none, and a flip card
	// was never typed at all.
	if !eval.Correct && ex.Answer != "" &&
		ex.Type != exercise.TypeFreeProduction && ex.Type != exercise.TypeWordRecall {
		v.Answer = ex.Answer
		// toDiffSpans, not a second mapping of its own: the correction
		// card already renders .d-ins/.d-del from the same diff, and two
		// spellings of "what changed" is two things to keep agreeing
		// about Japanese.
		v.Spans = toDiffSpans(diff.Runes(strings.TrimSpace(response), strings.TrimSpace(ex.Answer)))
	}
	return v
}

// practicePage renders the /practice page shell: a 練習する button posts
// /practice/start into #exercise-area.
func (s *Server) practicePage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	s.render(w, r, "practice", map[string]any{
		"Title":    "練習",
		"Identity": ident,
		// The same adapter dropdown the workspace has, from the same
		// helper. A drill is a model call of exactly the kind the
		// workspace lets you steer, and there was no reason for it to be
		// steerable in one place and not the other.
		"AIProviders": aiProviderOptionsWithAuto(s.opts.AIProviders, s.opts.AIDefaultProvider),
	})
}

// practiceStart handles the 練習する button (and the result partial's
// own 次の問題へ button, which posts here too): generates and persists
// one new exercise for the caller, rendering the "exercise" partial into
// #exercise-area.
// runStateFrom reads the run position and streak the previous partial
// sent back. Absent or malformed means "first question of a fresh run" —
// a run is a property of the exchange, not of the learner, so a lost
// value simply starts over rather than erroring.
func runStateFrom(r *http.Request) (position, streak int) {
	position, err := strconv.Atoi(r.FormValue("position"))
	if err != nil || position < 1 || position > runLength {
		position = 1
	}
	// run_correct, not "streak": the word itself must not appear in a
	// wrong answer's response body (PRD §56 — no penalty language, and a
	// broken streak is the most common way an app manufactures some).
	// The COUNT is still carried and still shown after a correct answer;
	// it is reset and hidden on a miss, so a failure never mentions it.
	streak, err = strconv.Atoi(r.FormValue("run_correct"))
	if err != nil || streak < 0 {
		streak = 0
	}
	return position, streak
}

// drillOptionsFor is the run's rhythm: which slots skip the word queue
// so a large vocabulary cannot crowd grammar out, and which ask for a
// reading passage. It lives here rather than in the service because only
// the HTTP layer knows where in a run a drill sits — and it is one
// function so that serving position N and PREPARING position N choose
// identically. Two spellings of this would mean every prefetch missed.
func drillOptionsFor(position int, override string) practice.StartOptions {
	return practice.StartOptions{
		ProviderOverride: override,
		SkipWords:        position%conceptEvery == 0,
		WantPassage:      position%passageEvery == 0,
	}
}

func (s *Server) practiceStart(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	// Same contract as the workspace's feedback route: the dropdown
	// always posts a value, and anything not in the configured list is a
	// stale or tampered form rather than a routing hint to honour.
	override := r.FormValue("provider_override")
	if override != "" && !isKnownAIProvider(s.opts.AIProviders, override) {
		http.Error(w, "unknown AI provider", http.StatusBadRequest)
		return
	}

	position, streak := runStateFrom(r)
	opts := drillOptionsFor(position, override)

	// Already built while the learner was answering the previous
	// question, in the common case — see practiceprefetch.go. A miss just
	// means building it now.
	ex, ok := s.prefetch.take(ident.ID, position, opts)
	var err error
	if !ok {
		// Nothing prepared, or it was still building when the learner
		// arrived. Build now — but WITHOUT the slots that need a model:
		// a word drill is instant and always available, and the whole
		// reason this path is being taken is that generation is being
		// slow. Falling back to a grammar drill here would reproduce the
		// stall the prefetch exists to remove.
		//
		// The model is still reached when there is no vocabulary to draw
		// on, which is the only case where waiting is the only option.
		ex, err = s.opts.Practice.Build(r.Context(), ident.ID, practice.StartOptions{
			ProviderOverride: opts.ProviderOverride,
			ExcludeSubject:   opts.ExcludeSubject,
		})
	}
	if err == nil {
		ex, err = s.opts.Practice.Serve(r.Context(), ident.ID, ex)
	}
	if err != nil {
		// Logged before answering, because htmx does not swap a non-2xx
		// response: without this the learner sees a page that did not
		// change and the operator sees nothing at all, which is how a
		// failing drill button reads as a dead one.
		slog.Error("practice: could not start",
			"identity", ident.ID, "provider_override", override, "err", err)
		http.Error(w, "could not start practice", http.StatusInternalServerError)
		return
	}
	// Start the NEXT one now. The learner is about to spend at least a
	// few seconds reading this question, and a grammar drill takes longer
	// than that to generate — this is the whole reason question three
	// used to stall while one and two were instant.
	if next := position + 1; next <= runLength {
		// Excluding what was just served: a word leaves the recent queue
		// when it is ANSWERED, and this drill has only been shown, so
		// without this the next one is very often the same card again.
		nextOpts := drillOptionsFor(next, override)
		nextOpts.ExcludeSubject = ex.SubjectRef
		s.prefetch.start(r.Context(), s.opts.Practice, ident.ID, next, nextOpts)
	}

	RenderPartial(w, r, "exercise", toExerciseView(ex, position, streak))
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

	ex, err := s.opts.Practice.Get(r.Context(), ident.ID, id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		slog.Error("practice: could not load exercise", "identity", ident.ID, "exercise", id, "err", err)
		http.Error(w, "could not submit answer", http.StatusInternalServerError)
		return
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
		slog.Error("practice: could not submit answer",
			"identity", ident.ID, "exercise", id, "err", err)
		http.Error(w, "could not submit answer", http.StatusInternalServerError)
		return
	}
	position, streak := runStateFrom(r)
	if eval.Correct {
		streak++
	} else {
		streak = 0
	}
	RenderPartial(w, r, "exercise_result", toExerciseResultView(eval, ex, r.FormValue("response"), position, streak))
}
