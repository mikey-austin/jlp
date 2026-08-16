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
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
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
	// createErr, when set, makes Create fail — used to exercise the
	// handlers' repository-failure path (must surface as 500 without
	// echoing this error's text back to the client).
	createErr error
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byKey: map[string]session.Session{}}
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
	s, ok := f.byKey[f.key(identity, id)]
	if !ok {
		return session.Session{}, storage.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessionRepo) List(_ context.Context, identity learner.IdentityID) ([]session.Session, error) {
	var out []session.Session
	for _, s := range f.byKey {
		if s.IdentityID == identity {
			out = append(out, s)
		}
	}
	return out, nil
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
	sessRepo := newFakeSessionRepo()
	opts.Sessions = sessions.NewService(sessRepo)
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())
	opts.Writing = appwriting.NewService(newFakeDocRepo(), rec)
	opts.Events = events
	// Zero-value stats by default; tests exercising the dashboard's
	// content (TestHomeRenders et al.) override this with their own
	// fakeAnalyticsRepo.
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{})
	// The workspace GET route (sessionsWorkspace) reads
	// Conversation.History on every render (Phase 4 Task 6) — wired
	// over the SAME sessRepo opts.Sessions uses, so a session created
	// through opts.Sessions is visible to it.
	opts.Conversation = appconversation.NewService(newFakeConversationRepo(), sessRepo, agentconversation.New(fakeai.New()), nil, rec)
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

// TestSessionsCreateRepositoryErrorReturns500AndHidesDetail: a
// repository failure unrelated to validation (e.g. a database error)
// must surface as a generic 500, and the response body must never leak
// the underlying error's text — it could contain a DSN, driver detail,
// or other internal information.
func TestSessionsCreateRepositoryErrorReturns500AndHidesDetail(t *testing.T) {
	opts := testOptionsWithSessions()
	repo := newFakeSessionRepo()
	repo.createErr = errors.New("pq: connection refused to host db.internal:5432 user=jlp password=hunter2")
	opts.Sessions = sessions.NewService(repo)

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

func TestSessionsActivityRendersRecentEventsScopedToCallerAndSession(t *testing.T) {
	opts := testOptionsWithSessions()
	sess, err := opts.Sessions.Create(context.Background(), "dev", "旅行について書く", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}

	events := opts.Events.(*fakeEventRepo)
	sid := sess.ID
	now := time.Now()
	events.byIdentity["dev"] = []event.LearningEvent{
		{ID: "ev-1", IdentityID: "dev", SessionID: &sid, Type: event.TypeWritingCreated, OccurredAt: now},
		{ID: "ev-2", IdentityID: "dev", SessionID: &sid, Type: event.TypeWritingUpdated, OccurredAt: now.Add(time.Minute)},
	}

	srv := NewServer(opts) // testOptions() authenticates as identity "dev"
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+string(sess.ID)+"/activity", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="activity-list"`) {
		t.Fatalf("body missing the activity-list wrapper: %s", body)
	}
	if !strings.Contains(body, string(event.TypeWritingUpdated)) {
		t.Fatalf("body missing %q: %s", event.TypeWritingUpdated, body)
	}
	if !strings.Contains(body, "<li>") {
		t.Fatalf("body missing <li> event rows: %s", body)
	}

	if events.lastIdentity != "dev" {
		t.Fatalf("ListRecent identity = %q, want dev", events.lastIdentity)
	}
	if events.lastSession == nil || *events.lastSession != sess.ID {
		t.Fatalf("ListRecent session filter = %v, want %s", events.lastSession, sess.ID)
	}
}

func TestSessionsActivityRepositoryErrorReturns500(t *testing.T) {
	opts := testOptionsWithSessions()
	sess, err := opts.Sessions.Create(context.Background(), "dev", "旅行について書く", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	opts.Events.(*fakeEventRepo).listErr = errors.New("db down")

	srv := NewServer(opts)
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+string(sess.ID)+"/activity", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
