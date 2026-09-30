// reading.go serves the 読解 pipeline: the /reading pages (list, detail,
// EPUB download and the learner's actions on an edition) and the JSON
// API the Chrome extension drives (/api/v1/reading/...). Both are thin:
// every rule — idempotency, what "ready" means, duplicate-delivery
// prevention, identity scoping — lives in application/reading.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	appreading "github.com/mikeyaustin/jlp/internal/application/reading"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// --- shared --------------------------------------------------------------

// readingStatusLabels renders an edition's status for a learner.
var readingStatusLabels = map[reading.EditionStatus]string{
	reading.EditionPending:   "作成待ち",
	reading.EditionAnalysing: "作成中",
	reading.EditionReady:     "完成",
	reading.EditionFailed:    "失敗",
}

var deliveryStatusLabels = map[reading.DeliveryStatus]string{
	reading.DeliveryPending: "送信待ち",
	reading.DeliverySending: "送信中",
	reading.DeliverySent:    "Kindleに送信済み",
	reading.DeliveryFailed:  "送信失敗",
}

func editionStatusLabel(s reading.EditionStatus) string {
	if l, ok := readingStatusLabels[s]; ok {
		return l
	}
	return string(s)
}

func deliveryStatusLabel(s reading.DeliveryStatus) string {
	if l, ok := deliveryStatusLabels[s]; ok {
		return l
	}
	return string(s)
}

// isReadingInputError reports whether err is the learner's to fix (a
// 400), not the server's (a 500).
func isReadingInputError(err error) bool {
	for _, e := range []error{reading.ErrEmptyContent, reading.ErrArticleTooLarge, reading.ErrInvalidURL} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// parsePublished accepts what pages actually carry in
// article:published_time and friends: RFC 3339, or a bare date. Anything
// else is dropped rather than rejected — a missing date costs nothing,
// refusing an article over its metadata would.
func parsePublished(s string) *time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// --- JSON API ------------------------------------------------------------

// submitArticleRequestDTO is POST /api/v1/reading/articles' body.
// Selection wins over Content when both are sent: a learner who
// highlighted a passage chose it, while Content is the extension's best
// guess at the page's article body.
type submitArticleRequestDTO struct {
	URL         string          `json:"url"`
	Title       string          `json:"title"`
	Source      string          `json:"source"`
	Author      string          `json:"author"`
	PublishedAt string          `json:"published_at"`
	Content     string          `json:"content"`
	Selection   string          `json:"selection"`
	Figures     []figureMetaDTO `json:"figures"`
	Deliver     bool            `json:"deliver"`
}

type readingArticleDTO struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Source string `json:"source"`
	URL    string `json:"url"`
	Chars  int    `json:"chars"`
}

type readingDeliveryDTO struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	StatusLabel string     `json:"status_label"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
}

type readingEditionDTO struct {
	ID              string               `json:"id"`
	ArticleID       string               `json:"article_id"`
	Status          string               `json:"status"`
	StatusLabel     string               `json:"status_label"`
	Terminal        bool                 `json:"terminal"`
	Attempts        int                  `json:"attempts"`
	LastError       string               `json:"last_error,omitempty"`
	Vocabulary      int                  `json:"vocabulary"`
	FigureCount     int                  `json:"figure_count"`
	PageURL         string               `json:"page_url"`
	EpubURL         string               `json:"epub_url,omitempty"`
	DeliveryEnabled bool                 `json:"delivery_enabled"`
	Deliveries      []readingDeliveryDTO `json:"deliveries"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

type submitArticleResponseDTO struct {
	Article   readingArticleDTO   `json:"article"`
	Edition   readingEditionDTO   `json:"edition"`
	Duplicate bool                `json:"duplicate"`
	Delivery  *readingDeliveryDTO `json:"delivery,omitempty"`
	// Figures were attached; FiguresRejected did not pass validation.
	Figures         int `json:"figures"`
	FiguresRejected int `json:"figures_rejected"`
}

func toReadingDeliveryDTO(d reading.Delivery) readingDeliveryDTO {
	return readingDeliveryDTO{
		ID: d.ID, Status: string(d.Status), StatusLabel: deliveryStatusLabel(d.Status),
		Attempts: d.Attempts, LastError: d.LastError, CreatedAt: d.CreatedAt, SentAt: d.SentAt,
	}
}

