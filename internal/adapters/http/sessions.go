package httpx

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func (s *Server) sessionsList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	list, err := s.opts.Sessions.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	Render(w, r, "sessions", map[string]any{
		"Title":    "セッション",
		"Identity": ident,
		"Sessions": list,
	})
}

func (s *Server) sessionsCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	profile := session.Profile{
		Audience:            r.FormValue("audience"),
		Tone:                r.FormValue("tone"),
		Register:            r.FormValue("register"),
		TeacherMode:         r.FormValue("teacher_mode"),
		ExplanationLanguage: r.FormValue("explanation_language"),
		Strictness:          r.FormValue("strictness"),
	}
	sess, err := s.opts.Sessions.Create(r.Context(), ident.ID, r.FormValue("title"), r.FormValue("purpose"), profile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/sessions/"+string(sess.ID), http.StatusSeeOther)
}

func (s *Server) sessionsWorkspace(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := session.ID(chi.URLParam(r, "id"))
	sess, err := s.opts.Sessions.Get(r.Context(), ident.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load session", http.StatusInternalServerError)
		return
	}
	doc, err := s.opts.Writing.Open(r.Context(), ident.ID, sess.ID)
	if err != nil {
		http.Error(w, "could not load document", http.StatusInternalServerError)
		return
	}
	Render(w, r, "workspace", map[string]any{
		"Title":    sess.Title,
		"Identity": ident,
		"Session":  sess,
		"Document": doc,
	})
}
