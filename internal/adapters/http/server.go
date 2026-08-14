package httpx

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Options struct {
	Addr string
}

type Server struct {
	http.Server
}

func NewServer(opts Options) *Server {
	s := &Server{}
	s.Addr = opts.Addr
	s.Handler = s.routes()
	return s
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	fs := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	r.Handle("/static/*", fs)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		Render(w, r, "home", map[string]any{"Title": "JLP"})
	})
	return r
}

func (s *Server) HandlerForTest() http.Handler { return s.Handler }
