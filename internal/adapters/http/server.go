package httpx

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Options struct {
	Addr       string
	Auth       auth.Authenticator
	Identities storage.IdentityRepository
	Sessions   *sessions.Service
	Writing    *appwriting.Service
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
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			ident, _ := IdentityFrom(r.Context())
			Render(w, r, "home", map[string]any{"Title": "JLP", "Identity": ident})
		})
		r.Get("/sessions", s.sessionsList)
		r.Post("/sessions", s.sessionsCreate)
		r.Get("/sessions/{id}", s.sessionsWorkspace)
		r.Post("/documents/{id}", s.documentsSave)
	})
	return r
}

func (s *Server) HandlerForTest() http.Handler { return s.Handler }
