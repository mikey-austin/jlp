// drafts.go serves 読解 drafts: the JSON API the extension and the phone
// pages append captured pages through, and the server-rendered review
// page where the learner drops junk blocks before one send. Like
// reading.go it is thin — page replacement, the 10-page cap, what a send
// keeps and the Submit pipeline itself live in application/reading.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	appreading "github.com/mikeyaustin/jlp/internal/application/reading"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const draftFullMessage = "下書きは10ページまでです"

// --- JSON API ------------------------------------------------------------

type draftStatusDTO struct {
	DraftID         string `json:"draft_id"`
	Pages           int    `json:"pages"`
	Paragraphs      int    `json:"paragraphs"`
	KeptParagraphs  int    `json:"kept_paragraphs"`
	Images          int    `json:"images"`
	KeptImages      int    `json:"kept_images"`
	Chars           int    `json:"chars"`
	FiguresRejected int    `json:"figures_rejected,omitempty"`
	ReviewURL       string `json:"review_url"`
}

func toDraftStatusDTO(st appreading.DraftStatus) draftStatusDTO {
	sum := st.Summary
	return draftStatusDTO{
		DraftID: st.Draft.ID, Pages: sum.Pages,
		Paragraphs: sum.Paragraphs, KeptParagraphs: sum.KeptParagraphs,
		Images: sum.Images, KeptImages: sum.KeptImages, Chars: sum.Chars,
		FiguresRejected: st.Rejected, ReviewURL: "/reading/drafts/" + st.Draft.ID,
	}
}

// apiDraftAddPart handles POST /api/v1/reading/drafts/active/parts: one
// captured page appended to the learner's draft. The body is the article
// submit body (JSON or multipart), and its url is the page's identity —
// sending the same url again replaces that page rather than doubling it.
func (s *Server) apiDraftAddPart(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	req, figs, err := decodeSubmit(w, r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	st, err := s.opts.Reading.AddDraftPage(r.Context(), ident.ID, appreading.DraftPage{
		Meta: reading.DraftMeta{
			Title: req.Title, SourceName: req.Source, SourceURL: req.URL,
			Author: req.Author, PublishedAt: parsePublished(req.PublishedAt),
		},
		PageURL: req.URL, Content: req.Content, Selection: req.Selection, Figures: figs,
	})
	switch {
	case errors.Is(err, reading.ErrDraftFull):
		writeAPIError(w, http.StatusBadRequest, draftFullMessage)
	case err != nil && isReadingInputError(err):
		writeAPIError(w, http.StatusBadRequest, readingErrorMessage(err))
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "could not add page to draft")
	default:
		writeJSON(w, http.StatusOK, toDraftStatusDTO(st))
	}
}

func (s *Server) apiDraftActive(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	st, err := s.opts.Reading.ActiveDraft(r.Context(), ident.ID)
	if errors.Is(err, storage.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "no active draft")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not load draft")
		return
	}
	writeJSON(w, http.StatusOK, toDraftStatusDTO(st))
}

func (s *Server) apiDraftDiscard(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	st, err := s.opts.Reading.ActiveDraft(r.Context(), ident.ID)
	if err == nil {
		err = s.opts.Reading.DiscardDraft(r.Context(), ident.ID, st.Draft.ID)
	}
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "no active draft")
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "could not discard draft")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- review page ---------------------------------------------------------

// draftStatusView is the part of the review page a toggle changes: the
// summary line, the warnings and whether sending is possible. OOB makes
// each piece an htmx out-of-band swap, so one toggle response updates
// them all beside the re-rendered row.
type draftStatusView struct {
	ID           string
	Summary      string
	Warnings     []string
	SendDisabled bool
	OOB          bool
}

type draftBlockView struct {
	DraftID  string
	Seq      int
	IsImage  bool
	Text     string
	Alt      string
	Excluded bool
}

type draftPageView struct {
	Number int
	URL    string
	Blocks []draftBlockView
}

func draftStatus(rv appreading.DraftReview, oob bool) draftStatusView {
	sum := rv.Summary
	v := draftStatusView{
		ID: rv.Draft.ID, OOB: oob,
		Summary: fmt.Sprintf("%dページ・段落 %d/%d・画像 %d/%d・%d字",
			sum.Pages, sum.KeptParagraphs, sum.Paragraphs, sum.KeptImages, sum.Images, sum.Chars),
		SendDisabled: rv.TooLong || sum.KeptParagraphs == 0,
	}
	if rv.TooLong {
		v.Warnings = append(v.Warnings, fmt.Sprintf("長すぎます（上限 %d字）— 段落を除外してください", rv.MaxRunes))
	}
	if rv.TooManyImages {
		v.Warnings = append(v.Warnings, fmt.Sprintf("画像は最初の%d枚だけ使われます", reading.MaxFigures))
	}
	return v
}

func draftBlockFor(draftID string, b reading.DraftBlock) draftBlockView {
	alt := b.Figure.Alt
	if alt == "" {
		alt = b.Figure.Caption
	}
	return draftBlockView{DraftID: draftID, Seq: b.Seq, IsImage: b.Kind == reading.BlockImage, Text: b.Text, Alt: alt, Excluded: b.Excluded}
}

