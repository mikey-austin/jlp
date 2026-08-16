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
// are the two Status values a conversation correction's
// correctionCardView carries — see toConversationTurnCardView's doc
// comment for exactly when each applies and why.
const (
	conversationCorrectionStatusGated = "presented"
	conversationCorrectionStatusNoted = "noted"
)

// conversationGatedNote is the copy a GATED conversation turn card
// carries where the writing pane's card carries its 答えを見る/retry
// forms (whole-branch review C-2).
//
// A conversation correction has no corrections-table row, so it has no
// per-correction reveal route and none is being added here. Without
// this line the card was a permanent dead end in the DEFAULT
// configuration (socratic teacher mode + "end" feedback timing): a hint
// with no affordance, no explanation, and — before this round — a
// digest that re-gated the same correction, so the answer was
// unreachable on every surface. The fix is to make the conversation's
// EXISTING end-of-conversation digest the reveal (see
// toConversationDigestCardView) and to say so here, on the card that
// withholds, naming the exact button that opens it. The wording is
// deliberately concrete about which control to press: "not available
// yet" without a destination is the same dead end with an apology
// attached.
const conversationGatedNote = "答えは会話の最後に「会話をまとめる」を押すと表示されます。"

// toConversationCardView maps a domain correction.Correction onto the
// SAME correctionCardView the writing feedback pane's correction_card
// partial renders — the fields that are identical for BOTH conversation
// surfaces. Status/Gated/GatedNote are the two surfaces' only
// difference and are set by the two callers below, never here, so
// "which surface reveals" is stated once per surface rather than being
// re-derived by a shared helper that would then need a flag.
//
// Attempts is always 0, Revealed always false and Interactive always
// false: a conversation correction has no accept/reject/retry/reveal
// lifecycle of its own (application/conversation's package doc comment
// explains why corrections are persisted whole on the turn rather than
// as their own row), so correction_card.html.tmpl's retry/reveal
// <form>s — which post to /corrections/{id}/… — would 404 if rendered.
func toConversationCardView(c correction.Correction) correctionCardView {
	return correctionCardView{
		ID:            c.ID,
		Type:          string(c.Type),
		Severity:      string(c.Severity),
		Original:      c.Original,
		Replacement:   c.Replacement,
		ExplanationJA: c.Explanation.JA,
		ExplanationEN: c.Explanation.EN,
		DiffSpans:     toDiffSpans(diff.Runes(c.Original, c.Replacement)),
		Concepts:      c.Concepts,
		HintJA:        c.Hint.JA,
		HintEN:        c.Hint.EN,
		HasHint:       c.HasHint(),
		Interactive:   false,
	}
}

// toConversationTurnCardView is a correction as it appears INSIDE the
// running conversation — the surface that still withholds.
//
//   - HasHint() true (socratic teacher mode): Status is "presented",
//     the only value that, with HasHint and !Revealed, satisfies
//     domain correction.IsGated — called here directly rather than
//     restated, so this surface cannot drift from the definition every
//     other surface uses. The card shows the hint, never Replacement or
//     Explanation, plus conversationGatedNote naming where the answer
//     will come from.
//   - Otherwise: Status is "noted" — the full explanation, with no
//     interactive footer at all (correction_card.html.tmpl has no case
//     for "noted" except its trailing `<footer class="resolved">`).
func toConversationTurnCardView(c correction.Correction) correctionCardView {
	v := toConversationCardView(c)
	v.Status = conversationCorrectionStatusNoted
	if c.HasHint() {
		v.Status = conversationCorrectionStatusGated
	}
	v.Gated = correction.IsGated(v.HasHint, v.Status, v.Revealed)
	if v.Gated {
		v.GatedNote = conversationGatedNote
	}
	return v
}

// toConversationDigestCardView is a correction as it appears in the
// end-of-conversation digest — the surface that REVEALS.
//
// This is the whole-branch review's C-2 ruling, and it is a product
// decision, so it is stated here rather than implied: pressing
// 会話をまとめる IS the conversation's reveal. A per-correction 答えを見る
// has nothing to flip (no row), and a gate with no ungate anywhere is
// not a gate — it is a deletion. The digest is a deliberate learner
// action, taken once the conversation (where the tutor's own socratic
// follow-ups do the eliciting) is over, and it is the last surface a
// conversation correction ever reaches; withholding here withholds
// forever. So Gated is false unconditionally — NOT a re-derivation of
// correction.IsGated with different inputs, but the explicit statement
// that this surface is past the gate — and Status is "noted" so the
// card renders the answer with no accept/reject footer pointing at
// routes that would 404.
//
// The socratic contract is intact where it does work: every turn card
// during the conversation still withholds (toConversationTurnCardView),
// and nothing reveals until the learner asks.
func toConversationDigestCardView(c correction.Correction) correctionCardView {
	v := toConversationCardView(c)
	v.Status = conversationCorrectionStatusNoted
	v.Gated = false
	return v
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
		cards = append(cards, toConversationTurnCardView(c))
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
		cards = append(cards, toConversationDigestCardView(c))
	}
	return conversationSummaryView{Turns: sum.Turns, Cards: cards}
}

// conversationSay handles the conversation pane's message form: form
// field "text" is the learner's new message. "speech_event_id" is an
// optional hidden field (workspace.html.tmpl's conversation-form,
// populated by record.js from POST /speech/transcribe's own
// response) — empty for ordinary typed input, set when text came from
// a transcript, so appconversation.Service.Say can tag the resulting
// turn's event with the join key code review Important I1 asked for.
// This is decode/encode plumbing only: whether or not the field is
// set, this is the exact same Say call, same corrections, same turn.
// Renders the "conversation_turn" partial, appended to the transcript
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

	turn, err := s.opts.Conversation.Say(r.Context(), ident.ID, sid, r.FormValue("text"), r.FormValue("speech_event_id"))
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
