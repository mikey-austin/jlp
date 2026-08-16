package httpx

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// settingsPage handles GET /settings (Phase 4 Task S): one row per AI
// provider, its current EFFECTIVE model (and effort, for the two CLI
// providers), and whether that value came from config or an operator
// override. Linked from /ai — see server.go's routes() comment on why
// this deliberately isn't a new top-level nav item.
func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	Render(w, r, "settings", map[string]any{
		"Title":    "設定",
		"Identity": ident,
		"Rows":     s.opts.Settings.Rows(r.Context()),
	})
}

// settingsSetModel handles POST /settings/{provider}/model: form field
// "model". A validation failure (unknown provider, empty model) is
// re-rendered on the SAME page with the error message — never a bare
// 500 or a silent no-op — and nothing is written to the database.
func (s *Server) settingsSetModel(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(r.FormValue("model"))
	if err := s.opts.Settings.SetModel(r.Context(), provider, model); err != nil {
		s.renderSettingsError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// settingsSetEffort handles POST /settings/{provider}/effort: form
// field "effort", validated against that TOOL's own accepted
// vocabulary (config.ValidateEffort — the one shared definition
// config.Load's boot-time validation also uses). An invalid effort
// (e.g. "max" for codexcli, "minimal" for claudecli) is rejected here
// and re-rendered with the error — never written to the database.
func (s *Server) settingsSetEffort(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	effort := strings.TrimSpace(r.FormValue("effort"))
	if err := s.opts.Settings.SetEffort(r.Context(), provider, effort); err != nil {
		s.renderSettingsError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// settingsReset handles POST /settings/{provider}/reset: the "reset to
// config" action, deleting any override row(s) for provider.
func (s *Server) settingsReset(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if err := s.opts.Settings.Reset(r.Context(), provider); err != nil {
		s.renderSettingsError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// renderSettingsError re-renders the /settings page with err's message
// surfaced in the form — the brief's "invalid input must be rejected
// ... and surfaced in the form" requirement — rather than a generic
// error page a learner/operator would have to interpret against server
// logs. 422 Unprocessable Entity: the request was well-formed but its
// content was rejected by validation, distinct from the 400s
// ParseForm failures above use for a genuinely malformed request.
func (s *Server) renderSettingsError(w http.ResponseWriter, r *http.Request, err error) {
	ident, _ := IdentityFrom(r.Context())
	w.WriteHeader(http.StatusUnprocessableEntity)
	Render(w, r, "settings", map[string]any{
		"Title":    "設定",
		"Identity": ident,
		"Rows":     s.opts.Settings.Rows(r.Context()),
		"Error":    err.Error(),
	})
}
