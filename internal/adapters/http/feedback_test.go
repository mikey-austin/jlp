package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeGrammarRepo is a minimal storage.GrammarRepository double for the
// HTTP-layer tests: they exercise the feedback request/response cycle,
// not concept-tagging persistence (that contract lives in
// application/feedback/service_test.go), so ListConcepts returns an
// empty candidate list and every other method panics if ever called.
type fakeGrammarRepo struct{}

func (fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by feedback http tests")
}

func (fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) { return nil, nil }

func (fakeGrammarRepo) GetConcept(context.Context, string) (grammar.Concept, error) {
	panic("not used by feedback http tests")
}

func (fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by feedback http tests")
}

func (fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by feedback http tests")
}

// fakeFeedbackRepo is an in-memory storage.FeedbackRepository for
// HTTP-layer tests, mirroring the identity-scoped join
// application/feedback/service_test.go's double performs: a
// correction's identity is only known via the feedback record it
// belongs to.
type fakeFeedbackRepo struct {
	feedback    map[string]storage.FeedbackRecord
	corrections map[string]storage.CorrectionRecord
}

func newFakeFeedbackRepo() *fakeFeedbackRepo {
	return &fakeFeedbackRepo{
		feedback:    map[string]storage.FeedbackRecord{},
		corrections: map[string]storage.CorrectionRecord{},
	}
}

func (f *fakeFeedbackRepo) InsertFeedback(_ context.Context, rec storage.FeedbackRecord, corrections []storage.CorrectionRecord) error {
	f.feedback[rec.ID] = rec
	for _, c := range corrections {
		f.corrections[c.ID] = c
	}
	return nil
}

