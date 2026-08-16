package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	agentanki "github.com/mikeyaustin/jlp/internal/agent/anki"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeAnkiCardRepo is an in-memory storage.AnkiCardRepository for
// HTTP-layer tests, identity-scoped like the real postgres adapter —
// mirrors application/anki/service_test.go's own double of the same
// shape.
type fakeAnkiCardRepo struct {
	byID map[string]storage.AnkiCard
}

func newFakeAnkiCardRepo() *fakeAnkiCardRepo {
	return &fakeAnkiCardRepo{byID: map[string]storage.AnkiCard{}}
}

func (f *fakeAnkiCardRepo) Insert(_ context.Context, c storage.AnkiCard) error {
	f.byID[c.ID] = c
	return nil
}

func (f *fakeAnkiCardRepo) List(_ context.Context, identity learner.IdentityID, status string) ([]storage.AnkiCard, error) {
	var out []storage.AnkiCard
	for _, c := range f.byID {
		if c.IdentityID != identity {
			continue
		}
		if status != "" && c.Status != status {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeAnkiCardRepo) UpdateStatus(_ context.Context, identity learner.IdentityID, id, status string) (storage.AnkiCard, error) {
	c, ok := f.byID[id]
	if !ok || c.IdentityID != identity {
		return storage.AnkiCard{}, storage.ErrNotFound
	}
	c.Status = status
	f.byID[id] = c
	return c, nil
}

// TakeApprovedForExport mirrors the real postgres repository's
// read+mark atomicity closely enough for these single-goroutine HTTP
// tests (see application/anki/service_test.go's own fake for the
// concurrency-safe version, and adapters/postgres/anki_test.go's
// integration test for the real concurrent-callers proof).
func (f *fakeAnkiCardRepo) TakeApprovedForExport(_ context.Context, identity learner.IdentityID, _ time.Time) ([]storage.AnkiCard, error) {
	var out []storage.AnkiCard
	for id, c := range f.byID {
		if c.IdentityID == identity && c.Status == "approved" {
			c.Status = "exported"
			f.byID[id] = c
			out = append(out, c)
		}
	}
	return out, nil
}

// fakeConnector is an appanki.AnkiConnector test double for HTTP-layer
// tests.
type fakeConnector struct {
	added int
	err   error
}

func (f *fakeConnector) AddNotes(context.Context, []storage.AnkiCard) (int, error) {
	return f.added, f.err
}

// ankiTestServer wires a real chi router with real feedback.Service and
// anki.Service (both over fakeai — deterministic, no network),
// mirroring feedback_test.go's feedbackTestServerWithMode: it creates a
// session, opens/autosaves a document containing the known-bad
// conjugation, requests feedback, and accepts the one resulting
// correction — returning the handler, the accepted correction's ID, the
// in-memory anki card repo, and the event repo, ready for a test to
// drive /corrections/{id}/anki onward. connector is wired via
// SetConnector only when non-nil, gating AnkiConnectEnabled the same
// way main.go does off APP_ANKI_CONNECT_URL.
func ankiTestServer(t *testing.T, connector appanki.AnkiConnector) (h http.Handler, correctionID string, cards *fakeAnkiCardRepo, events *fakeEventRepo) {
	t.Helper()
	opts := testOptions()
	sessionRepo := newFakeSessionRepo()
	docRepo := newFakeDocRepo()
	feedbackRepo := newFakeFeedbackRepo()
	events = newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())

	opts.Sessions = sessions.NewService(sessionRepo)
	opts.Writing = appwriting.NewService(docRepo, rec)
	opts.Events = events
	vocabRepo := newFakeVocabRepo()
	vocabSvc := appvocabulary.NewService(vocabRepo, rec)
	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, fakeGrammarRepo{}, fakePriorityRepo{}, vocabRepo, time.Now)
	opts.Feedback = appfeedback.NewService(sessionRepo, docRepo, feedbackRepo, fakeGrammarRepo{}, fakePriorityRepo{}, teachingPlanner, vocabSvc, teacher.New(fakeai.New()), rec, false, nil, nil)

	cards = newFakeAnkiCardRepo()
	ankiSvc := appanki.NewService(cards, feedbackRepo, agentanki.New(fakeai.New()), rec)
	if connector != nil {
		ankiSvc.SetConnector(connector)
	}
	opts.Anki = ankiSvc
	opts.AnkiCards = cards
	opts.AnkiConnectEnabled = connector != nil

	content := "とても面白いでした"
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
	handler := srv.HandlerForTest()

	runeLen := len([]rune(content))
	form := url.Values{}
	form.Set("document_id", string(doc.ID))
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	form.Set("text", content)
	rec2 := postForm(t, handler, "/sessions/"+string(sess.ID)+"/feedback", form)
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	correctionID = extractCorrectionID(t, rec2.Body.String())

	statusForm := url.Values{}
	statusForm.Set("status", "accepted")
	statusRec := postForm(t, handler, "/corrections/"+correctionID+"/status", statusForm)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("POST correction status = %d, body=%s", statusRec.Code, statusRec.Body.String())
	}

	return handler, correctionID, cards, events
}

