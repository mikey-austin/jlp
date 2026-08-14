package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

func TestMain(m *testing.M) {
	// Templates are read from web/templates relative to repo root.
	os.Chdir("../../..")
	os.Exit(m.Run())
}

type testAuth struct{ id learner.Identity }

func (a testAuth) Authenticate(*http.Request) (learner.Identity, error) { return a.id, nil }

type testIdentityRepo struct{}

func (testIdentityRepo) Upsert(context.Context, learner.Identity) error { return nil }
func (testIdentityRepo) Get(_ context.Context, id learner.IdentityID) (learner.Identity, error) {
	return learner.Identity{ID: id}, nil
}

func testOptions() Options {
	return Options{
		Addr:       ":0",
		Auth:       testAuth{id: learner.Identity{ID: "dev", DisplayName: "Dev Learner"}},
		Identities: testIdentityRepo{},
	}
}

func TestHealthz(t *testing.T) {
	srv := NewServer(testOptions())
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("healthz body = %s", rec.Body.String())
	}
}

func TestHomeRenders(t *testing.T) {
	srv := NewServer(testOptions())
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("home status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ようこそ") {
		t.Fatalf("home body missing greeting")
	}
}
