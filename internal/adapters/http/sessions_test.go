package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double injected via agentconversation.New(ai.StructuredGenerator); PRD §75 Rule 3 forbids agents reaching real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	agentconversation "github.com/mikeyaustin/jlp/internal/agent/conversation"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeSessionRepo is an in-memory storage.SessionRepository for HTTP-layer
// tests, mirroring the port and scoped by identity like the real adapters.
type fakeSessionRepo struct {
	byKey map[string]session.Session
	// deleted mirrors the real table's deleted_at column: Get and List
	// honour it, so a handler test can assert a deleted session really
	// stops reaching the page rather than merely that the delete route
	// returned 303.
	deleted map[string]time.Time
	// createErr, when set, makes Create fail — used to exercise the
	// handlers' repository-failure path (must surface as 500 without
	// echoing this error's text back to the client).
	createErr error
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byKey: map[string]session.Session{}, deleted: map[string]time.Time{}}
}

func (f *fakeSessionRepo) key(identity learner.IdentityID, id session.ID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeSessionRepo) Create(_ context.Context, s session.Session) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.byKey[f.key(s.IdentityID, s.ID)] = s
	return nil
}

func (f *fakeSessionRepo) Get(_ context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	key := f.key(identity, id)
	s, ok := f.byKey[key]
	if !ok {
		return session.Session{}, storage.ErrNotFound
	}
	if _, gone := f.deleted[key]; gone {
		return session.Session{}, storage.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessionRepo) List(_ context.Context, identity learner.IdentityID) ([]session.Session, error) {
	var out []session.Session
	for key, s := range f.byKey {
		if s.IdentityID != identity {
			continue
		}
		if _, gone := f.deleted[key]; gone {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// SoftDelete/Restore mirror the real adapter's contract exactly:
// identity-scoped, idempotent, and ErrNotFound for both "unknown id"
// and "someone else's id".
func (f *fakeSessionRepo) SoftDelete(_ context.Context, identity learner.IdentityID, id session.ID, at time.Time) error {
	key := f.key(identity, id)
	if _, ok := f.byKey[key]; !ok {
		return storage.ErrNotFound
	}
	if _, already := f.deleted[key]; !already {
		f.deleted[key] = at
	}
	return nil
}

func (f *fakeSessionRepo) Restore(_ context.Context, identity learner.IdentityID, id session.ID) error {
	key := f.key(identity, id)
	if _, ok := f.byKey[key]; !ok {
		return storage.ErrNotFound
	}
	delete(f.deleted, key)
	return nil
}

// isDeleted / exists let a handler test assert on the raw storage state
// rather than on a status code — the difference between "the delete was
// refused" and "the row was quietly removed anyway".
func (f *fakeSessionRepo) isDeleted(identity learner.IdentityID, id session.ID) bool {
	_, gone := f.deleted[f.key(identity, id)]
	return gone
}

func (f *fakeSessionRepo) exists(identity learner.IdentityID, id session.ID) bool {
	_, ok := f.byKey[f.key(identity, id)]
	return ok
}

// fakeDocRepo is an in-memory storage.DocumentRepository for HTTP-layer
// tests, identity-scoped like the real adapter.
type fakeDocRepo struct {
	bySession map[string]writing.DocumentID
	docs      map[string]writing.Document
	nextID    int
}

func newFakeDocRepo() *fakeDocRepo {
	return &fakeDocRepo{bySession: map[string]writing.DocumentID{}, docs: map[string]writing.Document{}}
}

func (f *fakeDocRepo) key(identity learner.IdentityID, id writing.DocumentID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeDocRepo) sessionKey(identity learner.IdentityID, sid session.ID) string {
	return string(identity) + "/" + string(sid)
}

func (f *fakeDocRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, bool, error) {
	sk := f.sessionKey(identity, sid)
	if id, ok := f.bySession[sk]; ok {
		return f.docs[f.key(identity, id)], false, nil
	}
	f.nextID++
	id := writing.DocumentID(fmt.Sprintf("doc-%d", f.nextID))
	doc := writing.Document{ID: id, SessionID: sid, IdentityID: identity, Version: 1}
	f.bySession[sk] = id
	f.docs[f.key(identity, id)] = doc
	return doc, true, nil
}

func (f *fakeDocRepo) Save(_ context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	k := f.key(identity, id)
	doc, ok := f.docs[k]
	if !ok {
		return writing.Document{}, storage.ErrNotFound
	}
	doc.Content = content
	doc.Version++
	f.docs[k] = doc
	return doc, nil
}

func (f *fakeDocRepo) Get(_ context.Context, identity learner.IdentityID, id writing.DocumentID) (writing.Document, error) {
	doc, ok := f.docs[f.key(identity, id)]
	if !ok {
		return writing.Document{}, storage.ErrNotFound
	}
	return doc, nil
}

func (f *fakeDocRepo) ListVersions(context.Context, learner.IdentityID, writing.DocumentID, int) ([]writing.Document, error) {
	return nil, nil
}

// fakeEventRepo is an in-memory storage.LearningEventRepository for
// HTTP-layer tests: identity-scoped and newest-first like the real
// adapter. It records the arguments of the last ListRecent call so tests
// can assert the activity handler scopes correctly, and listErr, when
// set, simulates a repository failure.
type fakeEventRepo struct {
	byIdentity map[learner.IdentityID][]event.LearningEvent
	listErr    error

	lastIdentity learner.IdentityID
	lastSession  *session.ID
	lastLimit    int
}

func newFakeEventRepo() *fakeEventRepo {
	return &fakeEventRepo{byIdentity: map[learner.IdentityID][]event.LearningEvent{}}
}

func (f *fakeEventRepo) Append(_ context.Context, ev event.LearningEvent) error {
	f.byIdentity[ev.IdentityID] = append(f.byIdentity[ev.IdentityID], ev)
	return nil
}

func (f *fakeEventRepo) ListRecent(_ context.Context, identity learner.IdentityID, sid *session.ID, limit int) ([]event.LearningEvent, error) {
	f.lastIdentity = identity
	f.lastSession = sid
	f.lastLimit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	all := f.byIdentity[identity]
	out := make([]event.LearningEvent, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		ev := all[i]
		if sid != nil && (ev.SessionID == nil || *ev.SessionID != *sid) {
			continue
		}
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeEventRepo) ListAll(_ context.Context, identity learner.IdentityID) ([]event.LearningEvent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	all := f.byIdentity[identity]
	out := make([]event.LearningEvent, len(all))
	copy(out, all)
	return out, nil
}

// fakeConversationRepo is a minimal in-memory storage.ConversationRepository
// for HTTP-layer tests — the workspace GET handler calls
// Conversation.History on every render, so any test reaching
// sessionsWorkspace needs a non-nil, non-panicking one wired in (see
// testOptionsWithSessions below), even though no test in this file
// exercises the conversation pane's own POST routes directly (see
// conversation_test.go for those).
type fakeConversationRepo struct {
	byConvID  map[string]learner.IdentityID
	bySession map[string]string
	turns     map[string][]storage.ConversationTurn
}

func newFakeConversationRepo() *fakeConversationRepo {
	return &fakeConversationRepo{byConvID: map[string]learner.IdentityID{}, bySession: map[string]string{}, turns: map[string][]storage.ConversationTurn{}}
}

func (f *fakeConversationRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (string, bool, error) {
	if id, ok := f.bySession[string(sid)]; ok {
		if f.byConvID[id] != identity {
			return "", false, storage.ErrNotFound
		}
		return id, false, nil
	}
	id := "conv-" + string(sid)
	f.byConvID[id] = identity
	f.bySession[string(sid)] = id
	return id, true, nil
}

func (f *fakeConversationRepo) InsertTurn(_ context.Context, identity learner.IdentityID, conversationID string, turn storage.ConversationTurn) error {
	if f.byConvID[conversationID] != identity {
		return storage.ErrNotFound
	}
	f.turns[conversationID] = append(f.turns[conversationID], turn)
	return nil
}

func (f *fakeConversationRepo) ListTurns(_ context.Context, identity learner.IdentityID, conversationID string) ([]storage.ConversationTurn, error) {
	if f.byConvID[conversationID] != identity {
		return nil, storage.ErrNotFound
	}
	return f.turns[conversationID], nil
}

func testOptionsWithSessions() Options {
	opts := testOptions()
	// SpeechEnabled mirrors a deployment WITH APP_SPEECH_STTURL set, so
	// the workspace tests that assert on the record button keep testing
	// the configured case. The dormant-by-default case (whole-branch
	// review F8) is covered by
	// TestWorkspaceHidesTheRecordButtonWhenSpeechIsNotConfigured.
	opts.SpeechEnabled = true
	sessRepo := newFakeSessionRepo()
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())
	opts.Sessions = sessions.NewService(sessRepo, rec)
	docRepo := newFakeDocRepo()
	opts.Writing = appwriting.NewService(docRepo, rec)
	opts.Events = events
	// Zero-value stats by default; tests exercising the dashboard's
	// content (TestHomeRenders et al.) override this with their own
	// fakeAnalyticsRepo.
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{})
	// The workspace GET route (sessionsWorkspace) reads
	// Conversation.History on every render (Phase 4 Task 6) — wired
	// over the SAME sessRepo opts.Sessions uses, so a session created
	// through opts.Sessions is visible to it.
	opts.Conversation = appconversation.NewService(newFakeConversationRepo(), sessRepo, agentconversation.New(fakeai.New()), nil, rec, fakeGrammarRepo{}, events)
	// Feedback (Phase 4 Task W item 2): sessionsWorkspace also reads
	// Feedback.ListForSession on every GET now (the feedback-history
	// pane replacing the old activity feed), so every test hitting the
	// workspace route needs a working feedback.Service, not just
	// Sessions/Writing/Conversation — over the SAME sessRepo/docRepo
	// instances, same reasoning as Conversation above.
	feedbackRepo := newFakeFeedbackRepo()
	vocabRepo := newFakeVocabRepo()
	vocabSvc := appvocabulary.NewService(vocabRepo, rec)
	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, fakeGrammarRepo{}, fakePriorityRepo{}, vocabRepo, time.Now)
	opts.Feedback = appfeedback.NewService(sessRepo, docRepo, feedbackRepo, fakeGrammarRepo{}, fakePriorityRepo{}, teachingPlanner, vocabSvc, teacher.New(fakeai.New()), rec, false, nil, nil)
	return opts
}

func TestSessionsCreateThenListShowsNewSession(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	form := url.Values{}
	form.Set("title", "旅行について書く")
	form.Set("purpose", "Blog post")
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /sessions status = %d, body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sessions/") {
		t.Fatalf("Location = %q, want prefix /sessions/", loc)
	}

	recList := httptest.NewRecorder()
	h.ServeHTTP(recList, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if recList.Code != http.StatusOK {
		t.Fatalf("GET /sessions status = %d", recList.Code)
	}
	if !strings.Contains(recList.Body.String(), "旅行について書く") {
		t.Fatalf("GET /sessions body missing created title: %s", recList.Body.String())
	}

	recWorkspace := httptest.NewRecorder()
	h.ServeHTTP(recWorkspace, httptest.NewRequest(http.MethodGet, loc, nil))
	if recWorkspace.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, body=%s", loc, recWorkspace.Code, recWorkspace.Body.String())
	}
	if !strings.Contains(recWorkspace.Body.String(), `id="editor"`) {
		t.Fatalf("workspace body missing editor textarea: %s", recWorkspace.Body.String())
	}
	if !strings.Contains(recWorkspace.Body.String(), "旅行について書く") {
		t.Fatalf("workspace body missing session title: %s", recWorkspace.Body.String())
	}
	// Phase 4 Task 8 (PRD §66): the conversation pane's record button —
	// record.js finds it by id and reads data-transcribe-url to know
	// where to POST, so both must be present in the rendered markup.
	if !strings.Contains(recWorkspace.Body.String(), `id="record-btn"`) {
		t.Fatalf("workspace body missing the speech record button: %s", recWorkspace.Body.String())
	}
	if !strings.Contains(recWorkspace.Body.String(), `data-transcribe-url="/speech/transcribe"`) {
		t.Fatalf("workspace body missing the record button's transcribe URL: %s", recWorkspace.Body.String())
	}
}

// TestWorkspaceHidesTheRecordButtonWhenSpeechIsNotConfigured pins
// whole-branch review F8/W-2: speech is dormant by DEFAULT
// (APP_SPEECH_STTURL unset), and until now the workspace rendered the
// 🎙 録音 button anyway — so the learner granted microphone permission,
// recorded a sentence, waited for the upload and only THEN read a 503 in
// the status line. AnkiConnectEnabled is the in-tree precedent: an
// optional integration's control is not rendered at all until it is
// configured. Typed conversation input must be unaffected.
func TestWorkspaceHidesTheRecordButtonWhenSpeechIsNotConfigured(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.SpeechEnabled = false
	h := NewServer(opts).HandlerForTest()
	sid := createTestSession(t, h, "socratic", "end")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+sid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET workspace status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, dead := range []string{`id="record-btn"`, `data-transcribe-url`, `id="record-status"`} {
		if strings.Contains(body, dead) {
			t.Errorf("workspace rendered %s with speech unconfigured — the learner records before learning it can't work: %s", dead, body)
		}
	}
	// The conversation form itself must still be there: gating speech
	// must not gate typed practice.
	if !strings.Contains(body, `id="conversation-form"`) {
		t.Errorf("workspace lost the conversation form along with the record button: %s", body)
	}
}

func TestSessionsCreateEmptyTitleReturnsBadRequest(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	form := url.Values{}
	form.Set("title", "")
	form.Set("purpose", "Diary")
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestSessionsPageRendersTriggerAndModal: the new-session form must live
// inside the <dialog id="session-modal"> the "新しいセッション" button
// opens — nothing below the list any more (Task NS).
func TestSessionsPageRendersTriggerAndModal(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="new-session-trigger"`) {
		t.Fatalf("missing new-session trigger button: %s", body)
	}
	if !strings.Contains(body, `aria-controls="session-modal"`) {
		t.Fatalf("trigger missing aria-controls=session-modal: %s", body)
	}
	dialogIdx := strings.Index(body, `<dialog id="session-modal"`)
	formIdx := strings.Index(body, `action="/sessions"`)
	if dialogIdx == -1 {
		t.Fatalf("missing <dialog id=\"session-modal\">: %s", body)
	}
	if formIdx == -1 || formIdx < dialogIdx {
		t.Fatalf("form must be inside the dialog: dialogIdx=%d formIdx=%d body=%s", dialogIdx, formIdx, body)
	}
}

// TestSessionsCreateEmptyTitleReopensModalWithErrorAndPreservedValues
// pins the brief's core requirement: a rejected submission must not
// dump the learner back to a closed modal with their input gone. The
// re-rendered page must carry data-reopen (picked up by app.js to
// re-showModal()), the error text, and every field exactly as typed.
func TestSessionsCreateEmptyTitleReopensModalWithErrorAndPreservedValues(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	form := url.Values{}
	form.Set("title", "")
	form.Set("purpose", "Diary")
	form.Set("teacher_mode", "socratic")
	form.Set("explanation_language", "en")
	form.Set("strictness", "strict")
	form.Set("feedback_timing", "immediate")
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<dialog id="session-modal"`) || !strings.Contains(body, "data-reopen") {
		t.Fatalf("dialog missing data-reopen marker: %s", body)
	}
	if !strings.Contains(body, `class="error" role="alert"`) {
		t.Fatalf("missing visible error message: %s", body)
	}
	for _, want := range []string{
		`value="Diary" selected`,
		`value="socratic" selected`,
		`value="en" selected`,
		`value="strict" selected`,
		`value="immediate" selected`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("preserved form value %q not found: %s", want, body)
		}
	}
	// The list stays rendered behind the modal, not replaced by it.
	if !strings.Contains(body, "<table>") {
		t.Fatalf("session list missing from reopened page: %s", body)
	}
}

// TestSessionsCreateRepositoryErrorReturns500AndHidesDetail: a
// repository failure unrelated to validation (e.g. a database error)
// must surface as a generic 500, and the response body must never leak
// the underlying error's text — it could contain a DSN, driver detail,
// or other internal information.
func TestSessionsCreateRepositoryErrorReturns500AndHidesDetail(t *testing.T) {
	opts := testOptionsWithSessions()
	repo := newFakeSessionRepo()
	repo.createErr = errors.New("pq: connection refused to host db.internal:5432 user=jlp password=hunter2")
	opts.Sessions = sessions.NewService(repo, learning.NewRecorder(newFakeEventRepo(), inprocbus.New()))

	srv := NewServer(opts)
	h := srv.HandlerForTest()

	form := url.Values{}
	form.Set("title", "旅行について書く")
	form.Set("purpose", "Diary")
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("body leaked underlying repository error: %s", rec.Body.String())
	}
}

func TestSessionsWorkspaceCrossIdentityNotFound(t *testing.T) {
	opts := testOptionsWithSessions()
	svc := opts.Sessions
	created, err := svc.Create(context.Background(), "someone-else", "他人のセッション", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(opts) // testOptions() authenticates as identity "dev"
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+string(created.ID), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Phase 4 Task W item 2 removed /sessions/{id}/activity (the workspace's
// old アクティビティ feed) along with its handler and template — the
// feedback-history pane replaces it, and nothing else in the codebase
// consumed that route (see internal/adapters/http/feedback.go's
// feedbackRequest/feedbackShow and toFeedbackHistoryView for its
// replacement). TestSessionsActivityRendersRecentEventsScopedToCallerAndSession
// and TestSessionsActivityRepositoryErrorReturns500 tested exactly that
// route and were removed with it.
