package httpx

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ankiCardView is one storage.AnkiCard as the "anki_card" partial (and
// the /anki page's draft/approved lists) render it.
type ankiCardView struct {
	ID, Front, Back, Notes, Status string
}

func toAnkiCardView(c storage.AnkiCard) ankiCardView {
	return ankiCardView{ID: c.ID, Front: c.Front, Back: c.Back, Notes: c.Notes, Status: c.Status}
}

func toAnkiCardViews(cards []storage.AnkiCard) []ankiCardView {
	views := make([]ankiCardView, 0, len(cards))
	for _, c := range cards {
		views = append(views, toAnkiCardView(c))
	}
	return views
}

// ankiPage renders the /anki review queue: draft cards (承認/却下
// buttons) and approved cards (TSVエクスポート link, plus 「Ankiへ送信」
// when AnkiConnect is configured — see Options.AnkiConnectEnabled).
func (s *Server) ankiPage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	drafts, err := s.opts.AnkiCards.List(r.Context(), ident.ID, "draft")
	if err != nil {
		http.Error(w, "could not load draft cards", http.StatusInternalServerError)
		return
	}
	approved, err := s.opts.AnkiCards.List(r.Context(), ident.ID, "approved")
	if err != nil {
		http.Error(w, "could not load approved cards", http.StatusInternalServerError)
		return
	}

	Render(w, r, "anki", map[string]any{
		"Title":              "Anki",
		"Identity":           ident,
		"Drafts":             toAnkiCardViews(drafts),
		"Approved":           toAnkiCardViews(approved),
		"AnkiConnectEnabled": s.opts.AnkiConnectEnabled,
	})
}

// ankiStatus handles a draft card's 承認/却下 buttons: form field
// status = approved|rejected. Renders the "anki_card" partial with the
// updated status, replacing the card in place (hx-swap="outerHTML") —
// same convention as feedback.go's correctionStatus.
func (s *Server) ankiStatus(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	card, err := s.opts.Anki.SetStatus(r.Context(), ident.ID, id, r.FormValue("status"))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, appanki.ErrInvalidStatus) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not update card status", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "anki_card", toAnkiCardView(card))
}

// ankiExportTSV is a GET that MUTATES: it marks every currently
// approved card exported as part of building the download (see
// application/anki.Service.ExportTSV's doc comment for the atomicity/
// no-double-export contract this relies on). It's kept a GET — not a
// POST behind ordinary CSRF, which every other state-changing route in
// this group is — deliberately: a file download triggered by a plain
// link click (rather than an XHR/htmx form submit) is the ergonomic
// way to get a browser's native "Save As" flow, and GET is the only
// method a bare <a href> can drive.
//
// That ergonomic choice does NOT mean this route is CSRF-exempt,
// though: a cross-site page CAN make a victim's browser issue this GET
// carrying the victim's own session cookie (e.g.
// `window.open('https://host/anki/export.tsv')`), and because the
// response is Content-Disposition: attachment the victim's tab never
// visibly navigates — the unwanted "mark everything exported" mutation
// would be silent. csrf.go's mutatingGetPaths therefore opts this exact
// path back into the SAME Sec-Fetch-Site/Origin cross-site rejection
// every POST/PUT/PATCH/DELETE route already gets — see that var's doc
// comment. What remains true independent of that check: the mutation
// is idempotent per learner (a second immediate GET is a no-op — see
// the Service doc comment, and the second-GET assertion inside
// TestAnkiStatusApproveThenExportTSV in this package's tests) and
// identity-scoped (RequireIdentity gates this whole route group), so a
// same-site request — the only kind that reaches this handler at all
// now — can only ever affect its own caller's own already-approved
// cards.
func (s *Server) ankiExportTSV(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	tsv, _, err := s.opts.Anki.ExportTSV(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not export anki cards", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/tab-separated-values; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="jlp-anki.tsv"`)
	_, _ = w.Write(tsv)
}

// ankiPush handles the 「Ankiへ送信」 button: POST /anki/push, only ever
// reachable when Options.AnkiConnectEnabled (the template only renders
// the button then — see anki.html.tmpl), though the service itself
// still enforces the same gate independently (ErrAnkiConnectNotConfigured)
// so this handler is safe even if reached some other way. Renders the
// "anki_push_result" partial with a count or an error message. Added is
// included alongside Error too (not just on the success path):
// PushToAnkiConnect's partial-acceptance case (AnkiConnect accepted
// some but not all of the batch — see that method's own doc comment)
// still reports how many WERE accepted even though the whole batch gets
// reverted back to "approved", so the learner sees that count rather
// than a bare, uninformative failure message.
func (s *Server) ankiPush(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	added, err := s.opts.Anki.PushToAnkiConnect(r.Context(), ident.ID)
	if err != nil {
		RenderPartial(w, r, "anki_push_result", map[string]any{"Error": err.Error(), "Added": added})
		return
	}
	RenderPartial(w, r, "anki_push_result", map[string]any{"Added": added})
}

// correctionAnki handles a correction card's 「Ankiカード作成」 button:
// POST /corrections/{id}/anki, no form fields. Renders the "anki_toast"
// partial confirming the generated draft.
func (s *Server) correctionAnki(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")

	card, err := s.opts.Anki.GenerateFromCorrection(r.Context(), ident.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not generate anki card", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "anki_toast", toAnkiCardView(card))
}