func (s *Server) toReadingEditionDTO(e reading.StudyEdition, dls []reading.Delivery, figureCount int) readingEditionDTO {
	dto := readingEditionDTO{
		ID: e.ID, ArticleID: e.ArticleID, Status: string(e.Status), StatusLabel: editionStatusLabel(e.Status),
		Terminal: e.Terminal(), Attempts: e.Attempts, LastError: e.LastError,
		// Relative on purpose: the extension joins it to the base URL the
		// learner configured, which is the only URL it knows is right.
		PageURL:         "/reading/" + e.ID,
		DeliveryEnabled: s.opts.Reading.DeliveryEnabled(),
		FigureCount:     figureCount,
		Deliveries:      make([]readingDeliveryDTO, 0, len(dls)),
		CreatedAt:       e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
	if e.Status == reading.EditionReady {
		dto.EpubURL = "/reading/" + e.ID + "/epub"
	}
	if e.Lesson != nil {
		dto.Vocabulary = len(e.Lesson.Vocabulary)
	}
	for _, d := range dls {
		dto.Deliveries = append(dto.Deliveries, toReadingDeliveryDTO(d))
	}
	return dto
}

type figureMetaDTO struct {
	Caption        string `json:"caption"`
	Alt            string `json:"alt"`
	AfterParagraph int    `json:"after_paragraph"`
	AfterText      string `json:"after_text"`
	Lead           bool   `json:"lead"`
	// InText defaults to true; the extension sends false for a page's
	// og:image used only as the cover.
	InText *bool `json:"in_text"`
}

// maxMultipartSubmitBytes bounds POST /api/v1/reading/articles when it
// carries images: 12 figures at the client's 1200 px downscale are well
// under this; the JSON form keeps maxRequestBodyBytes.
const maxMultipartSubmitBytes = 15 << 20

// decodeSubmit reads the submit request in either form. Images are
// matched to metadata.figures by index (image-0 is figures[0]); a
// figure whose image part is missing is dropped.
func decodeSubmit(w http.ResponseWriter, r *http.Request) (submitArticleRequestDTO, []reading.FigureDraft, error) {
	var req submitArticleRequestDTO
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		return req, nil, decodeJSON(w, r, &req)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartSubmitBytes)
	if err := r.ParseMultipartForm(maxMultipartSubmitBytes); err != nil {
		return req, nil, err
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	if err := json.Unmarshal([]byte(r.FormValue("metadata")), &req); err != nil {
		return req, nil, err
	}
	var figs []reading.FigureDraft
	for i, m := range req.Figures {
		fhs := r.MultipartForm.File[fmt.Sprintf("image-%d", i)]
		if len(fhs) == 0 {
			continue
		}
		f, err := fhs[0].Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, reading.MaxFigureBytes+1))
		_ = f.Close()
		if err != nil {
			continue
		}
		inText := m.InText == nil || *m.InText
		figs = append(figs, reading.FigureDraft{Caption: m.Caption, Alt: m.Alt, AfterParagraph: m.AfterParagraph,
			AfterText: m.AfterText, Lead: m.Lead, InText: inText, Data: data})
	}
	return req, figs, nil
}

// readingFigure handles GET /reading/articles/{id}/figures/{n}: one of
// the learner's article images. The bytes never change for a given
// (article, ordinal), so they are cached for a year and revalidated by
// their sha256.
func (s *Server) readingFigure(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n < 0 {
		http.NotFound(w, r)
		return
	}
	f, err := s.opts.Reading.Figure(r.Context(), ident.ID, chi.URLParam(r, "id"), n)
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
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(f.Data)
}

