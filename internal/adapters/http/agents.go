package httpx

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	appsettings "github.com/mikeyaustin/jlp/internal/application/settings"
)

// The agent-provider settings page (/settings/agents).
//
// APP_AI_ROUTES already chooses which adapter answers each prompt, but
// only at boot. This is the same choice at runtime: pick a provider for
// a prompt and the next request uses it, with no restart and no
// redeploy — the counterpart of the model settings next door.

// agentsPage handles GET /settings/agents.
func (s *Server) agentsPage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	s.render(w, r, "agents", map[string]any{
		"Title":    "エージェントのプロバイダー",
		"Identity": ident,
		"Rows":     s.agentRows(),
		"Error":    r.URL.Query().Get("error"),
	})
}

// agentsSet handles POST /settings/agents/{prompt}: form field
// "provider". An empty value clears the pin, returning the prompt to
// its configured route — that is the "設定のまま" option, and it is why
// clearing is a normal save rather than a separate control.
func (s *Server) agentsSet(w http.ResponseWriter, r *http.Request) {
	if s.opts.Settings == nil {
		http.NotFound(w, r)
		return
	}
	prompt := chi.URLParam(r, "prompt")
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/agents?error="+url.QueryEscape("フォームを読み取れませんでした。"), http.StatusSeeOther)
		return
	}

	// The prompt name must be one this process actually routes. Without
	// this, a hand-posted name would store a pin that nothing ever reads
	// — a setting that silently does nothing is worse than a rejection.
	if !s.knowsPrompt(prompt) {
		slog.Warn("settings: pin for an unknown prompt refused", "prompt", prompt)
		http.Redirect(w, r, "/settings/agents?error="+url.QueryEscape("知らないプロンプトです。"), http.StatusSeeOther)
		return
	}

	// The provider must be one THIS PROCESS BUILT — the same list the
	// dropdown offers. A pin naming, say, gemini in a deployment with no
	// Gemini key would otherwise be stored happily and then ignored by
	// the router: a saved setting that does nothing, which is the same
	// failure as the unknown-prompt case above. The check lives in the
	// service so there is one answer; this passes it the list.
	provider := r.FormValue("provider")
	var err error
	if provider == "" {
		err = s.opts.Settings.ClearPinnedProvider(r.Context(), prompt)
	} else {
		err = s.opts.Settings.SetPinnedProvider(r.Context(), prompt, provider, s.opts.AIProviders)
	}
	if err != nil {
		slog.Error("settings: pin provider", "err", err, "prompt", prompt, "provider", provider)
		http.Redirect(w, r, "/settings/agents?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings/agents", http.StatusSeeOther)
}

func (s *Server) agentRows() []appsettings.RouteRow {
	if s.opts.Settings == nil {
		return nil
	}
	return s.opts.Settings.RouteRows(s.opts.PromptNames, s.opts.AIProviders)
}

func (s *Server) knowsPrompt(name string) bool {
	for _, n := range s.opts.PromptNames {
		if n == name {
			return true
		}
	}
	return false
}