// TestCorrectionAnkiCreatesDraftAndTogglesFromAnkiPage pins the brief's
// Step 2 browser scenario: 「Ankiカード作成」 creates a draft with the
// canned front/back, and /anki shows it.
func TestCorrectionAnkiCreatesDraftAndTogglesFromAnkiPage(t *testing.T) {
	h, correctionID, _, events := ankiTestServer(t, nil)

	rec := postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /corrections/{id}/anki status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "「とても面白いでした」— 何が不自然？") {
		t.Fatalf("toast body missing the canned front: %s", rec.Body.String())
	}

	pageRec := httptest.NewRecorder()
	h.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, "/anki", nil))
	if pageRec.Code != http.StatusOK {
		t.Fatalf("GET /anki status = %d, body=%s", pageRec.Code, pageRec.Body.String())
	}
	body := pageRec.Body.String()
	if !strings.Contains(body, "「とても面白いでした」— 何が不自然？") {
		t.Fatalf("/anki page missing the draft card's front: %s", body)
	}
	if !strings.Contains(body, "承認") || !strings.Contains(body, "却下") {
		t.Fatalf("/anki page missing 承認/却下 buttons for the draft: %s", body)
	}

	var found bool
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == "anki.card.created" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an anki.card.created event, found none: %+v", events.byIdentity["dev"])
	}
}

// TestAnkiStatusApproveThenExportTSV pins the brief's Step 2 export
// scenario: 承認 an existing draft, then GET /anki/export.tsv returns
// the front/back as TSV with a tab separator and the download headers,
// and the card's status flips to "exported".
func TestAnkiStatusApproveThenExportTSV(t *testing.T) {
	h, correctionID, cards, _ := ankiTestServer(t, nil)
	postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})

	var cardID string
	for id := range cards.byID {
		cardID = id
	}
	if cardID == "" {
		t.Fatal("no draft card found")
	}

	approveRec := postForm(t, h, "/anki/"+cardID+"/status", url.Values{"status": {"approved"}})
	if approveRec.Code != http.StatusOK {
		t.Fatalf("POST /anki/{id}/status status = %d, body=%s", approveRec.Code, approveRec.Body.String())
	}
	if !strings.Contains(approveRec.Body.String(), `data-status="approved"`) {
		t.Fatalf("body missing data-status=\"approved\": %s", approveRec.Body.String())
	}

	exportReq := httptest.NewRequest(http.MethodGet, "/anki/export.tsv", nil)
	exportRec := httptest.NewRecorder()
	h.ServeHTTP(exportRec, exportReq)
	if exportRec.Code != http.StatusOK {
		t.Fatalf("GET /anki/export.tsv status = %d, body=%s", exportRec.Code, exportRec.Body.String())
	}
	if got := exportRec.Header().Get("Content-Disposition"); got != `attachment; filename="jlp-anki.tsv"` {
		t.Fatalf("Content-Disposition = %q, want the jlp-anki.tsv attachment header", got)
	}
	if !strings.Contains(exportRec.Header().Get("Content-Type"), "text/tab-separated-values") {
		t.Fatalf("Content-Type = %q, want text/tab-separated-values", exportRec.Header().Get("Content-Type"))
	}
	body := exportRec.Body.String()
	if !strings.Contains(body, "「とても面白いでした」— 何が不自然？") {
		t.Fatalf("tsv body missing the front text: %s", body)
	}
	if !strings.Contains(body, "\t") {
		t.Fatalf("tsv body missing a tab separator: %q", body)
	}

	if cards.byID[cardID].Status != "exported" {
		t.Fatalf("card status = %q, want exported", cards.byID[cardID].Status)
	}

	// Second immediate GET must return an empty TSV — no double export.
	secondRec := httptest.NewRecorder()
	h.ServeHTTP(secondRec, httptest.NewRequest(http.MethodGet, "/anki/export.tsv", nil))
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second GET /anki/export.tsv status = %d", secondRec.Code)
	}
	if secondRec.Body.Len() != 0 {
		t.Fatalf("second GET /anki/export.tsv body = %q, want empty (no double export)", secondRec.Body.String())
	}
}

