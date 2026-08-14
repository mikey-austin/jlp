package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
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
	// Cap the autosave body: without this a client (malicious or just a
	// runaway editor buffer) could stream an unbounded body and have
	// ParseForm read all of it into memory. MaxBytesReader makes the
	// oversized read fail instead, which ParseForm surfaces as an error
	// handled by the branch below (400, not a panic).
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
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
	if err := json.NewEncoder(w).Encode(map[string]any{
		"version":  doc.Version,
		"saved_at": doc.UpdatedAt.Format(time.RFC3339),
		"runes":    doc.RuneCount(),
	}); err != nil {
		slog.Error("write autosave response", "err", err)
	}
}