// apiReadingSubmit handles POST /api/v1/reading/articles: 202 when a
// new study edition was queued, 200 when the same article (same text,
// same learner) was already in — the extension shows the existing
// edition either way.
func (s *Server) apiReadingSubmit(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	req, figs, err := decodeSubmit(w, r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	content := req.Content
	if strings.TrimSpace(req.Selection) != "" {
		content = req.Selection
		// Figure positions refer to the whole article, so only the lead
		// survives, and only as the cover.
		kept := figs[:0]
		for _, f := range figs {
			if f.Lead {
				f.InText = false
				kept = append(kept, f)
			}
		}
		figs = kept
	}
	res, err := s.opts.Reading.Submit(r.Context(), ident.ID, reading.Draft{
		SourceURL: req.URL, SourceName: req.Source, Title: req.Title, Author: req.Author,
		PublishedAt: parsePublished(req.PublishedAt), Content: content, Figures: figs,
	}, appreading.SubmitOptions{Deliver: req.Deliver})
	if err != nil {
		if isReadingInputError(err) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not submit article")
		return
	}
	out := submitArticleResponseDTO{
		Article: readingArticleDTO{
			ID: res.Article.ID, Title: res.Article.Title, Source: res.Article.SourceName,
			URL: res.Article.SourceURL, Chars: res.Article.Runes(),
		},
		Edition:   s.toReadingEditionDTO(res.Edition, nil, res.Figures),
		Duplicate: res.Duplicate,
		Figures:   res.Figures, FiguresRejected: res.FiguresRejected,
	}
	if res.Delivery != nil {
		d := toReadingDeliveryDTO(*res.Delivery)
		out.Delivery = &d
	}
	status := http.StatusAccepted
	if res.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// apiReadingEdition handles GET /api/v1/reading/editions/{id} — what the
// extension polls while the edition is being written.
func (s *Server) apiReadingEdition(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	d, err := s.opts.Reading.Edition(r.Context(), ident.ID, chi.URLParam(r, "id"))
	if errors.Is(err, storage.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "edition not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not load edition")
		return
	}
	writeJSON(w, http.StatusOK, s.toReadingEditionDTO(d.Edition, d.Deliveries, len(d.Figures)))
}

// apiReadingDeliver handles POST /api/v1/reading/editions/{id}/deliver.
func (s *Server) apiReadingDeliver(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	d, created, err := s.opts.Reading.Deliver(r.Context(), ident.ID, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "edition not found")
	case errors.Is(err, appreading.ErrDeliveryNotConfigured):
		writeAPIError(w, http.StatusServiceUnavailable, "Kindle delivery is not configured on this server")
	case errors.Is(err, appreading.ErrNotReady):
		writeAPIError(w, http.StatusConflict, "the study edition is not ready yet")
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "could not queue delivery")
	default:
		status := http.StatusAccepted
		if !created {
			status = http.StatusOK
		}
		writeJSON(w, status, toReadingDeliveryDTO(d))
	}
}

// --- HTML ----------------------------------------------------------------

type readingListRow struct {
	EditionID, ArticleID, Title, Source, SourceURL string
	Status, StatusLabel, DeliveryLabel             string
	Created, Published                             string
	DeleteAction                                   string
}

// readingList renders /reading: the learner's articles, newest first,
// and the form to add one by pasting.
func (s *Server) readingList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	s.renderReadingList(w, r, ident.ID, "", readingFormValues{})
}

type readingFormValues struct{ Title, URL, Source, Content string }

func (s *Server) renderReadingList(w http.ResponseWriter, r *http.Request, identity learner.IdentityID, formError string, values readingFormValues) {
	list, err := s.opts.Reading.List(r.Context(), identity)
	if err != nil {
		http.Error(w, "could not load articles", http.StatusInternalServerError)
		return
	}
	rows := make([]readingListRow, 0, len(list))
	for _, e := range list {
		row := readingListRow{
			EditionID: e.EditionID, ArticleID: e.ArticleID, Title: e.Title, Source: e.SourceName, SourceURL: e.SourceURL,
			Status: string(e.Status), StatusLabel: editionStatusLabel(e.Status),
			Created:      e.CreatedAt.Format("2006-01-02 15:04"),
			DeleteAction: "/reading/articles/" + e.ArticleID + "/delete",
		}
		if e.DeliveryStatus != "" {
			row.DeliveryLabel = deliveryStatusLabel(e.DeliveryStatus)
		}
		if e.PublishedAt != nil {
			row.Published = e.PublishedAt.Format("2006-01-02")
		}
		rows = append(rows, row)
	}
	ident, _ := IdentityFrom(r.Context())
	s.render(w, r, "reading", map[string]any{
		"Title":           "読解",
		"Identity":        ident,
		"Articles":        rows,
		"DeliveryEnabled": s.opts.Reading.DeliveryEnabled(),
		"RestoreAction":   undoArticleRestoreAction(r),
		"FormError":       formError,
		"FormValues":      values,
	})
}