// draftPages groups blocks under their captured page, numbered in the
// order they appear rather than by the stored Page value, so the
// headings read 1ページ目, 2ページ目 whatever the repository counts from.
func draftPages(rv appreading.DraftReview) []draftPageView {
	var pages []draftPageView
	last := -1
	for _, b := range rv.Blocks {
		if len(pages) == 0 || b.Page != last {
			pages = append(pages, draftPageView{Number: len(pages) + 1, URL: b.PageURL})
			last = b.Page
		}
		p := &pages[len(pages)-1]
		p.Blocks = append(p.Blocks, draftBlockFor(rv.Draft.ID, b))
	}
	return pages
}

func (s *Server) renderDraft(w http.ResponseWriter, r *http.Request, ident learner.Identity, rv appreading.DraftReview, formError string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "reading_draft", map[string]any{
		"Title": "下書き", "Identity": ident, "Draft": rv.Draft,
		"DraftTitle": rv.Draft.Meta.Title, "Pages": draftPages(rv),
		"Status": draftStatus(rv, false), "FormError": formError,
		"DeliveryEnabled": s.opts.Reading.DeliveryEnabled(),
	})
}

func (s *Server) draftGone(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, "reading_draft", map[string]any{"Title": "下書き", "Identity": ident, "Gone": true})
}

// draftFailed maps a service error for the review routes: someone
// else's or a gone draft is the 404 page, anything else a 500.
func (s *Server) draftFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		// htmx does not swap a 4xx, so a tap on a gone draft would do
		// nothing; send the browser to the page that says so.
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", "/reading/drafts/"+chi.URLParam(r, "id"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.draftGone(w, r)
		return
	}
	http.Error(w, "could not complete the action", http.StatusInternalServerError)
}

func (s *Server) readingDraft(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	rv, err := s.opts.Reading.Draft(r.Context(), ident.ID, chi.URLParam(r, "id"))
	if err != nil {
		s.draftFailed(w, r, err)
		return
	}
	s.renderDraft(w, r, ident, rv, "", http.StatusOK)
}

// readingDraftToggle flips one block in or out of the draft. htmx gets
// the re-rendered row plus the out-of-band summary, warnings and send
// button; a plain form post (no script) gets the page back.
func (s *Server) readingDraftToggle(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	seq, err := strconv.Atoi(chi.URLParam(r, "seq"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rv, err := s.opts.Reading.Draft(r.Context(), ident.ID, id)
	if err != nil {
		s.draftFailed(w, r, err)
		return
	}
	var cur *reading.DraftBlock
	for i := range rv.Blocks {
		if rv.Blocks[i].Seq == seq {
			cur = &rv.Blocks[i]
		}
	}
	if cur == nil {
		http.NotFound(w, r)
		return
	}
	rv, err = s.opts.Reading.SetDraftBlockExcluded(r.Context(), ident.ID, id, seq, !cur.Excluded)
	if err != nil {
		s.draftFailed(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/reading/drafts/"+id, http.StatusSeeOther)
		return
	}
	for _, b := range rv.Blocks {
		if b.Seq == seq {
			RenderPartial(w, r, "reading_draft_block", draftBlockFor(id, b))
		}
	}
	RenderPartial(w, r, "reading_draft_oob", draftStatus(rv, true))
}

func (s *Server) readingDraftTitle(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := s.opts.Reading.SetDraftTitle(r.Context(), ident.ID, id, r.FormValue("title")); err != nil {
		s.draftFailed(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/reading/drafts/"+id, http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readingDraftSend builds the article from the kept blocks. A validation
// failure re-renders the review with the message and keeps the draft, so
// the learner fixes it and sends again.
func (s *Server) readingDraftSend(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	// The title input rides in the send form, so a title typed a moment
	// before the tap (or without script) is applied before building.
	if t := strings.TrimSpace(r.FormValue("title")); t != "" {
		if err := s.opts.Reading.SetDraftTitle(r.Context(), ident.ID, id, t); err != nil {
			s.draftFailed(w, r, err)
			return
		}
	}
	res, err := s.opts.Reading.SendDraft(r.Context(), ident.ID, id, r.FormValue("deliver") != "")
	if err == nil {
		http.Redirect(w, r, "/reading/"+res.Edition.ID, http.StatusSeeOther)
		return
	}
	if !isReadingInputError(err) {
		s.draftFailed(w, r, err)
		return
	}
	rv, derr := s.opts.Reading.Draft(r.Context(), ident.ID, id)
	if derr != nil {
		s.draftFailed(w, r, derr)
		return
	}
	s.renderDraft(w, r, ident, rv, readingErrorMessage(err), http.StatusBadRequest)
}

func (s *Server) readingDraftDiscard(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if err := s.opts.Reading.DiscardDraft(r.Context(), ident.ID, chi.URLParam(r, "id")); err != nil {
		s.draftFailed(w, r, err)
		return
	}
	http.Redirect(w, r, "/reading", http.StatusSeeOther)
}

// readingDraftImage serves one draft image. Unlike an article's figures
// these are not immutable: re-capturing a page renumbers its blocks, so
// the cache is private and always revalidated by the content hash.
func (s *Server) readingDraftImage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	seq, err := strconv.Atoi(chi.URLParam(r, "seq"))
	if err != nil || seq < 0 {
		http.NotFound(w, r)
		return
	}
	f, err := s.opts.Reading.DraftImage(r.Context(), ident.ID, chi.URLParam(r, "id"), seq)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load image", http.StatusInternalServerError)
		return
	}
	etag := `"` + f.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", f.MediaType)
	_, _ = w.Write(f.Data)
}
