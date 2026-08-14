package httpx

import (
	"net/http"

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
		w.Write([]byte(`{"status":"ok"}`))
	})
	fs := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
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
	})
	return r
}

func (s *Server) HandlerForTest() http.Handler { return s.Handler }

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
