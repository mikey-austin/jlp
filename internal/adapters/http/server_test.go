package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/authelia"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeAnalyticsRepo is an in-memory storage.AnalyticsRepository double
// for HTTP-layer tests: it returns whatever Statistics a test
// configures, unconditionally (analytics doesn't need identity scoping
// in these tests — the postgres repo's identity scoping is covered by
// its own integration test).
type fakeAnalyticsRepo struct {
	stats storage.Statistics
	err   error
}

func (f fakeAnalyticsRepo) Statistics(context.Context, learner.IdentityID) (storage.Statistics, error) {
	return f.stats, f.err
}

var errStatistics = errors.New("statistics unavailable")

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

// TestHomeRenders covers the Task 14 dashboard: stat tiles built from
// Options.Analytics, and a recent-sessions list built from
// Options.Sessions.List.
func TestHomeRenders(t *testing.T) {
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
	sess, err := opts.Sessions.Create(context.Background(), "dev", "旅行について書く", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("home status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="stat-grid"`) {
		t.Fatalf("home body missing stat-grid: %s", body)
	}
	if !strings.Contains(body, "250") {
		t.Fatalf("home body missing RunesWritten tile value: %s", body)
	}
	if !strings.Contains(body, "75%") {
		t.Fatalf("home body missing AcceptanceRate as a percentage (3/(3+1)=75%%): %s", body)
	}
	if !strings.Contains(body, "16.0") {
		t.Fatalf("home body missing CorrectionsPer1000 (4/250*1000=16.0): %s", body)
	}
	if !strings.Contains(body, "conjugation") {
		t.Fatalf("home body missing top error type: %s", body)
	}
	if !strings.Contains(body, "/sessions/"+string(sess.ID)) {
		t.Fatalf("home body missing recent session link: %s", body)
	}
	if !strings.Contains(body, "旅行について書く") {
		t.Fatalf("home body missing recent session title: %s", body)
	}
}

// TestHomeStatisticsRepositoryErrorReturns500 mirrors the existing
// activity-feed error handling (TestSessionsActivityRepositoryErrorReturns500):
// a statistics failure must surface as 500, not a partial/blank dashboard.
func TestHomeStatisticsRepositoryErrorReturns500(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{err: errStatistics})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
