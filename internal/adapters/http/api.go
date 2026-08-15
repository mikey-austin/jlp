// api.go is the versioned JSON API (/api/v1): thin handlers over the
// exact same application services the HTML routes in sessions.go,
// feedback.go, and ai.go use — sessions.Service, appwriting.Service,
// feedback.Service, analytics.Service, storage.AIRatingRepository.
// Proving that HTML and JSON can share one application layer is the
// point of this file (PRD §38): no new application-layer code is
// introduced here, only decode/encode plumbing and the stable DTOs
// below.
//
// Every handler follows the same shape: decode the JSON body (if any),
// validate/parse path params, call the shared service, encode the
// result. Errors are always {"error":"..."} with the appropriate
// status — storage.ErrNotFound as 404, validation failures as 400,
// anything else as 500.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// sessionDTO, feedbackDTO, and correctionDTO have frozen json tags —
// external clients (Phase 3's Chrome extension among them) depend on
// this exact shape.

type sessionDTO struct {
	ID                  string    `json:"id"`
	Title               string    `json:"title"`
	Purpose             string    `json:"purpose"`
	TeacherMode         string    `json:"teacher_mode"`
	ExplanationLanguage string    `json:"explanation_language"`
	Strictness          string    `json:"strictness"`
	CreatedAt           time.Time `json:"created_at"`
}

type feedbackDTO struct {
	ID          string          `json:"id"`
	Original    string          `json:"original"`
	Corrected   string          `json:"corrected"`
	AIRequestID string          `json:"ai_request_id"`
	Corrections []correctionDTO `json:"corrections"`
}

type correctionDTO struct {
	ID            string `json:"id"`
	Original      string `json:"original"`
	Replacement   string `json:"replacement"`
	Type          string `json:"type"`
	Severity      string `json:"severity"`
	ExplanationJA string `json:"explanation_ja"`
	ExplanationEN string `json:"explanation_en"`
	Status        string `json:"status"`
}

// errorTypeCountDTO and statisticsDTO mirror storage.Statistics in
// snake_case; unlike the three DTOs above their tags aren't frozen by
// the task brief, but they follow the same convention for consistency.
type errorTypeCountDTO struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type statisticsDTO struct {
	RunesWritten         int                 `json:"runes_written"`
	SessionCount         int                 `json:"session_count"`
	FeedbackRequests     int                 `json:"feedback_requests"`
	CorrectionsPresented int                 `json:"corrections_presented"`
	CorrectionsAccepted  int                 `json:"corrections_accepted"`
	CorrectionsRejected  int                 `json:"corrections_rejected"`
	AcceptanceRate       float64             `json:"acceptance_rate"`
	CorrectionsPer1000   float64             `json:"corrections_per_1000"`
	TopErrorTypes        []errorTypeCountDTO `json:"top_error_types"`
}

func toSessionDTO(sess session.Session) sessionDTO {
	return sessionDTO{
		ID:                  string(sess.ID),
		Title:               sess.Title,
		Purpose:             sess.Purpose,
		TeacherMode:         sess.Profile.TeacherMode,
		ExplanationLanguage: sess.Profile.ExplanationLanguage,
		Strictness:          sess.Profile.Strictness,
		CreatedAt:           sess.CreatedAt,
	}
}

func toCorrectionDTO(cv feedback.CorrectionView) correctionDTO {
	return correctionDTO{
		ID:            cv.ID,
		Original:      cv.Original,
		Replacement:   cv.Replacement,
		Type:          string(cv.Type),
		Severity:      string(cv.Severity),
		ExplanationJA: cv.Explanation.JA,
		ExplanationEN: cv.Explanation.EN,
		Status:        cv.Status,
	}
}

func toFeedbackDTO(fb feedback.Feedback) feedbackDTO {
	corrections := make([]correctionDTO, 0, len(fb.Corrections))
	for _, c := range fb.Corrections {
		corrections = append(corrections, toCorrectionDTO(c))
	}
	return feedbackDTO{
		ID:          fb.ID,
		Original:    fb.Original,
		Corrected:   fb.Corrected,
		AIRequestID: fb.AIRequestID,
		Corrections: corrections,
	}
}

