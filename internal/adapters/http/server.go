package httpx

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mikeyaustin/jlp/internal/application/analytics"
	"github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Options struct {
	Addr       string
	Auth       auth.Authenticator
	Identities storage.IdentityRepository
	Sessions   *sessions.Service
	Writing    *appwriting.Service
	Events     storage.LearningEventRepository
	// Feedback drives the workspace's フィードバックを取得 button and
	// correction accept/reject buttons (Task 13): it wraps the Teacher
	// agent with authorization, persistence, and learning events.
	Feedback *feedback.Service
	// Analytics drives the home dashboard's stat tiles and よくある間違い
	// list (Task 14).
	Analytics *analytics.Service
	// AI is the always-observed structured generator (fake or Anthropic
	// underneath). No route consumes it directly — the teacher feedback
	// pipeline (Task 12/13) goes through Feedback above — but it's
	// wired through here in case a future task needs it directly.
	AI ai.StructuredGenerator
	// AIRequests backs the /ai observability page's table (Task 15):
	// the same audit-log repository the observability decorator writes
	// through, read back here for display.
	AIRequests storage.AIRequestRepository
	// AIRatings backs the feedback partial's star widget and the /ai
	// page's rating column (Task 15).
	AIRatings storage.AIRatingRepository
}

type Server struct {
	http.Server
	opts Options
}

func NewServer(opts Options) *Server {
	s := &Server{opts: opts}
	s.Addr = opts.Addr
	s.Handler = s.routes()
	// Guard against slow-client resource exhaustion (Slowloris-style
	// connections that trickle bytes to keep a handler goroutine and its
	// connection pinned indefinitely). None of these routes stream or
	// long-poll, so ordinary request/response timing comfortably fits
	// inside all four.
	s.ReadHeaderTimeout = 5 * time.Second
	s.ReadTimeout = 30 * time.Second
	s.WriteTimeout = 60 * time.Second
	s.IdleTimeout = 120 * time.Second
	return s
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	// Deliberately omit chi's middleware.RealIP: it unconditionally rewrites
	// r.RemoteAddr from the client-supplied X-Forwarded-For/X-Real-IP header,
	// which would let any client spoof the peer address our auth adapters use
	// for their trusted-proxy decision (see internal/adapters/authelia). The
	// authelia adapter must observe the true TCP peer.
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
			slog.Error("write healthz response", "err", err)
		}
	})
	// /offline: the PWA shell's offline fallback page (Task 17). It sits
	// outside the auth group like /healthz — the service worker serves it
	// from cache with no network (and thus no session) available.
	r.Get("/offline", s.offline)
	fs := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	// /static/sw.js needs its own exact-path route ahead of the wildcard
	// below so we can set Service-Worker-Allowed: / on it — without that
	// response header, a worker served from /static/ cannot register with
	// scope '/' (the browser would otherwise restrict it to /static/*).
	r.Get("/static/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Worker-Allowed", "/")
		fs.ServeHTTP(w, r)
	})
	r.Handle("/static/*", fs)
	r.Group(func(r chi.Router) {
		r.Use(RequireIdentity(s.opts.Auth, s.opts.Identities))
		r.Get("/", s.home)
		r.Get("/sessions", s.sessionsList)
		r.Post("/sessions", s.sessionsCreate)
		r.Get("/sessions/{id}", s.sessionsWorkspace)
		r.Get("/sessions/{id}/activity", s.sessionsActivity)
		r.Post("/sessions/{id}/feedback", s.feedbackRequest)
		r.Post("/corrections/{id}/status", s.correctionStatus)
		r.Post("/documents/{id}", s.documentsSave)
		r.Get("/ai", s.aiRequests)
		r.Post("/ratings", s.ratingsCreate)

		// /api/v1: the versioned JSON API (Task 16). It shares the exact
		// same application services as the HTML routes above — no new
		// application-layer code — proving HTMX isn't the domain boundary
		// (PRD §38). It sits inside this same auth group so RequireIdentity
		// covers it too; see middleware.go for how that middleware emits
		// JSON 401s for paths under /api/ instead of the HTML routes'
		// plain-text body.
		r.Route("/api/v1", func(r chi.Router) {
			r.Get("/sessions", s.apiSessionsList)
			r.Post("/sessions", s.apiSessionsCreate)
			r.Get("/sessions/{id}", s.apiSessionsGet)
			r.Post("/sessions/{id}/feedback", s.apiFeedbackRequest)
			r.Post("/corrections/{id}/status", s.apiCorrectionStatus)
			r.Get("/learner/statistics", s.apiLearnerStatistics)
			r.Post("/ratings", s.apiRatingsCreate)
		})
	})
	return r
}

func (s *Server) HandlerForTest() http.Handler { return s.Handler }

// offline renders the PWA shell's offline fallback (Task 17). It's
// reached two ways: directly, as an ordinary page; and by the service
// worker's fetch handler, which serves this precached response in
// place of a failed navigation. Either way there's no authenticated
// identity available, so the data passed to Render omits Identity —
// the layout's {{with .Identity}} already tolerates that (see the
// topnav, which renders without the "who" span for unauthenticated
// requests).
func (s *Server) offline(w http.ResponseWriter, r *http.Request) {
	Render(w, r, "offline", map[string]any{
		"Title": "オフライン",
	})
}

// recentSessionsLimit is how many of the caller's most-recently-updated
// sessions the dashboard links to — Sessions.List already returns them
// newest-first, so this just truncates.
const recentSessionsLimit = 5

// home renders the Task 14 dashboard: aggregate learner statistics
// (stat tiles + よくある間違い) plus a short list of recent sessions to
// jump back into.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	stats, err := s.opts.Analytics.Statistics(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load statistics", http.StatusInternalServerError)
		return
	}
	recent, err := s.opts.Sessions.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	if len(recent) > recentSessionsLimit {
		recent = recent[:recentSessionsLimit]
	}

	Render(w, r, "home", map[string]any{
		"Title":          "JLP",
		"Identity":       ident,
		"Stats":          stats,
		"RecentSessions": recent,
	})
}