// undoArticleRestoreAction is undoRestoreAction for /reading, whose
// deletes are keyed by article, not by the edition the rows link to.
func undoArticleRestoreAction(r *http.Request) string {
	return undoRestoreAction(r, "/reading/articles")
}

// readingCreate handles the paste form: POST /reading.
func (s *Server) readingCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	values := readingFormValues{
		Title: r.FormValue("title"), URL: r.FormValue("url"), Source: r.FormValue("source"), Content: r.FormValue("content"),
	}
	res, err := s.opts.Reading.Submit(r.Context(), ident.ID, reading.Draft{
		SourceURL: values.URL, SourceName: values.Source, Title: values.Title, Content: values.Content,
	}, appreading.SubmitOptions{Deliver: r.FormValue("deliver") == "on"})
	if err != nil {
		if isReadingInputError(err) {
			w.WriteHeader(http.StatusBadRequest)
			s.renderReadingList(w, r, ident.ID, readingErrorMessage(err), values)
			return
		}
		http.Error(w, "could not submit article", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/reading/"+res.Edition.ID, http.StatusSeeOther)
}

// readingErrorMessage is the learner-facing wording of a validation
// error.
func readingErrorMessage(err error) string {
	switch {
	case errors.Is(err, reading.ErrEmptyContent):
		return "本文が空です。記事の本文を貼り付けてください。"
	case errors.Is(err, reading.ErrArticleTooLarge):
		return "本文が長すぎます。読みたい部分だけを貼り付けてください。"
	case errors.Is(err, reading.ErrInvalidURL):
		return "URLは http:// または https:// で始まる完全なURLにしてください。"
	}
	return err.Error()
}

// readingVocabView is one 重要語彙 entry as the detail page shows it.
type readingVocabView struct {
	reading.VocabularyItem
	Known bool
}

// readingDetail renders one study edition. While the edition is still
// being written, the page refreshes itself every few seconds (a plain
// meta refresh — no script needed for a page that is waiting).
func (s *Server) readingDetail(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	d, err := s.opts.Reading.Edition(r.Context(), ident.ID, chi.URLParam(r, "id"))
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load edition", http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Title":           d.Article.Title,
		"Identity":        ident,
		"Article":         d.Article,
		"Edition":         d.Edition,
		"StatusLabel":     editionStatusLabel(d.Edition.Status),
		"Ready":           d.Edition.Status == reading.EditionReady,
		"Failed":          d.Edition.Status == reading.EditionFailed,
		"Waiting":         !d.Edition.Terminal(),
		"DeliveryEnabled": s.opts.Reading.DeliveryEnabled(),
		"Created":         d.Article.CreatedAt.Format("2006-01-02 15:04"),
		"DeleteAction":    "/reading/articles/" + d.Article.ID + "/delete",
		"Notice":          readingNotice(r.URL.Query()),
	}
	if d.Article.PublishedAt != nil {
		data["Published"] = d.Article.PublishedAt.Format("2006-01-02")
	}
	var dls []map[string]any
	for _, dl := range d.Deliveries {
		v := map[string]any{"Label": deliveryStatusLabel(dl.Status), "Status": string(dl.Status), "Created": dl.CreatedAt.Format("2006-01-02 15:04"), "Error": dl.LastError}
		if dl.SentAt != nil {
			v["Sent"] = dl.SentAt.Format("2006-01-02 15:04")
		}
		dls = append(dls, v)
	}
	data["Deliveries"] = dls
	if d.Edition.Lesson != nil {
		l := d.Edition.Lesson
		vocab := make([]readingVocabView, 0, len(l.Vocabulary))
		for _, v := range l.Vocabulary {
			vocab = append(vocab, readingVocabView{VocabularyItem: v, Known: d.Known[v.Expression]})
		}
		data["Lesson"] = l
		data["Vocabulary"] = vocab
	}
	var vocabForBody []reading.VocabularyItem
	if d.Edition.Lesson != nil {
		vocabForBody = d.Edition.Lesson.Vocabulary
	}
	data["Blocks"] = reading.Layout(d.Article.Paragraphs, d.Figures, vocabForBody)
	s.render(w, r, "reading_detail", data)
}