// TestAnkiStatusRejectPath pins the 却下 path on a second card.
func TestAnkiStatusRejectPath(t *testing.T) {
	h, correctionID, cards, _ := ankiTestServer(t, nil)
	postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})

	var cardID string
	for id := range cards.byID {
		cardID = id
	}

	rec := postForm(t, h, "/anki/"+cardID+"/status", url.Values{"status": {"rejected"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /anki/{id}/status status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-status="rejected"`) {
		t.Fatalf("body missing data-status=\"rejected\": %s", rec.Body.String())
	}
	if cards.byID[cardID].Status != "rejected" {
		t.Fatalf("card status = %q, want rejected", cards.byID[cardID].Status)
	}
}

// TestAnkiPageHidesAnkiConnectButtonWhenNotConfigured pins the config
// gate: with no connector wired (nil, the ankiTestServer(t, nil) case
// every other test above uses), the /anki page must never render the
// 「Ankiへ送信」 button at all.
func TestAnkiPageHidesAnkiConnectButtonWhenNotConfigured(t *testing.T) {
	h, _, _, _ := ankiTestServer(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anki", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /anki status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Ankiへ送信") {
		t.Fatalf("/anki page unexpectedly shows the Ankiへ送信 button with no connector configured: %s", rec.Body.String())
	}
}

// TestAnkiPageShowsAnkiConnectButtonWhenConfiguredAndPushWorks pins the
// configured-path counterpart: the button renders, and POST /anki/push
// reports the pushed count.
func TestAnkiPageShowsAnkiConnectButtonWhenConfiguredAndPushWorks(t *testing.T) {
	h, correctionID, cards, _ := ankiTestServer(t, &fakeConnector{added: 1})
	postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})
	var cardID string
	for id := range cards.byID {
		cardID = id
	}
	postForm(t, h, "/anki/"+cardID+"/status", url.Values{"status": {"approved"}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anki", nil))
	if !strings.Contains(rec.Body.String(), "Ankiへ送信") {
		t.Fatalf("/anki page missing the Ankiへ送信 button with a connector configured: %s", rec.Body.String())
	}

	pushRec := postForm(t, h, "/anki/push", url.Values{})
	if pushRec.Code != http.StatusOK {
		t.Fatalf("POST /anki/push status = %d, body=%s", pushRec.Code, pushRec.Body.String())
	}
	if !strings.Contains(pushRec.Body.String(), "1") {
		t.Fatalf("push result body missing the added count: %s", pushRec.Body.String())
	}
}

// TestCorrectionAnkiUnknownCorrectionReturnsNotFound pins the not-found
// contract at the HTTP layer.
func TestCorrectionAnkiUnknownCorrectionReturnsNotFound(t *testing.T) {
	h, _, _, _ := ankiTestServer(t, nil)
	rec := postForm(t, h, "/corrections/00000000-0000-0000-0000-000000000000/anki", url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAnkiPushFailureDoesNotLeakInternalErrorText pins Finding 4 of the
// final-review fix wave: a PushToAnkiConnect failure must never render
// err.Error()'s wrapped internal repo/network text into the partial —
// every other handler in this package shows a fixed Japanese message
// instead, and this route was the one holdout.
func TestAnkiPushFailureDoesNotLeakInternalErrorText(t *testing.T) {
	const internalDetail = "dial tcp 127.0.0.1:8765: connect: connection refused"
	h, correctionID, cards, _ := ankiTestServer(t, &fakeConnector{err: errors.New(internalDetail)})
	postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})
	var cardID string
	for id := range cards.byID {
		cardID = id
	}
	postForm(t, h, "/anki/"+cardID+"/status", url.Values{"status": {"approved"}})

	rec := postForm(t, h, "/anki/push", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /anki/push status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), internalDetail) {
		t.Fatalf("push result body leaks internal error detail: %s", rec.Body.String())
	}
}

// gatedAnkiTestServer mirrors ankiTestServer, but the session's
// TeacherMode is "socratic" (the mode adapters/fakeai keys hint
// attachment off of — see feedback_test.go's socraticFeedbackTestServer)
// and the resulting correction is left in its freshly-presented, hint-
// gated state: unlike ankiTestServer, this helper deliberately does NOT
// POST /corrections/{id}/status, so correctionID comes back still
// gated (HasHint() && Status == "presented" && !Revealed).
func gatedAnkiTestServer(t *testing.T) (h http.Handler, correctionID string, cards *fakeAnkiCardRepo, events *fakeEventRepo) {
	t.Helper()
	opts := testOptions()
	sessionRepo := newFakeSessionRepo()
	docRepo := newFakeDocRepo()
	feedbackRepo := newFakeFeedbackRepo()
	events = newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())

	opts.Sessions = sessions.NewService(sessionRepo)
	opts.Writing = appwriting.NewService(docRepo, rec)
	opts.Events = events
	vocabRepo := newFakeVocabRepo()
	vocabSvc := appvocabulary.NewService(vocabRepo, rec)
	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, fakeGrammarRepo{}, fakePriorityRepo{}, vocabRepo, time.Now)
	opts.Feedback = appfeedback.NewService(sessionRepo, docRepo, feedbackRepo, fakeGrammarRepo{}, fakePriorityRepo{}, teachingPlanner, vocabSvc, teacher.New(fakeai.New()), rec, false, nil, nil)

	cards = newFakeAnkiCardRepo()
	ankiSvc := appanki.NewService(cards, feedbackRepo, agentanki.New(fakeai.New()), rec)
	opts.Anki = ankiSvc
	opts.AnkiCards = cards

	content := "とても面白いでした"
	sess, err := opts.Sessions.Create(context.Background(), "dev", "日記", "Diary", session.Profile{
		TeacherMode:         "socratic",
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
	handler := srv.HandlerForTest()

	runeLen := len([]rune(content))
	form := url.Values{}
	form.Set("document_id", string(doc.ID))
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	form.Set("text", content)
	rec2 := postForm(t, handler, "/sessions/"+string(sess.ID)+"/feedback", form)
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	correctionID = extractCorrectionID(t, rec2.Body.String())

	return handler, correctionID, cards, events
}

// TestCorrectionAnkiRefusesGatedCorrection pins the final-review fix at
// the HTTP layer: POSTing /corrections/{id}/anki directly against a
// still-gated socratic correction (no plain "accept" step, no reveal —
// exactly what devtools or a script could do even though the /anki UI
// only ever renders the button on an accepted correction) must come
// back 409, must not leak the correction's Replacement anywhere in the
// response, and must not create a draft card.
func TestCorrectionAnkiRefusesGatedCorrection(t *testing.T) {
	h, correctionID, cards, _ := gatedAnkiTestServer(t)

	rec := postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "面白かったです") {
		t.Fatalf("409 response body leaks the corrected form: %s", rec.Body.String())
	}
	if len(cards.byID) != 0 {
		t.Fatalf("cards persisted = %d, want 0 for a gated correction", len(cards.byID))
	}
}

// TestCorrectionAnkiSucceedsAfterReveal pins the other half: once the
// learner explicitly reveals the answer via POST
// /corrections/{id}/reveal, the exact same correction is no longer
// gated and 「Ankiカード作成」 succeeds normally.
func TestCorrectionAnkiSucceedsAfterReveal(t *testing.T) {
	h, correctionID, cards, _ := gatedAnkiTestServer(t)

	revealRec := postForm(t, h, "/corrections/"+correctionID+"/reveal", url.Values{})
	if revealRec.Code != http.StatusOK {
		t.Fatalf("POST /corrections/{id}/reveal status = %d, body=%s", revealRec.Code, revealRec.Body.String())
	}

	rec := postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /corrections/{id}/anki status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(cards.byID) != 1 {
		t.Fatalf("cards persisted = %d, want 1 after reveal", len(cards.byID))
	}
}

// TestAnkiStatusInvalidValueReturnsBadRequest pins status validation at
// the HTTP layer.
func TestAnkiStatusInvalidValueReturnsBadRequest(t *testing.T) {
	h, correctionID, cards, _ := ankiTestServer(t, nil)
	postForm(t, h, "/corrections/"+correctionID+"/anki", url.Values{})
	var cardID string
	for id := range cards.byID {
		cardID = id
	}

	rec := postForm(t, h, "/anki/"+cardID+"/status", url.Values{"status": {"bogus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}
