package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeSessionRepo is an in-memory storage.SessionRepository for HTTP-layer
// tests, mirroring the port and scoped by identity like the real adapters.
type fakeSessionRepo struct {
	byKey map[string]session.Session
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byKey: map[string]session.Session{}}
}

func (f *fakeSessionRepo) key(identity learner.IdentityID, id session.ID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeSessionRepo) Create(_ context.Context, s session.Session) error {
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

func (f *fakeDocRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error) {
	sk := f.sessionKey(identity, sid)
	if id, ok := f.bySession[sk]; ok {
		return f.docs[f.key(identity, id)], nil
	}
	f.nextID++
	id := writing.DocumentID(fmt.Sprintf("doc-%d", f.nextID))
	doc := writing.Document{ID: id, SessionID: sid, IdentityID: identity, Version: 1}
	f.bySession[sk] = id
	f.docs[f.key(identity, id)] = doc
	return doc, nil
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

func testOptionsWithSessions() Options {
	opts := testOptions()
	opts.Sessions = sessions.NewService(newFakeSessionRepo())
	opts.Writing = appwriting.NewService(newFakeDocRepo())
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