// readingNotice turns an action's redirect query into the one-line
// confirmation the detail page shows.
func readingNotice(q url.Values) string {
	switch {
	case q.Get("delivery") == "queued":
		return "Kindleへの送信を開始しました。"
	case q.Get("delivery") == "inflight":
		return "この版はすでに送信中です。"
	case q.Has("vocab"):
		return fmt.Sprintf("語彙リストに%s語を追加しました。", q.Get("vocab"))
	case q.Has("cards"):
		n, _ := strconv.Atoi(q.Get("cards"))
		if n == 0 {
			return "Ankiカードはすでに作成済みです。"
		}
		return fmt.Sprintf("Ankiカードを%d枚作成しました（/ankiで確認できます）。", n)
	}
	return ""
}

// readingEpub handles GET /reading/{id}/epub.
func (s *Server) readingEpub(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	book, err := s.opts.Reading.RenderEbook(r.Context(), ident.ID, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, storage.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, appreading.ErrNotReady):
		http.Error(w, "the study edition is not ready yet", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "could not render ebook", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", book.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": book.Filename}))
	w.Header().Set("Content-Length", strconv.Itoa(len(book.Data)))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(book.Data)
}

// readingAction wraps the detail page's POST buttons: run fn, map the
// pipeline's errors, redirect back to the page with a notice.
func (s *Server) readingAction(fn func(r *http.Request, identity learner.IdentityID, id string) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, _ := IdentityFrom(r.Context())
		id := chi.URLParam(r, "id")
		target, err := fn(r, ident.ID, id)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			http.NotFound(w, r)
		case errors.Is(err, appreading.ErrNotReady):
			http.Error(w, "the study edition is not ready yet", http.StatusConflict)
		case errors.Is(err, appreading.ErrDeliveryNotConfigured):
			http.Error(w, "Kindle delivery is not configured", http.StatusServiceUnavailable)
		case err != nil:
			http.Error(w, "could not complete the action", http.StatusInternalServerError)
		default:
			http.Redirect(w, r, target, http.StatusSeeOther)
		}
	}
}

func (s *Server) readingDeliver(r *http.Request, identity learner.IdentityID, id string) (string, error) {
	_, created, err := s.opts.Reading.Deliver(r.Context(), identity, id)
	state := "queued"
	if !created {
		state = "inflight"
	}
	return "/reading/" + id + "?delivery=" + state, err
}

func (s *Server) readingRegenerate(r *http.Request, identity learner.IdentityID, id string) (string, error) {
	e, err := s.opts.Reading.Regenerate(r.Context(), identity, id)
	return "/reading/" + e.ID, err
}

func (s *Server) readingAddVocabulary(r *http.Request, identity learner.IdentityID, id string) (string, error) {
	n, err := s.opts.Reading.AddVocabulary(r.Context(), identity, id)
	return "/reading/" + id + "?vocab=" + strconv.Itoa(n), err
}

func (s *Server) readingAnki(r *http.Request, identity learner.IdentityID, id string) (string, error) {
	n, err := s.opts.Reading.CreateAnkiCards(r.Context(), identity, id)
	return "/reading/" + id + "?cards=" + strconv.Itoa(n), err
}

// readingDelete handles POST /reading/articles/{id}/delete — the
// article and every edition of it. Same identity-from-context and
// 404-for-someone-else's rule as every other delete.
func (s *Server) readingDelete(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := s.opts.Reading.Delete(r.Context(), ident.ID, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not delete article", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/reading?"+url.Values{"undo": {id}}.Encode(), http.StatusSeeOther)
}

func (s *Server) readingRestore(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if err := s.opts.Reading.Restore(r.Context(), ident.ID, chi.URLParam(r, "id")); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not restore article", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/reading", http.StatusSeeOther)
}
