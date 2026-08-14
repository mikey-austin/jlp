package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// documentsSave handles autosave POSTs from the workspace editor
// (web/static/js/app.js): form field "content" in, JSON
// {"version","saved_at","runes"} out.
func (s *Server) documentsSave(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := writing.DocumentID(chi.URLParam(r, "id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	doc, err := s.opts.Writing.Autosave(r.Context(), ident.ID, id, r.FormValue("content"))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not save document", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"version":  doc.Version,
		"saved_at": doc.UpdatedAt.Format(time.RFC3339),
		"runes":    doc.RuneCount(),
	})
}
