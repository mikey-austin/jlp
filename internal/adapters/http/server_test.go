package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

	funnel         storage.VocabFunnel
	funnelErr      error
	trends         []storage.SubjectTrend
	trendsErr      error
	calibration    []storage.ConfidenceCalibration
	calibrationErr error
	agentUsage     []storage.AgentUsage
	agentUsageErr  error
	system         storage.SystemStats
	systemErr      error
}

func (f fakeAnalyticsRepo) Statistics(context.Context, learner.IdentityID) (storage.Statistics, error) {
	return f.stats, f.err
}

func (f fakeAnalyticsRepo) VocabFunnel(context.Context, learner.IdentityID) (storage.VocabFunnel, error) {
	return f.funnel, f.funnelErr
}

func (f fakeAnalyticsRepo) WeaknessTrends(context.Context, learner.IdentityID) ([]storage.SubjectTrend, error) {
	return f.trends, f.trendsErr
}

func (f fakeAnalyticsRepo) ConfidenceCalibration(context.Context, learner.IdentityID) ([]storage.ConfidenceCalibration, error) {
	return f.calibration, f.calibrationErr
}

func (f fakeAnalyticsRepo) AgentUsage(context.Context, learner.IdentityID) ([]storage.AgentUsage, error) {
	return f.agentUsage, f.agentUsageErr
}

func (f fakeAnalyticsRepo) SystemStats(context.Context, learner.IdentityID) (storage.SystemStats, error) {
	return f.system, f.systemErr
}

var errStatistics = errors.New("statistics unavailable")

func TestMain(m *testing.M) {
	// Templates are read from web/templates relative to repo root.
	if err := os.Chdir("../../.."); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

type testAuth struct{ id learner.Identity }

func (a testAuth) Authenticate(*http.Request) (learner.Identity, error) { return a.id, nil }

type testIdentityRepo struct{}

func (testIdentityRepo) Upsert(context.Context, learner.Identity) error { return nil }
func (testIdentityRepo) Get(_ context.Context, id learner.IdentityID) (learner.Identity, error) {
	return learner.Identity{ID: id}, nil
}
func (testIdentityRepo) ListIdentities(context.Context) ([]learner.Identity, error) { return nil, nil }

func testOptions() Options {
	return Options{
		Addr:       ":0",
		Auth:       testAuth{id: learner.Identity{ID: "dev", DisplayName: "Dev Learner"}},
		Identities: testIdentityRepo{},
	}
}

// TestAuthRoutesAreServedWithoutASession is the whole point of
// mounting Options.AuthRoutes outside the authenticated group: a
// learner reaches /auth/login precisely because they have no session,
// so if RequireIdentity guarded it they could never sign in.
func TestAuthRoutesAreServedWithoutASession(t *testing.T) {
	opts := testOptions()
	opts.Auth = failingAuth{}
	opts.AuthRoutes = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("login " + r.URL.Path)); err != nil {
			t.Error(err)
		}
	})
	h := NewServer(opts).HandlerForTest()

	for _, path := range []string{"/auth/login", "/auth/callback", "/auth/logout"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "login "+path) {
			t.Errorf("%s: code=%d body=%q — the login routes must not require a session", path, rec.Code, rec.Body.String())
		}
	}
	// And everything else still does.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/sessions code = %d, want 401", rec.Code)
	}
}

// The mirror image: with no authenticator of its own (static and
// authelia mode), /auth/* must not exist at all rather than 401 or
// half-answer.
func TestNoAuthRoutesWhenTheAuthenticatorHasNone(t *testing.T) {
	rec := httptest.NewRecorder()
	NewServer(testOptions()).HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/auth/login code = %d, want 404", rec.Code)
	}
}

type failingAuth struct{}