func (f *fakeFeedbackRepo) UpdateCorrectionStatus(_ context.Context, identity learner.IdentityID, correctionID, status string) (storage.CorrectionRecord, error) {
	c, ok := f.corrections[correctionID]
	if !ok {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	fb, ok := f.feedback[c.FeedbackID]
	if !ok || fb.IdentityID != identity {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	c.Status = status
	c.SessionID = fb.SessionID
	f.corrections[correctionID] = c
	return c, nil
}

// InsertCorrectionConcepts is a no-op here: the HTTP-layer tests assert
// on rendered correction-card HTML, not persisted concept rows — that
// contract lives in application/feedback/service_test.go.
func (f *fakeFeedbackRepo) InsertCorrectionConcepts(_ context.Context, _ string, _ []string, _ map[string]bool) error {
	return nil
}

// feedbackTestServer wires a real chi router with a real
// feedback.Service (a real teacher.Agent over fakeai — deterministic,
// no network) over in-memory repos, the same "real collaborators, fake
// edges" shape the rest of the HTTP-layer tests use. It creates a
// session, opens its document, and autosaves content into it, then
// returns the handler plus the session and document id the test posts
// against.
func feedbackTestServer(t *testing.T, content string) (http.Handler, session.Session, string) {
	t.Helper()
	opts := testOptions()
	sessionRepo := newFakeSessionRepo()
	docRepo := newFakeDocRepo()
	feedbackRepo := newFakeFeedbackRepo()
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())

	opts.Sessions = sessions.NewService(sessionRepo)
	opts.Writing = appwriting.NewService(docRepo, rec)
	opts.Events = events
	opts.Feedback = appfeedback.NewService(sessionRepo, docRepo, feedbackRepo, fakeGrammarRepo{}, teacher.New(fakeai.New()), rec)

	sess, err := opts.Sessions.Create(context.Background(), "dev", "日記", "Diary", session.Profile{
		TeacherMode:         "teacher",
		Strictness:          "balanced",
		ExplanationLanguage: "both",
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := opts.Writing.Open(context.Background(), "dev", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err = opts.Writing.Autosave(context.Background(), "dev", doc.ID, content)
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(opts)
	return srv.HandlerForTest(), sess, string(doc.ID)
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// extractCorrectionID pulls the id out of the first `id="corr-XXXX"`
// attribute in body, the way a browser's DOM would let a click handler
// find it.
func extractCorrectionID(t *testing.T, body string) string {
	t.Helper()
	const marker = `id="corr-`
	idx := strings.Index(body, marker)
	if idx == -1 {
		t.Fatalf("body missing correction id marker %q: %s", marker, body)
	}
	rest := body[idx+len(marker):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatalf("malformed correction id attribute: %s", body)
	}
	return rest[:end]
}

// TestFeedbackRequestThenAcceptCorrection is the core loop end to end:
// POST feedback on a selection containing the known-bad conjugation
// (see internal/adapters/fakeai) returns a rendered correction with
// the diff and both explanations, and posting "accepted" on that
// correction's id flips its rendered status.
func TestFeedbackRequestThenAcceptCorrection(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	form.Set("text", content)
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "面白かったです") {
		t.Fatalf("body missing corrected text 面白かったです: %s", body)
	}
	if !strings.Contains(body, "d-ins") {
		t.Fatalf("body missing d-ins diff span: %s", body)
	}
	if !strings.Contains(body, "d-del") {
		t.Fatalf("body missing d-del diff span: %s", body)
	}
	wantJA := "い形容詞の過去形は「〜かった」を使います。「面白い」→「面白かった」。"
	wantEN := "い-adjectives form the past tense with 〜かった, so 面白いでした must be 面白かったです."
	if !strings.Contains(body, wantJA) {
		t.Fatalf("body missing Japanese explanation %q: %s", wantJA, body)
	}
	if !strings.Contains(body, wantEN) {
		t.Fatalf("body missing English explanation %q: %s", wantEN, body)
	}
	if !strings.Contains(body, "conjugation") {
		t.Fatalf("body missing type badge conjugation: %s", body)
	}
	if !strings.Contains(body, "incorrect") {
		t.Fatalf("body missing severity badge incorrect: %s", body)
	}

	correctionID := extractCorrectionID(t, body)

	statusForm := url.Values{}
	statusForm.Set("status", "accepted")
	statusRec := postForm(t, h, "/corrections/"+correctionID+"/status", statusForm)

	if statusRec.Code != http.StatusOK {
		t.Fatalf("POST correction status = %d, body=%s", statusRec.Code, statusRec.Body.String())
	}
	if !strings.Contains(statusRec.Body.String(), `data-status="accepted"`) {
		t.Fatalf("body missing data-status=\"accepted\": %s", statusRec.Body.String())
	}
	if strings.Contains(statusRec.Body.String(), "納得した") {
		t.Fatalf("accepted card should not still show the accept button: %s", statusRec.Body.String())
	}
}

// TestFeedbackRequestEmptySelectionReviewsWholeDocument mirrors the
// "collapse selection, review whole document" browser flow: start==end
// finds the same correction.
func TestFeedbackRequestEmptySelectionReviewsWholeDocument(t *testing.T) {
	content := "昨日友達と映画を見に行って、とても面白いでした。"
	h, sess, docID := feedbackTestServer(t, content)

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", "0")
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "面白かったです") {
		t.Fatalf("whole-document review missing corrected text: %s", rec.Body.String())
	}
}

// TestFeedbackRequestCrossIdentitySessionNotFound: a session owned by
// another identity must 404, not leak feedback.
func TestFeedbackRequestCrossIdentitySessionNotFound(t *testing.T) {
	h, _, docID := feedbackTestServer(t, "とても面白いでした")

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", "1")
	rec := postForm(t, h, "/sessions/someone-elses-session/feedback", form)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestFeedbackRequestBadStartReturnsBadRequest: a non-integer start
// field is a client error, not a 500.
func TestFeedbackRequestBadStartReturnsBadRequest(t *testing.T) {
	h, sess, docID := feedbackTestServer(t, "とても面白いでした")

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "not-a-number")
	form.Set("end", "1")
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestCorrectionStatusInvalidValueReturnsBadRequest: the service
// rejects any status other than accepted/rejected.
func TestCorrectionStatusInvalidValueReturnsBadRequest(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)
	correctionID := extractCorrectionID(t, rec.Body.String())

	statusForm := url.Values{}
	statusForm.Set("status", "bogus")
	statusRec := postForm(t, h, "/corrections/"+correctionID+"/status", statusForm)

	if statusRec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", statusRec.Code, statusRec.Body.String())
	}
}
