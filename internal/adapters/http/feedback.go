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
// will consume it.
type correctionCardView struct {
	ID, Type, Severity, Original, Replacement string
	ExplanationJA, ExplanationEN, Status      string
	DiffSpans                                 []diffSpanView
}

// feedbackView is the result of one review, as the feedback partial
// renders it.
type feedbackView struct {
	ID, Original, Corrected, AIRequestID string
	DiffSpans                            []diffSpanView
	Cards                                []correctionCardView
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
	}
}

func toFeedbackView(fb feedback.Feedback) feedbackView {
	cards := make([]correctionCardView, 0, len(fb.Corrections))
	for _, c := range fb.Corrections {
		cards = append(cards, toCorrectionCardView(c))
	}
	return feedbackView{
		ID:          fb.ID,
		Original:    fb.Original,
		Corrected:   fb.Corrected,
		AIRequestID: fb.AIRequestID,
		DiffSpans:   toDiffSpans(fb.Diff),
		Cards:       cards,
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