func toStatisticsDTO(stats storage.Statistics) statisticsDTO {
	topErrors := make([]errorTypeCountDTO, 0, len(stats.TopErrorTypes))
	for _, e := range stats.TopErrorTypes {
		topErrors = append(topErrors, errorTypeCountDTO{Type: e.Type, Count: e.Count})
	}
	return statisticsDTO{
		RunesWritten:         stats.RunesWritten,
		SessionCount:         stats.SessionCount,
		FeedbackRequests:     stats.FeedbackRequests,
		CorrectionsPresented: stats.CorrectionsPresented,
		CorrectionsAccepted:  stats.CorrectionsAccepted,
		CorrectionsRejected:  stats.CorrectionsRejected,
		AcceptanceRate:       stats.AcceptanceRate,
		CorrectionsPer1000:   stats.CorrectionsPer1000,
		TopErrorTypes:        topErrors,
	}
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is already committed by this point, so a failure here
	// (e.g. the client disconnected mid-write) can't be turned into a
	// different HTTP response — log it and move on, matching Render's
	// treatment of a post-header ExecuteTemplate failure.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "err", err)
	}
}

// writeAPIError writes the API's uniform {"error":"..."} shape.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// maxRequestBodyBytes caps request bodies the app reads into memory —
// the JSON API's decodeJSON below and the HTML autosave handler
// (documents.go) both use it. 1 MiB comfortably covers any legitimate
// session/feedback/rating payload or writing-document autosave; beyond
// that a request is either misbehaving or malicious, and either way
// shouldn't be allowed to buffer unbounded bytes into memory.
const maxRequestBodyBytes = 1 << 20

// decodeJSON decodes r's body into v, after capping it to
// maxRequestBodyBytes via http.MaxBytesReader. Any failure — malformed
// JSON, an empty body, a type mismatch, or an oversized body — is
// reported as a single error the caller treats uniformly as 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	return json.NewDecoder(r.Body).Decode(v)
}

// parseUUIDParam parses id (a path or body value expected to be a
// uuid) and writes a 400 {"error":"..."} if it isn't one — the
// repositories backing these services key everything by uuid, so a
// malformed id would otherwise either false-negative as "not found" or,
// worse, fail deep inside a real adapter as an unhandled 500. Returns
// ok=false when it has already written the response.
func parseUUIDParam(w http.ResponseWriter, field, id string) (ok bool) {
	if _, err := uuid.Parse(id); err != nil {
		writeAPIError(w, http.StatusBadRequest, field+" must be a valid id")
		return false
	}
	return true
}

// apiSessionsList handles GET /api/v1/sessions: the caller's sessions,
// newest-first, via the same sessions.Service.List the HTML /sessions
// page uses.
func (s *Server) apiSessionsList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	list, err := s.opts.Sessions.List(r.Context(), ident.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not load sessions")
		return
	}
	dtos := make([]sessionDTO, 0, len(list))
	for _, sess := range list {
		dtos = append(dtos, toSessionDTO(sess))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// sessionCreateRequest is the JSON body POST /api/v1/sessions expects.
type sessionCreateRequest struct {
	Title               string `json:"title"`
	Purpose             string `json:"purpose"`
	TeacherMode         string `json:"teacher_mode"`
	ExplanationLanguage string `json:"explanation_language"`
	Strictness          string `json:"strictness"`
}

// apiSessionsCreate handles POST /api/v1/sessions via
// sessions.Service.Create — the same service and validation
// (non-empty title, defaulted profile fields) the HTML form POST uses.
func (s *Server) apiSessionsCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	var req sessionCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	profile := session.Profile{
		TeacherMode:         req.TeacherMode,
		ExplanationLanguage: req.ExplanationLanguage,
		Strictness:          req.Strictness,
	}
	sess, err := s.opts.Sessions.Create(r.Context(), ident.ID, req.Title, req.Purpose, profile)
	if err != nil {
		if errors.Is(err, sessions.ErrInvalidTitle) {
			writeAPIError(w, http.StatusBadRequest, sessions.ErrInvalidTitle.Error())
			return
		}
		// Anything else (a repository failure, most likely) is never
		// echoed back verbatim — see sessionsCreate's HTML counterpart for
		// the same reasoning.
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, toSessionDTO(sess))
}

