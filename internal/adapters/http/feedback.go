package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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
// Task 8's active-recall fields (PRD §9/§53). HasHint is copied
// straight from correction.Correction.HasHint() — the domain's own
// JA-OR-EN-non-empty definition of "this is a socratic correction" —
// rather than re-derived here from HintJA alone: a hint-ful response
// with an empty JA but populated EN (the schema permits it; nothing
// stops a real model from doing this even though fakeai never does)
// would otherwise pass the domain's HasHint() check (and so fire
// hint.shown — see RequestFeedback) while this view's own gate stayed
// closed on an empty HintJA, showing the answer in the clear the
// instant hint.shown claimed one had been shown. See
// correction_card.html.tmpl's pre-reveal gate, which reads HasHint,
// never HintJA, for exactly this reason.
type correctionCardView struct {
	ID, Type, Severity, Original, Replacement string
	ExplanationJA, ExplanationEN, Status      string
	DiffSpans                                 []diffSpanView
	Concepts                                  []string // grammar concept slugs; chips link /grammar/{slug} (Task 3)
	HintJA, HintEN                            string
	HasHint                                   bool
	Attempts                                  int
	Revealed                                  bool
	// Gated is THE socratic pre-reveal decision for this card, computed
	// once here by whoever builds the view and rendered by
	// correction_card.html.tmpl as a plain `{{if .Gated}}`. The template
	// used to re-derive the predicate itself
	// (`and .HasHint (eq .Status "presented") (not .Revealed)`) — the
	// 4th copy of a definition that has leaked in this project before,
	// and load-bearing for three surfaces since Phase 4 Task 6 (writing
	// pane, conversation turns, conversation digest). Every producer
	// now calls through to domain correction.IsGated (via
	// isGatedCorrection for the writing pane, directly in
	// conversation.go for turns) or states an explicit policy in one
	// place (the digest — see toConversationDigestCardView), so there
	// is exactly one definition of "hidden" and the template holds
	// none of it.
	Gated bool
	// GatedNote is optional copy rendered INSIDE the gated branch, in
	// place of the retry/reveal forms, for a surface where those forms
	// don't exist. Empty for a writing-pane correction, which has real
	// forms; set for a conversation turn's correction, which has none —
	// see conversationGatedNote. A gate the learner cannot open, with
	// nothing saying where the answer comes from, is a dead end; this
	// field is what stops the card implying a reveal that cannot happen.
	GatedNote string
	// Interactive gates correction_card.html.tmpl's retry/reveal <form>s
	// (Finding I-2): true for every writing-pane correction (backed by a
	// real corrections table row, so /corrections/{id}/retry|reveal
	// resolve), false for a conversation correction
	// (toConversationCardView) — a conversation turn's corrections have
	// no corrections-table row (see application/conversation's package
	// doc comment), so those two routes would 404 if the forms rendered
	// and were submitted. This does NOT touch the socratic pre-reveal
	// gate itself (still driven by HasHint/Status/Revealed via
	// correction.IsGated's predicate) — a gated conversation correction
	// still shows only its hint, exactly as mandated; it just has no
	// buttons under it.
	Interactive bool
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
	// Provider/Model name which AI provider/model actually served this
	// review (Phase 4 Task W item 1) — copied verbatim from
	// feedback.Feedback.Provider/Model; see that field's own doc
	// comment for why it can differ from whatever the caller requested.
	// Both "" for a fakeai-backed review outside the observability
	// decorator (every test, offline dev).
	Provider, Model string
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
		HasHint:       cv.HasHint(),
		Attempts:      cv.Attempts,
		Revealed:      cv.Revealed,
		Gated:         isGatedCorrection(cv),
		Interactive:   true,
	}
}

// toFeedbackView maps a feedback.Feedback onto the "feedback" partial's
// view. HasGatedCard is true when ANY card is still in
// correction_card.html.tmpl's socratic pre-reveal gate — isGatedCorrection
// (api.go), the SAME predicate toFeedbackDTO uses for the JSON API's own
// Gated field, so the HTML and API response shapes can never
// independently drift on what counts as "hidden" — when it is, the
// whole-selection "修正案" diff block above the individual cards is
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
		if isGatedCorrection(c) {
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
		Provider:     fb.Provider,
		Model:        fb.Model,
	}
}

// historyExcerptRunes caps how much of a history row's reviewed
// selection is shown before an ellipsis — enough to recognize which
// review this was without the right-hand pane growing to fit an entire
// paragraph.
const historyExcerptRunes = 40

func excerptRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// feedbackHistoryItemView is one row of the workspace's feedback
// history list (Phase 4 Task W item 2): a timestamp, a short excerpt of
// what was reviewed, how many corrections came back, and the
// provider/model badge — Active marks whichever row is currently shown
// in #feedback-results (see feedback_history.html.tmpl's is-active
// class), server-computed only for the SSR initial render and the
// POST's own OOB refresh (a client-side click on an older row updates
// this purely in the DOM — see app.js — since a GET to load it doesn't
// itself re-render the whole list).
type feedbackHistoryItemView struct {
	ID              string
	Excerpt         string
	CorrectionCount int
	Provider, Model string
	CreatedAt       time.Time
	Active          bool
}

// feedbackHistoryView is the "feedback_history" partial's data: SessionID
// for building each row's link, OOB (true only when this partial is
// appended after a POST's "feedback" partial as an htmx out-of-band
// swap — Phase 4 Task W item 3) controls whether the root element
// carries hx-swap-oob, and Items is the list itself, newest first.
type feedbackHistoryView struct {
	SessionID string
	OOB       bool
	Items     []feedbackHistoryItemView
}

// toFeedbackHistoryView maps a session's []storage.FeedbackSummary onto
// the history list's view, marking activeID's row (if present) Active.
func toFeedbackHistoryView(sessionID session.ID, activeID string, items []storage.FeedbackSummary, oob bool) feedbackHistoryView {
	out := make([]feedbackHistoryItemView, 0, len(items))
	for _, it := range items {
		out = append(out, feedbackHistoryItemView{
			ID:              it.ID,
			Excerpt:         excerptRunes(it.SelectionText, historyExcerptRunes),
			CorrectionCount: it.CorrectionCount,
			Provider:        it.Provider,
			Model:           it.Model,
			CreatedAt:       it.CreatedAt,
			Active:          activeID != "" && it.ID == activeID,
		})
	}
	return feedbackHistoryView{SessionID: string(sessionID), OOB: oob, Items: out}
}

// aiProviderLabels maps a constructible provider name to the
// workspace dropdown's display label (Phase 4 Task W item 5) — the SAME
// four real-provider names/labels application/settings.Service's own
// providers catalog uses (that page's persistent overrides are a
// DIFFERENT concept from this per-request override, but there's no
// reason to show an operator two different labels for the same
// provider across the app), plus "fake" for the rare case it's genuinely
// the configured default (every non-integration test, `jlp eval`) —
// see cmd/jlp/ai.go's aiProviderPriority doc comment for why "fake"
// isn't normally offered as a choice.
var aiProviderLabels = map[string]string{
	"ollama":    "Ollama",
	"anthropic": "Anthropic",
	"claudecli": "Claude Code CLI",
	"codexcli":  "Codex CLI",
	"agycli":    "Antigravity CLI",
	"fake":      "Fake（オフライン）",
}

// aiProviderOptionView is one <option> in the workspace's per-request
// adapter-override <select> (Phase 4 Task W item 5). Default marks
// (and its Label names) the highest-priority provider — see
// cmd/jlp/ai.go's buildAIGenerator doc comment for exactly how that's
// computed.
type aiProviderOptionView struct {
	Name    string
	Label   string
	Default bool
}

func aiProviderLabel(name string) string {
	if l, ok := aiProviderLabels[name]; ok {
		return l
	}
	return name
}

// aiProviderOptions builds the dropdown's option list from names
// (Options.AIProviders, already in a stable priority order) and def
// (Options.AIDefaultProvider).
func aiProviderOptions(names []string, def string) []aiProviderOptionView {
	out := make([]aiProviderOptionView, 0, len(names))
	for _, name := range names {
		isDefault := name == def
		label := aiProviderLabel(name)
		if isDefault {
			label += "（デフォルト）"
		}
		out = append(out, aiProviderOptionView{Name: name, Label: label, Default: isDefault})
	}
	return out
}

