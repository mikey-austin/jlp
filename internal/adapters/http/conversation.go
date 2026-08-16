package httpx

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/diff"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// conversationCorrectionStatusGated/conversationCorrectionStatusNoted
// are the two Status values toConversationCardView assigns a
// conversation correction's correctionCardView — see that function's
// doc comment for exactly when each applies and why.
const (
	conversationCorrectionStatusGated = "presented"
	conversationCorrectionStatusNoted = "noted"
)

// toConversationCardView maps a domain correction.Correction (Phase 4
// Task 6, PRD §17.4) onto the SAME correctionCardView the writing
// feedback pane's correction_card partial already renders (see
// feedback.go) — one partial, one IsGated-respecting template, for both
// surfaces. A conversation correction has no accept/reject/retry/reveal
// lifecycle of its own — application/conversation's package doc comment
// explains why corrections are persisted whole on the turn rather than
// as their own row — so this always sets Attempts 0, Revealed false,
// and picks Status by whether the correction is socratically hinted:
//
//   - HasHint() true (Task 8 socratic mode): Status is "presented" —
//     the ONLY value that, combined with HasHint, satisfies
//     correction.IsGated (see that function's doc comment: hasHint &&
//     status=="presented" && !revealed) — so correction_card's
//     pre-reveal gate hides Replacement/Explanation and shows only the
//     hint, exactly like a writing correction's card would. This is
//     the task's mandated pin: a gated correction surfaced in a
//     conversation shows the hint and NOT the replacement (see
//     conversation_test.go). Its retry/reveal <form> actions still
//     point at /corrections/{id}/retry|reveal, which would 404 if
//     actually submitted (no corrections-table row backs this ID) —
//     a known, narrow gap accepted for this task; see the task report.
//   - Otherwise: Status is "noted", a value correction_card.html.tmpl
//     has no case for except its trailing `{{else}}<footer
//     class="resolved">` branch — the full explanation is shown, but
//     with NO interactive footer at all (not even accept/reject), so
//     there's nothing that could 404.
func toConversationCardView(c correction.Correction) correctionCardView {
	status := conversationCorrectionStatusNoted
	if c.HasHint() {
		status = conversationCorrectionStatusGated
	}
	return correctionCardView{
		ID:            c.ID,
		Type:          string(c.Type),
		Severity:      string(c.Severity),
		Original:      c.Original,
		Replacement:   c.Replacement,
		ExplanationJA: c.Explanation.JA,
		ExplanationEN: c.Explanation.EN,
		Status:        status,
		DiffSpans:     toDiffSpans(diff.Runes(c.Original, c.Replacement)),
		Concepts:      c.Concepts,
		HintJA:        c.Hint.JA,
		HintEN:        c.Hint.EN,
		HasHint:       c.HasHint(),
	}
}

// conversationTurnView is one exchange as the "conversation_turn"
// partial renders it.
type conversationTurnView struct {
	ID          string
	LearnerText string
	Reply       string
	ReplyEN     string
	Followup    string
	Cards       []correctionCardView
	// Withheld is true when this turn's OWN message found at least one
	// correction that FeedbackTiming is holding back (t.Pending > 0 —
	// see appconversation.Turn.Pending's doc comment) — the template's
	// cue for the "held for later" caption PRD §17.4 asks the UI to
	// explain, as opposed to a turn that was simply clean. Driven by the
	// service's own count of what was found, never guessed from Timing
	// alone: a "end"/"delayed" session's genuinely clean turn must not
	// show this caption just because its timing policy would otherwise
	// withhold something.
	Withheld bool
}

func toConversationTurnView(t appconversation.Turn) conversationTurnView {
	cards := make([]correctionCardView, 0, len(t.Corrections))
	for _, c := range t.Corrections {
		cards = append(cards, toConversationCardView(c))
	}
	return conversationTurnView{
		ID:          t.ID,
		LearnerText: t.LearnerText,
		Reply:       t.Reply,
		ReplyEN:     t.ReplyEN,
		Followup:    t.Followup,
		Cards:       cards,
		Withheld:    t.Pending > 0,
	}
}

// conversationSummaryView is Summarise's digest as the
// "conversation_summary" partial renders it.
type conversationSummaryView struct {
	Turns int
	Cards []correctionCardView
}

func toConversationSummaryView(sum appconversation.Summary) conversationSummaryView {
	cards := make([]correctionCardView, 0, len(sum.Corrections))
	for _, c := range sum.Corrections {
		cards = append(cards, toConversationCardView(c))
	}
	return conversationSummaryView{Turns: sum.Turns, Cards: cards}
}

// conversationSay handles the conversation pane's message form: form
// field "text" is the learner's new message. Renders the
// "conversation_turn" partial, appended to the transcript
// (hx-swap="beforeend" on #conversation-transcript — see
// workspace.html.tmpl).
func (s *Server) conversationSay(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	sid := session.ID(chi.URLParam(r, "id"))
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	turn, err := s.opts.Conversation.Say(r.Context(), ident.ID, sid, r.FormValue("text"))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not send message", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "conversation_turn", toConversationTurnView(turn))
}

// conversationSummarise handles the conversation pane's まとめる
// button. Renders the "conversation_summary" partial, replacing
// #conversation-digest.
func (s *Server) conversationSummarise(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	sid := session.ID(chi.URLParam(r, "id"))

	summary, err := s.opts.Conversation.Summarise(r.Context(), ident.ID, sid)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not summarise conversation", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "conversation_summary", toConversationSummaryView(summary))
}
