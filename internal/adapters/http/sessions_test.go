package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
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

func testOptionsWithSessions() Options {
	opts := testOptions()
	opts.Sessions = sessions.NewService(newFakeSessionRepo())
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
	if !strings.Contains(recWorkspace.Body.String(), "エディタは次のタスクで") {
		t.Fatalf("workspace body missing editor placeholder: %s", recWorkspace.Body.String())
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