// aiProviderOptionsWithAuto is aiProviderOptions plus a leading
// "automatic" entry, selected by default, whose value is EMPTY.
//
// An empty provider_override means "route normally", which is what
// restores the configured route chain and its fallback. A dropdown whose
// default option carries a provider NAME pins every request to that
// provider with no fallback (see airouter's override branch) — so the
// most common case, the learner never touching the dropdown, silently
// became the least resilient one. Picking a provider deliberately still
// pins it; that is the point of picking.
//
// The workspace deliberately keeps the naming-default behaviour (Phase 4
// Task W item 5, where an operator choosing an adapter wants exactly
// that adapter). This variant exists because 練習's dropdown is used by
// a learner who just wants a question, not by an operator running a
// comparison.
func aiProviderOptionsWithAuto(names []string, def string) []aiProviderOptionView {
	out := make([]aiProviderOptionView, 0, len(names)+1)
	out = append(out, aiProviderOptionView{Name: "", Label: "自動（推奨）", Default: true})
	for _, name := range names {
		label := aiProviderLabel(name)
		if name == def {
			label += "（既定）"
		}
		out = append(out, aiProviderOptionView{Name: name, Label: label})
	}
	return out
}

// isKnownAIProvider reports whether name appears in names — the
// feedbackRequest handler's guard against a stale/tampered
// provider_override value (Phase 4 Task W item 5's "reject an unknown/
// unconfigured provider with a 400" requirement).
func isKnownAIProvider(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
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

	// provider_override (Phase 4 Task W item 5): the dropdown always
	// carries a value, defaulting to s.opts.AIDefaultProvider. Selecting
	// that same default provider is treated as NO override — routing
	// (and its fallback chain, if APP_AI_ROUTES configures one for
	// teacher.feedback) behaves exactly as it did before this feature.
	// Only a genuinely different, explicit choice sets
	// Request.ProviderOverride, which airouter then dispatches to with
	// no fallback (see that field's own doc comment) — this is also why
	// "which provider actually served it" (the finished feedback's own
	// badge) can differ from what was requested: it's the DEFAULT case,
	// not the override case, where a route can fail over. An unknown/
	// unconfigured name is rejected with a 400 rather than silently
	// falling through to the default — a stale form must never produce
	// a different provider's answer than what it displayed.
	override := r.FormValue("provider_override")
	if override != "" && override != s.opts.AIDefaultProvider {
		if !isKnownAIProvider(s.opts.AIProviders, override) {
			http.Error(w, "unknown provider", http.StatusBadRequest)
			return
		}
	} else {
		override = ""
	}

	fb, err := s.opts.Feedback.RequestFeedback(r.Context(), feedback.Request{
		Identity:         ident.ID,
		SessionID:        sid,
		DocumentID:       writing.DocumentID(r.FormValue("document_id")),
		Start:            start,
		End:              end,
		Text:             r.FormValue("text"),
		ProviderOverride: override,
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
		// The client deliberately sees a generic message — the error can
		// quote model output — but the server must not swallow it. This
		// handler previously returned 500 with NO log line at all, which
		// made a production failure undiagnosable: ai_requests showed the
		// provider call succeeding, the app logged nothing, and the only
		// symptom was a 500 in the browser.
		slog.Error("feedback: request failed",
			"err", err,
			"session_id", sid,
			"identity", ident.ID,
			"provider_override", override)
		http.Error(w, "could not get feedback", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "feedback", toFeedbackView(fb))

	// The history list refreshes alongside the detail (Phase 4 Task W
	// item 3), via an htmx out-of-band swap: the "feedback" partial
	// above already became #feedback-results' new content (hx-target on
	// #feedback-btn), and this second, independently-rendered partial
	// carries hx-swap-oob="true" on its own root so htmx replaces
	// #feedback-history with it too, regardless of hx-target — one
	// response, two things updated, no page reload. A history-list
	// failure here is logged, not surfaced as a request error: the
	// detail itself already rendered successfully above, so the
	// response's status code is already committed.
	history, herr := s.opts.Feedback.ListForSession(r.Context(), ident.ID, sid)
	if herr != nil {
		slog.Error("list feedback history", "err", herr)
		return
	}
	RenderPartial(w, r, "feedback_history", toFeedbackHistoryView(sid, fb.ID, history, true))
}

// feedbackShow handles a feedback history row's click (Phase 4 Task W
// item 2): a GET (back/forward-friendly, per the brief) that loads one
// past review into #feedback-results exactly as if it had just been
// requested — same "feedback" partial, same socratic gate (see
// feedback.Service.GetFeedback's doc comment).
func (s *Server) feedbackShow(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "feedbackID")

	fb, err := s.opts.Feedback.GetFeedback(r.Context(), ident.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load feedback", http.StatusInternalServerError)
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