// apiSessionsGet handles GET /api/v1/sessions/{id}.
func (s *Server) apiSessionsGet(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	idParam := chi.URLParam(r, "id")
	if !parseUUIDParam(w, "id", idParam) {
		return
	}
	sess, err := s.opts.Sessions.Get(r.Context(), ident.ID, session.ID(idParam))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not load session")
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(sess))
}

// feedbackCreateRequest is the JSON body POST
// /api/v1/sessions/{id}/feedback expects: document_id, start, end are
// rune offsets into the document, and text is the client's own copy of
// the selection (used when non-empty, same as the HTML form field).
type feedbackCreateRequest struct {
	DocumentID string `json:"document_id"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Text       string `json:"text"`
}

// apiFeedbackRequest handles POST /api/v1/sessions/{id}/feedback via
// feedback.Service.RequestFeedback — the same pipeline (Teacher agent
// review, persistence, learning events) the workspace's フィードバックを取得
// button drives.
func (s *Server) apiFeedbackRequest(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	idParam := chi.URLParam(r, "id")
	if !parseUUIDParam(w, "id", idParam) {
		return
	}
	var req feedbackCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "malformed request body")
		return
	}

	fb, err := s.opts.Feedback.RequestFeedback(r.Context(), feedback.Request{
		Identity:   ident.ID,
		SessionID:  session.ID(idParam),
		DocumentID: writing.DocumentID(req.DocumentID),
		Start:      req.Start,
		End:        req.End,
		Text:       req.Text,
	})
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		if errors.Is(err, feedback.ErrInvalidSelection) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not get feedback")
		return
	}
	writeJSON(w, http.StatusOK, toFeedbackDTO(fb))
}

// correctionStatusRequest is the JSON body POST
// /api/v1/corrections/{id}/status expects.
type correctionStatusRequest struct {
	Status string `json:"status"`
}

// apiCorrectionStatus handles POST /api/v1/corrections/{id}/status via
// feedback.Service.SetCorrectionStatus — the same accept/reject
// pipeline the correction cards' buttons drive.
func (s *Server) apiCorrectionStatus(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	idParam := chi.URLParam(r, "id")
	if !parseUUIDParam(w, "id", idParam) {
		return
	}
	var req correctionStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "malformed request body")
		return
	}

	cv, err := s.opts.Feedback.SetCorrectionStatus(r.Context(), ident.ID, idParam, req.Status)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		if errors.Is(err, feedback.ErrInvalidStatus) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not update correction status")
		return
	}
	writeJSON(w, http.StatusOK, toCorrectionDTO(cv))
}

// apiLearnerStatistics handles GET /api/v1/learner/statistics via the
// same analytics.Service.Statistics the home dashboard's stat tiles
// use.
func (s *Server) apiLearnerStatistics(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	stats, err := s.opts.Analytics.Statistics(r.Context(), ident.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not load statistics")
		return
	}
	writeJSON(w, http.StatusOK, toStatisticsDTO(stats))
}

// priorityDTO mirrors storage.Priority's teaching-relevant fields in
// snake_case — Task 5's brief pins this exact shape for
// GET /api/v1/learner/priorities. IdentityID and UpdatedAt are omitted:
// the caller already knows who they are (every /api/v1 route is
// identity-scoped) and the page has no use for the bookkeeping
// timestamp.
type priorityDTO struct {
	SubjectType string  `json:"subject_type"`
	Subject     string  `json:"subject"`
	Score       float64 `json:"score"`
	Reason      string  `json:"reason"`
}

func toPriorityDTO(p storage.Priority) priorityDTO {
	return priorityDTO{
		SubjectType: p.SubjectType,
		Subject:     p.Subject,
		Score:       p.Score,
		Reason:      p.Reason,
	}
}

// apiLearnerPriorities handles GET /api/v1/learner/priorities: the same
// storage.PriorityRepository.Top(N) the /learner page's table and
// feedback.Service's RecentErrors both read from.
func (s *Server) apiLearnerPriorities(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	top, err := s.opts.Priorities.Top(r.Context(), ident.ID, learnerPriorityLimit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not load priorities")
		return
	}
	dtos := make([]priorityDTO, 0, len(top))
	for _, p := range top {
		dtos = append(dtos, toPriorityDTO(p))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// ratingCreateRequest is the JSON body POST /api/v1/ratings expects.
type ratingCreateRequest struct {
	AIRequestID string `json:"ai_request_id"`
	Rating      int    `json:"rating"`
}

// apiRatingsCreate handles POST /api/v1/ratings via the same
// storage.AIRatingRepository.Upsert the feedback partial's star widget
// uses. One rating per (ai_request_id, identity); responds 204 with no
// body, matching the HTML route.
func (s *Server) apiRatingsCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	var req ratingCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Rating < 1 || req.Rating > 5 {
		writeAPIError(w, http.StatusBadRequest, "rating must be an integer between 1 and 5")
		return
	}
	if !parseUUIDParam(w, "ai_request_id", req.AIRequestID) {
		return
	}

	err := s.opts.AIRatings.Upsert(r.Context(), storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: req.AIRequestID,
		IdentityID:  ident.ID,
		Rating:      req.Rating,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		// Same ownership semantics as the HTML route's ratingsCreate:
		// Upsert verifies ai_request_id belongs to ident.ID via a join,
		// so a request that doesn't exist or belongs to someone else
		// comes back as ErrNotFound.
		if errors.Is(err, storage.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not save rating")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// vocabularyItemDTO mirrors vocabulary.Item's learner-facing fields in
// snake_case, the same shape every other DTO in this file uses.
type vocabularyItemDTO struct {
	ID                    string `json:"id"`
	Expression            string `json:"expression"`
	Reading               string `json:"reading"`
	Meaning               string `json:"meaning"`
	Kind                  string `json:"kind"`
	JLPTLevel             int    `json:"jlpt_level"`
	Source                string `json:"source"`
	Lookups               int    `json:"lookups"`
	Productions           int    `json:"productions"`
	SuccessfulProductions int    `json:"successful_productions"`
}

func toVocabularyItemDTO(item vocabulary.Item) vocabularyItemDTO {
	return vocabularyItemDTO{
		ID:                    item.ID,
		Expression:            item.Expression,
		Reading:               item.Reading,
		Meaning:               item.Meaning,
		Kind:                  string(item.Kind),
		JLPTLevel:             item.JLPTLevel,
		Source:                item.Source,
		Lookups:               item.Lookups,
		Productions:           item.Productions,
		SuccessfulProductions: item.SuccessfulProductions,
	}
}

// apiVocabularyIngest handles POST /api/v1/vocabulary/events (PRD
// §12) via appvocabulary.Service.Ingest — the same service the
// /vocabulary page's List reads back from (Options.Vocabulary is one
// instance shared by both routes). The request body IS
// appvocabulary.IngestEvent directly (its json tags ARE the wire
// contract PRD §12 pins), unlike every other POST handler in this
// file, which decodes into a local *Request DTO before translating
// into application-layer types — there is no translation step here to
// keep separate.
func (s *Server) apiVocabularyIngest(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	var ev appvocabulary.IngestEvent
	if err := decodeJSON(w, r, &ev); err != nil {
		writeAPIError(w, http.StatusBadRequest, "malformed request body")
		return
	}

	item, err := s.opts.Vocabulary.Ingest(r.Context(), ident.ID, ev)
	if err != nil {
		if errors.Is(err, appvocabulary.ErrUnsupportedType) || errors.Is(err, appvocabulary.ErrEmptyExpression) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not ingest vocabulary event")
		return
	}
	writeJSON(w, http.StatusCreated, toVocabularyItemDTO(item))
}
