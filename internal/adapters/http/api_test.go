package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/analytics"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// postJSON issues a JSON POST request against h and returns the
// recorded response. body may be nil for an empty request.
func postJSON(t *testing.T, h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAPISessionsCreateThenGetReturnsSessionDTO covers the contract's
// core create-then-read loop: POST /api/v1/sessions with a JSON body
// returns 201 with an id and the exact sessionDTO json tags, and that
// session is visible via GET /api/v1/sessions and GET
// /api/v1/sessions/{id} — the same sessions.Service the HTML /sessions
// routes use (PRD §38: HTMX is not the domain boundary).
func TestAPISessionsCreateThenGetReturnsSessionDTO(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	body := []byte(`{"title":"旅行について書く","purpose":"Blog post","teacher_mode":"teacher","explanation_language":"both","strictness":"balanced"}`)
	rec := postJSON(t, h, "/api/v1/sessions", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/sessions status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID                  string `json:"id"`
		Title               string `json:"title"`
		Purpose             string `json:"purpose"`
		TeacherMode         string `json:"teacher_mode"`
		ExplanationLanguage string `json:"explanation_language"`
		Strictness          string `json:"strictness"`
		CreatedAt           string `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("could not decode response %s: %v", rec.Body.String(), err)
	}
	if created.ID == "" {
		t.Fatalf("created session id is empty: %s", rec.Body.String())
	}
	if created.Title != "旅行について書く" || created.Purpose != "Blog post" {
		t.Fatalf("created session mismatch: %+v", created)
	}
	if created.TeacherMode != "teacher" || created.ExplanationLanguage != "both" || created.Strictness != "balanced" {
		t.Fatalf("created session profile mismatch: %+v", created)
	}
	if created.CreatedAt == "" {
		t.Fatalf("created_at is empty: %s", rec.Body.String())
	}

	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/sessions status = %d, body=%s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), created.ID) {
		t.Fatalf("GET /api/v1/sessions missing created id %s: %s", created.ID, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), `"teacher_mode"`) {
		t.Fatalf("GET /api/v1/sessions missing snake_case teacher_mode key: %s", listRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+created.ID, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/sessions/{id} status = %d, body=%s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "旅行について書く") {
		t.Fatalf("GET /api/v1/sessions/{id} missing title: %s", getRec.Body.String())
	}
}

// TestAPISessionsCreateEmptyTitleReturnsBadRequest mirrors the HTML
// route's validation: sessions.Service.Create rejects an empty title,
// and the API surfaces that as 400 {"error":"..."}, not 500.
func TestAPISessionsCreateEmptyTitleReturnsBadRequest(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	rec := postJSON(t, h, "/api/v1/sessions", []byte(`{"title":"","purpose":"Diary"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode error body %s: %v", rec.Body.String(), err)
	}
	if got.Error == "" {
		t.Fatalf("error body missing error field: %s", rec.Body.String())
	}
}

// TestAPISessionsCreateMalformedJSONReturnsBadRequest: broken JSON is a
// client error, not a 500 — the "malformed JSON → 400" contract case.
func TestAPISessionsCreateMalformedJSONReturnsBadRequest(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	rec := postJSON(t, h, "/api/v1/sessions", []byte(`{"title": "unterminated`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body missing {\"error\":...}: %s", rec.Body.String())
	}
}

// TestAPISessionsCreateOversizedBodyReturns400 is the API equivalent of
// documents_test.go's TestDocumentsSaveOversizedBodyReturns400: a body
// over decodeJSON's maxRequestBodyBytes cap must fail cleanly as 400
// {"error":...}, not panic or hang buffering unbounded bytes.
func TestAPISessionsCreateOversizedBodyReturns400(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	oversized := []byte(`{"title":"` + strings.Repeat("a", maxRequestBodyBytes+1) + `"}`)
	rec := postJSON(t, h, "/api/v1/sessions", oversized)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body len=%d", rec.Code, rec.Body.Len())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body missing {\"error\":...}: %s", rec.Body.String())
	}
}

// TestAPISessionsCreateRepositoryErrorReturns500AndHidesDetail is the
// API equivalent of sessions_test.go's
// TestSessionsCreateRepositoryErrorReturns500AndHidesDetail: a
// repository failure must surface as a generic 500 {"error":"internal
// error"}, never echoing the underlying error's text.
func TestAPISessionsCreateRepositoryErrorReturns500AndHidesDetail(t *testing.T) {
	opts := testOptionsWithSessions()
	repo := newFakeSessionRepo()
	repo.createErr = errors.New("pq: connection refused to host db.internal:5432 user=jlp password=hunter2")
	opts.Sessions = sessions.NewService(repo)

	srv := NewServer(opts)
	h := srv.HandlerForTest()

	rec := postJSON(t, h, "/api/v1/sessions", []byte(`{"title":"旅行について書く","purpose":"Diary"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("body leaked underlying repository error: %s", rec.Body.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode error body %s: %v", rec.Body.String(), err)
	}
	if got.Error != "internal error" {
		t.Fatalf("error = %q, want %q", got.Error, "internal error")
	}
}

// TestAPISessionsGetCrossIdentityNotFound: a session owned by another
// identity 404s through the API exactly like the HTML route.
func TestAPISessionsGetCrossIdentityNotFound(t *testing.T) {
	opts := testOptionsWithSessions()
	created, err := opts.Sessions.Create(context.Background(), "someone-else", "他人のセッション", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(opts) // testOptions() authenticates as identity "dev"
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(created.ID), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPISessionsGetMalformedIDReturnsBadRequest: a path id that isn't
// even a well-formed uuid is a client error the API catches itself
// (400), rather than letting it fail deep inside a real uuid-keyed
// repository as an opaque 500.
func TestAPISessionsGetMalformedIDReturnsBadRequest(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/not-a-uuid", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPIFeedbackRequestReturnsCorrectionsWithReplacement is the
// frozen contract's headline case: a feedback POST against a seeded
// document (real feedback.Service + teacher.Agent over fakeai, the
// same collaborators feedback_test.go's HTML-route tests use) returns
// corrections[0].replacement == 面白かったです.
func TestAPIFeedbackRequestReturnsCorrectionsWithReplacement(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	reqBody, err := json.Marshal(map[string]any{
		"document_id": docID,
		"start":       0,
		"end":         runeLen,
		"text":        content,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := postJSON(t, h, "/api/v1/sessions/"+string(sess.ID)+"/feedback", reqBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		ID          string `json:"id"`
		Original    string `json:"original"`
		Corrected   string `json:"corrected"`
		AIRequestID string `json:"ai_request_id"`
		Corrections []struct {
			ID            string `json:"id"`
			Original      string `json:"original"`
			Replacement   string `json:"replacement"`
			Type          string `json:"type"`
			Severity      string `json:"severity"`
			ExplanationJA string `json:"explanation_ja"`
			ExplanationEN string `json:"explanation_en"`
			Status        string `json:"status"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response %s: %v", rec.Body.String(), err)
	}
	// ai_request_id is only populated by the observability decorator
	// (see internal/ports/ai.StructuredResponse.RequestID), which
	// feedbackTestServer's plain teacher.New(fakeai.New()) doesn't wrap
	// with — same as feedback_test.go's HTML-route tests, this asserts
	// the field exists and round-trips (possibly empty here), not that
	// it's populated.
	if got.ID == "" {
		t.Fatalf("feedback response missing id: %+v", got)
	}
	if len(got.Corrections) == 0 {
		t.Fatalf("feedback response has no corrections: %s", rec.Body.String())
	}
	if got.Corrections[0].Replacement != "面白かったです" {
		t.Fatalf("corrections[0].replacement = %q, want 面白かったです", got.Corrections[0].Replacement)
	}
	if got.Corrections[0].ExplanationJA == "" || got.Corrections[0].ExplanationEN == "" {
		t.Fatalf("corrections[0] missing explanations: %+v", got.Corrections[0])
	}
	if got.Corrections[0].Status != "presented" {
		t.Fatalf("corrections[0].status = %q, want presented", got.Corrections[0].Status)
	}
}

// TestAPIFeedbackRequestMalformedJSONReturnsBadRequest: broken JSON on
// the feedback endpoint is a 400, same as sessions create.
func TestAPIFeedbackRequestMalformedJSONReturnsBadRequest(t *testing.T) {
	h, sess, _ := feedbackTestServer(t, "とても面白いでした")

	rec := postJSON(t, h, "/api/v1/sessions/"+string(sess.ID)+"/feedback", []byte(`not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPICorrectionStatusAccepts covers POST
// /api/v1/corrections/{id}/status: the correction produced by the
// feedback call above can be marked accepted and the response is a
// correctionDTO reflecting that.
func TestAPICorrectionStatusAccepts(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	reqBody, _ := json.Marshal(map[string]any{
		"document_id": docID,
		"start":       0,
		"end":         runeLen,
		"text":        content,
	})
	fbRec := postJSON(t, h, "/api/v1/sessions/"+string(sess.ID)+"/feedback", reqBody)
	if fbRec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", fbRec.Code, fbRec.Body.String())
	}
	var fb struct {
		Corrections []struct {
			ID string `json:"id"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(fbRec.Body.Bytes(), &fb); err != nil {
		t.Fatal(err)
	}
	if len(fb.Corrections) == 0 {
		t.Fatalf("no corrections to accept: %s", fbRec.Body.String())
	}

	statusRec := postJSON(t, h, "/api/v1/corrections/"+fb.Corrections[0].ID+"/status", []byte(`{"status":"accepted"}`))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("POST correction status = %d, body=%s", statusRec.Code, statusRec.Body.String())
	}
	if !strings.Contains(statusRec.Body.String(), `"status":"accepted"`) {
		t.Fatalf("body missing accepted status: %s", statusRec.Body.String())
	}
}

// TestAPICorrectionStatusInvalidValueReturnsBadRequest mirrors the
// HTML route's validation for an unrecognized status value.
func TestAPICorrectionStatusInvalidValueReturnsBadRequest(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	reqBody, _ := json.Marshal(map[string]any{
		"document_id": docID,
		"start":       0,
		"end":         runeLen,
		"text":        content,
	})
	fbRec := postJSON(t, h, "/api/v1/sessions/"+string(sess.ID)+"/feedback", reqBody)
	var fb struct {
		Corrections []struct {
			ID string `json:"id"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(fbRec.Body.Bytes(), &fb); err != nil {
		t.Fatal(err)
	}

	statusRec := postJSON(t, h, "/api/v1/corrections/"+fb.Corrections[0].ID+"/status", []byte(`{"status":"bogus"}`))
	if statusRec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", statusRec.Code, statusRec.Body.String())
	}
}

// TestAPILearnerStatisticsReturnsDerivedFields covers GET
// /api/v1/learner/statistics: the same analytics.Service the home
// dashboard uses, with the response mirroring storage.Statistics in
// snake_case including the derived acceptance_rate and
// corrections_per_1000 fields.
func TestAPILearnerStatisticsReturnsDerivedFields(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{stats: storage.Statistics{
		RunesWritten:         250,
		SessionCount:         1,
		FeedbackRequests:     3,
		CorrectionsPresented: 4,
		CorrectionsAccepted:  3,
		CorrectionsRejected:  1,
		TopErrorTypes:        []storage.ErrorTypeCount{{Type: "conjugation", Count: 3}},
	}})
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/learner/statistics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		RunesWritten         int     `json:"runes_written"`
		SessionCount         int     `json:"session_count"`
		FeedbackRequests     int     `json:"feedback_requests"`
		CorrectionsPresented int     `json:"corrections_presented"`
		CorrectionsAccepted  int     `json:"corrections_accepted"`
		CorrectionsRejected  int     `json:"corrections_rejected"`
		AcceptanceRate       float64 `json:"acceptance_rate"`
		CorrectionsPer1000   float64 `json:"corrections_per_1000"`
		TopErrorTypes        []struct {
			Type  string `json:"type"`
			Count int    `json:"count"`
		} `json:"top_error_types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response %s: %v", rec.Body.String(), err)
	}
	if got.RunesWritten != 250 {
		t.Errorf("runes_written = %d, want 250", got.RunesWritten)
	}
	if got.AcceptanceRate != 0.75 {
		t.Errorf("acceptance_rate = %v, want 0.75 (3/(3+1))", got.AcceptanceRate)
	}
	if got.CorrectionsPer1000 != 16.0 {
		t.Errorf("corrections_per_1000 = %v, want 16.0 (4/250*1000)", got.CorrectionsPer1000)
	}
	if len(got.TopErrorTypes) != 1 || got.TopErrorTypes[0].Type != "conjugation" {
		t.Errorf("top_error_types = %+v, want [{conjugation 3}]", got.TopErrorTypes)
	}
}

// TestAPIRatingsCreateReturnsNoContent mirrors the HTML route's rating
// widget contract over JSON: a valid rating returns 204 with no body.
func TestAPIRatingsCreateReturnsNoContent(t *testing.T) {
	opts := aiTestOptions()
	srv := NewServer(opts)

	body := []byte(`{"ai_request_id":"11111111-1111-1111-1111-111111111111","rating":4}`)
	rec := postJSON(t, srv.HandlerForTest(), "/api/v1/ratings", body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

// TestAPIRatingsCreateMalformedAIRequestIDReturnsBadRequest mirrors the
// HTML route's ai_request_id uuid validation.
func TestAPIRatingsCreateMalformedAIRequestIDReturnsBadRequest(t *testing.T) {
	opts := aiTestOptions()
	srv := NewServer(opts)

	body := []byte(`{"ai_request_id":"not-a-uuid","rating":4}`)
	rec := postJSON(t, srv.HandlerForTest(), "/api/v1/ratings", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPIUnauthenticatedRequestReturnsJSON401 proves RequireIdentity
// emits {"error":"unauthorized"} JSON for /api/ paths instead of the
// plain-text 401 body the HTML routes still get — the mandate that lets
// a JSON client (this contract's whole point, and later the Chrome
// extension in Phase 3) branch on Content-Type reliably.
func TestAPIUnauthenticatedRequestReturnsJSON401(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Auth = fakeAuth{err: errors.New("no session cookie")}
	srv := NewServer(opts)

	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode error body %s: %v", rec.Body.String(), err)
	}
	if got.Error != "unauthorized" {
		t.Fatalf("error = %q, want %q", got.Error, "unauthorized")
	}
}

// TestHTMLUnauthenticatedRequestStaysPlainText is the regression guard
// for the change above: non-API routes must keep their existing
// plain-text 401 body (net/http's http.Error default), not switch to
// JSON just because RequireIdentity learned a new trick.
func TestHTMLUnauthenticatedRequestStaysPlainText(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Auth = fakeAuth{err: errors.New("no session cookie")}
	srv := NewServer(opts)

	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want plain text for HTML routes", ct)
	}
	if strings.TrimSpace(rec.Body.String()) != "unauthorized" {
		t.Fatalf("body = %q, want plain \"unauthorized\"", rec.Body.String())
	}
}
