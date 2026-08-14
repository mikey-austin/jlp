package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/authelia"
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

func TestServerDoesNotTrustForwardedForHeader(t *testing.T) {
	// Regression: chi's middleware.RealIP would rewrite r.RemoteAddr from a
	// client-supplied X-Forwarded-For header, letting an untrusted caller
	// spoof its way past the authelia adapter's peer-trust check. The server
	// must never let X-Forwarded-For influence the trust decision.
	authn, err := authelia.New([]string{"172.16.0.0/12"})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{Addr: ":0", Auth: authn, Identities: testIdentityRepo{}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1234" // real TCP peer: untrusted, outside 172.16.0.0/12
	req.Header.Set("X-Forwarded-For", "172.18.0.5")
	req.Header.Set("Remote-User", "mallory")
	req.Header.Set("Remote-Name", "Mallory Evil")

	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401 (untrusted peer must be rejected regardless of forged X-Forwarded-For)", rec.Code)
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