func (failingAuth) Authenticate(*http.Request) (learner.Identity, error) {
	return learner.Identity{}, errors.New("no session")
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

// TestServerHasTimeoutsConfigured guards against a slow-client
// resource-exhaustion regression: NewServer must set all four
// http.Server timeouts rather than leaving them at Go's zero-value
// defaults (no timeout at all).
func TestServerHasTimeoutsConfigured(t *testing.T) {
	srv := NewServer(testOptions())
	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 60*time.Second {
		t.Errorf("WriteTimeout = %v, want 60s", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
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
	// Built on testOptionsWithSessions (not a bare Options{Auth, Identities}
	// literal) so Sessions/Analytics are non-nil here too: the request below
	// is expected to be rejected by RequireIdentity before s.home ever runs,
	// but if a future change to that middleware ever let it through, this
	// should fail on the 401 assertion below rather than nil-panic inside
	// s.home.
	opts := testOptionsWithSessions()
	opts.Auth = authn
	srv := NewServer(opts)

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
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{
		stats: storage.Statistics{
			RunesWritten:         250,
			SessionCount:         1,
			FeedbackRequests:     3,
			CorrectionsPresented: 4,
			CorrectionsAccepted:  3,
			CorrectionsRejected:  1,
			TopErrorTypes:        []storage.ErrorTypeCount{{Type: "conjugation", Count: 3}},
		},
		funnel: storage.VocabFunnel{LookedUp: 42, Produced: 17, ProducedCorrectly: 9},
		trends: []storage.SubjectTrend{{
			Subject: "i-adjective-past",
			Weeks: []storage.WeeklyCount{
				{Count: 0}, {Count: 0}, {Count: 0}, {Count: 0},
				{Count: 1}, {Count: 0}, {Count: 2}, {Count: 3},
			},
		}},
		calibration: []storage.ConfidenceCalibration{{Confidence: 4, Attempts: 5, Corrects: 4}},
	})
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
	if !strings.Contains(body, "語彙ファネル") || !strings.Contains(body, "42") || !strings.Contains(body, "17") || !strings.Contains(body, "9") {
		t.Fatalf("home body missing vocab funnel section/values: %s", body)
	}
	if !strings.Contains(body, "弱点トレンド") || !strings.Contains(body, "i-adjective-past") {
		t.Fatalf("home body missing weakness trends section: %s", body)
	}
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "<polyline") {
		t.Fatalf("home body missing a sparkline <svg>/<polyline>: %s", body)
	}
	if !strings.Contains(body, "自信の較正") || !strings.Contains(body, "80%") {
		t.Fatalf("home body missing confidence calibration section (CorrectRate 0.8 -> 80%%): %s", body)
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

// TestHomeVocabFunnelRepositoryErrorReturns500, ...WeaknessTrends...,
// and ...ConfidenceCalibration... cover the Task 7 additions to home:
// each new dashboard section's repository failure must surface as 500
// too, same as Statistics above.
func TestHomeVocabFunnelRepositoryErrorReturns500(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{funnelErr: errStatistics})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHomeWeaknessTrendsRepositoryErrorReturns500(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{trendsErr: errStatistics})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHomeConfidenceCalibrationRepositoryErrorReturns500(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{calibrationErr: errStatistics})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestHomeRendersEmptyStatesForNoTrendsOrCalibration covers the "no
// rows" contract: an identity with no live weaknesses and no
// confidence-rated attempts gets explicit empty-state text, not a
// blank/broken section (and, implicitly, no NaN from an empty
// ConfidenceCalibration slice).
func TestHomeRendersEmptyStatesForNoTrendsOrCalibration(t *testing.T) {
	opts := testOptionsWithSessions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("home status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "NaN") {
		t.Fatalf("home body contains NaN: %s", body)
	}
	if !strings.Contains(body, "弱点トレンド") {
		t.Fatalf("home body missing 弱点トレンド heading: %s", body)
	}
	if !strings.Contains(body, "自信の較正") {
		t.Fatalf("home body missing 自信の較正 heading: %s", body)
	}
}

// TestOfflineRenders covers the Task 17 PWA shell's offline fallback:
// it must render with no authentication required (like /healthz), since
// the service worker serves it when the network — and thus any
// session — is unavailable.
func TestOfflineRenders(t *testing.T) {
	srv := NewServer(testOptions())
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/offline", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("offline status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "オフライン") {
		t.Fatalf("offline body missing オフライン: %s", rec.Body.String())
	}
}

// TestStaticServiceWorkerHasServiceWorkerAllowedHeader covers the
// registration prerequisite called out in Task 17: without this header,
// a worker served from under /static/ cannot register with scope '/'.
func TestStaticServiceWorkerHasServiceWorkerAllowedHeader(t *testing.T) {
	srv := NewServer(testOptions())
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/sw.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("sw.js status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Fatalf("Service-Worker-Allowed = %q, want \"/\"", got)
	}
	// This once asserted the literal "jlp-shell-v3" and was satisfied by
	// the old worker's line-3 COMMENT ("Cache name is versioned:
	// jlp-shell-v3"), so it kept passing while the real constant moved to
	// v4 and then v5 — it pinned prose, not behaviour. Assert the served
	// constant equals the one the app derives from the current assets.
	body := rec.Body.String()
	want := `const CACHE_NAME = "` + currentShell().CacheName + `";`
	if !strings.Contains(body, want) {
		t.Fatalf("sw.js does not declare %s:\n%s", want, body)
	}
	// And that it precaches content-addressed URLs, which is what makes
	// the cache-first strategy safe.
	if !strings.Contains(body, "/static/css/components.") || strings.Contains(body, `"/static/css/components.css"`) {
		t.Fatalf("sw.js precaches components.css unfingerprinted:\n%s", body)
	}
}

// TestStaticServesDesignSystemAssets covers Task 9: the design system's
// stylesheets, vendored font, and theme script must resolve through the
// running handler like any other /static/* asset (the fs.FileServer
// route added no new routing — this just guards against a typo'd path
// or a file that didn't get vendored/committed).
func TestStaticServesDesignSystemAssets(t *testing.T) {
	srv := NewServer(testOptions())

	cases := []struct {
		path            string
		wantContentType string
	}{
		{"/static/css/tokens.css", "text/css"},
		{"/static/css/components.css", "text/css"},
		{"/static/js/theme.js", "javascript"},
		{"/static/fonts/instrument-sans-latin-400-normal.woff2", "font/woff2"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantContentType) {
			t.Fatalf("%s Content-Type = %q, want to contain %q", tc.path, ct, tc.wantContentType)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s body is empty", tc.path)
		}
	}
}

// PracticeStats backs 学習's 練習 section; these tests do not assert on
// it, so an empty value keeps them honest about what they DO cover.
func (f fakeAnalyticsRepo) PracticeStats(context.Context, learner.IdentityID) (storage.PracticeStats, error) {
	return storage.PracticeStats{}, nil
}
