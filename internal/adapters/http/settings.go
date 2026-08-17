package httpx

import (
	"fmt"
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
	s.render(w, r, "settings", map[string]any{
		"Title":    "設定",
		"Identity": ident,
		"Rows":     s.opts.Settings.Rows(r.Context(), s.opts.AIProviders),
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
	if err := s.rejectUnavailableProvider(w, r, provider); err != nil {
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
	if err := s.rejectUnavailableProvider(w, r, provider); err != nil {
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

// rejectUnavailableProvider is the write-side half of whole-branch
// review I-2: an override for a provider this process never constructed
// can never take effect, so saving one and answering 303 would be a
// success-shaped no-op. The page already hides the controls for such a
// provider (settings.Row.Available), but the routes stay reachable — a
// stale tab, a bookmarked form, curl — so the rejection lives here too,
// surfaced in the form exactly like a validation failure. Returns a
// non-nil error when it has already written a response; the caller must
// return immediately.
func (s *Server) rejectUnavailableProvider(w http.ResponseWriter, r *http.Request, provider string) error {
	if s.opts.Settings.IsAvailable(provider, s.opts.AIProviders) {
		return nil
	}
	err := fmt.Errorf("%q は、この配備では構成されていないため変更できません（起動時に生成されたAIプロバイダー: %s）",
		provider, strings.Join(s.opts.AIProviders, ", "))
	s.renderSettingsError(w, r, err)
	return err
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
	s.render(w, r, "settings", map[string]any{
		"Title":    "設定",
		"Identity": ident,
		"Rows":     s.opts.Settings.Rows(r.Context(), s.opts.AIProviders),
		"Error":    err.Error(),
	})
}
