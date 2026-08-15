package httpx

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/domain/diff"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// diffSpanView is one rendered span of an inline diff. Class selects the
// CSS treatment ("d-eq" | "d-ins" | "d-del"); Text is the span's literal
// content.
type diffSpanView struct{ Class, Text string }

// correctionCardView is a single correction.Correction as the
// correction_card partial renders it — and as Task 15's rating widget
// will consume it. HintJA/HintEN, Attempts, and Revealed are Phase 2
// Task 8's active-recall fields (PRD §9/§53): HintJA non-empty is the
// template's own signal that this is a socratic correction — see
// correction_card.html.tmpl's pre-reveal gate — rather than a separate
// bool the view and template could drift out of agreement on.
type correctionCardView struct {
	ID, Type, Severity, Original, Replacement string
	ExplanationJA, ExplanationEN, Status      string
	DiffSpans                                 []diffSpanView
	Concepts                                  []string // grammar concept slugs; chips link /grammar/{slug} (Task 3)
	HintJA, HintEN                            string
	Attempts                                  int
	Revealed                                  bool
}

// feedbackView is the result of one review, as the feedback partial
// renders it. HasGatedCard (Phase 2 Task 8, PRD §9/§53) tells the
// template whether to render the whole-selection "修正案" diff block at
// all — see toFeedbackView's doc comment for why.
type feedbackView struct {
	ID, Original, Corrected, AIRequestID string
	DiffSpans                            []diffSpanView
	Cards                                []correctionCardView
	HasGatedCard                         bool
}

// toDiffSpans maps a rune-level diff onto the CSS classes the diff
// templates key off of.
func toDiffSpans(segs []diff.Segment) []diffSpanView {
	spans := make([]diffSpanView, 0, len(segs))
	for _, seg := range segs {
		class := "d-eq"
		switch seg.Op {
		case diff.OpInsert:
			class = "d-ins"
		case diff.OpDelete:
			class = "d-del"
		}
		spans = append(spans, diffSpanView{Class: class, Text: seg.Text})
	}
	return spans
}

func toCorrectionCardView(cv feedback.CorrectionView) correctionCardView {
	return correctionCardView{
		ID:            cv.ID,
		Type:          string(cv.Type),
		Severity:      string(cv.Severity),
		Original:      cv.Original,
		Replacement:   cv.Replacement,
		ExplanationJA: cv.Explanation.JA,
		ExplanationEN: cv.Explanation.EN,
		Status:        cv.Status,
		DiffSpans:     toDiffSpans(cv.Diff),
		Concepts:      cv.Concepts,
		HintJA:        cv.Hint.JA,
		HintEN:        cv.Hint.EN,
		Attempts:      cv.Attempts,
		Revealed:      cv.Revealed,
	}
}

// toFeedbackView maps a feedback.Feedback onto the "feedback" partial's
// view. HasGatedCard is true when ANY card is still in
// correction_card.html.tmpl's socratic pre-reveal gate (HintJA set,
// Status "presented", not yet Revealed) — when it is, the whole-
// selection "修正案" diff block above the individual cards is
// suppressed entirely (see the "feedback" partial): that block renders
// fb.Diff, the WHOLE selection's before/after, which — unlike each
// card's own diff — isn't behind any per-correction gate, so leaving it
// visible would print every gated correction's Replacement in the
// clear even while its own card is still hiding it. This is a
// per-render decision, not a live one: a later retry/reveal call only
// re-renders the affected correction_card (hx-swap="outerHTML" on
// #corr-{id}), never this outer partial, so a diff block suppressed on
// the initial render stays suppressed even after every gated card in
// it resolves — an accepted UX trade-off (see this task's report) for
// never risking the alternative.
func toFeedbackView(fb feedback.Feedback) feedbackView {
	cards := make([]correctionCardView, 0, len(fb.Corrections))
	gated := false
	for _, c := range fb.Corrections {
		cards = append(cards, toCorrectionCardView(c))
		if c.Hint.JA != "" && c.Status == "presented" && !c.Revealed {
			gated = true
		}
	}
	return feedbackView{
		ID:           fb.ID,
		Original:     fb.Original,
		Corrected:    fb.Corrected,
		AIRequestID:  fb.AIRequestID,
		DiffSpans:    toDiffSpans(fb.Diff),
		Cards:        cards,
		HasGatedCard: gated,
	}
}

// feedbackRequest handles the workspace's フィードバックを取得 button:
// form fields document_id/start/end/text describe the selection. An
// empty selection (start==end) means "review the whole document" —
// feedback.Service handles that, not this handler. Renders the
// "feedback" partial into #feedback-results.
func (s *Server) feedbackRequest(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	sid := session.ID(chi.URLParam(r, "id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	start, err := strconv.Atoi(r.FormValue("start"))
	if err != nil {
		http.Error(w, "bad start", http.StatusBadRequest)
		return
	}
	end, err := strconv.Atoi(r.FormValue("end"))
	if err != nil {
		http.Error(w, "bad end", http.StatusBadRequest)
		return
	}

	fb, err := s.opts.Feedback.RequestFeedback(r.Context(), feedback.Request{
		Identity:   ident.ID,
		SessionID:  sid,
		DocumentID: writing.DocumentID(r.FormValue("document_id")),
		Start:      start,
		End:        end,
		Text:       r.FormValue("text"),
	})
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, feedback.ErrInvalidSelection) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not get feedback", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "feedback", toFeedbackView(fb))
}

// correctionStatus handles a correction card's 納得した/同意しない
// buttons: form field status = accepted|rejected. Renders the
// "correction_card" partial with the updated status, replacing the
// card in place (hx-swap="outerHTML").
func (s *Server) correctionStatus(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	cv, err := s.opts.Feedback.SetCorrectionStatus(r.Context(), ident.ID, id, r.FormValue("status"))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, feedback.ErrInvalidStatus) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not update correction status", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "correction_card", toCorrectionCardView(cv))
}

// correctionRetry handles a socratic correction card's retry form: form
// field attempt = the learner's re-typed answer. Renders the refreshed
// "correction_card" partial — 正解！ banner and confidence widget when
// the attempt matched (correction_card.html.tmpl's own Attempts>0 gate
// distinguishes this from a plain 納得した accept), hint/attempts count
// otherwise — replacing the card in place, same hx-swap="outerHTML"
// convention as correctionStatus above.
func (s *Server) correctionRetry(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	result, err := s.opts.Feedback.RetryCorrection(r.Context(), ident.ID, id, r.FormValue("attempt"))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not retry correction", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "correction_card", toCorrectionCardView(result.CorrectionView))
}

// correctionReveal handles a socratic correction card's 答えを見る
// button: no form fields. Renders the refreshed "correction_card"
// partial with the answer now visible (correction_card.html.tmpl's
// pre-reveal gate checks Revealed).
func (s *Server) correctionReveal(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")

	cv, err := s.opts.Feedback.RevealCorrection(r.Context(), ident.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not reveal correction", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "correction_card", toCorrectionCardView(cv))
}

// correctionConfidence handles the correction card's confidence-star
// widget: form field confidence = 1..5. Responds 204 with no body,
// same "the widget updates its own state client-side via Alpine"
// convention as ratingsCreate (ai.go) — see that handler's doc comment.
func (s *Server) correctionConfidence(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	confidence, err := strconv.Atoi(r.FormValue("confidence"))
	if err != nil {
		http.Error(w, "confidence must be an integer", http.StatusBadRequest)
		return
	}

	if err := s.opts.Feedback.RecordConfidence(r.Context(), ident.ID, id, confidence); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, feedback.ErrInvalidConfidence) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not record confidence", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
