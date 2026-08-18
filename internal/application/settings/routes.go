package settings

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mikeyaustin/jlp/internal/config"
)

// Pinning a prompt to a provider, at runtime.
//
// APP_AI_ROUTES already chooses a provider chain per prompt, but only at
// boot: changing which model answers the A2A chat meant editing the
// deployment and restarting it. A pin is the same choice, stored, and
// applied to the next request — the counterpart of the model overrides
// this package already holds, and read by internal/adapters/airouter's
// WithPinnedProvider.
//
// A pin names exactly one provider and defeats fallback for that prompt,
// deliberately: an operator who picks an adapter and silently gets
// answers from a different one has not been given a choice at all. That
// is the same rule the per-request override on the workspace follows.

// KeyRoutePrefix + prompt name is the app_settings key holding a pin.
// Prefixed so that a pin can never collide with a model/effort key, and
// so every pin is greppable in one query.
const KeyRoutePrefix = "route."

// RouteKey returns the app_settings key holding promptName's pin.
func RouteKey(promptName string) string { return KeyRoutePrefix + promptName }

// PinnedProvider returns the provider promptName is pinned to, or "" if
// it is not pinned. This is the function handed to airouter, so it sits
// on the hot path of every AI request: it takes the read lock and does
// one map lookup, nothing more.
func (s *Service) PinnedProvider(promptName string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.overrides[RouteKey(promptName)]
}

// SetPinnedProvider pins promptName to provider.
//
// allowed is the set of providers THIS PROCESS BUILT — normally the same
// list the dropdown offers. A pin outside it is refused rather than
// stored, because the router would ignore it: a saved setting that
// silently does nothing is worse than an error, since the UI confirms
// it. Passing the list in rather than consulting the package-level
// `providers` is deliberate: that list drives the model settings page
// and excludes "fake" (nothing to choose), yet "fake" is perfectly
// dispatchable and is the development default.
func (s *Service) SetPinnedProvider(ctx context.Context, promptName, provider string, allowed []string) error {
	if strings.TrimSpace(promptName) == "" {
		return fmt.Errorf("settings: no prompt named")
	}
	if !contains(allowed, provider) {
		return fmt.Errorf("settings: provider %q is not one this process built", provider)
	}
	return s.setOverride(ctx, RouteKey(promptName), provider)
}

// ClearPinnedProvider removes promptName's pin, returning it to whatever
// APP_AI_ROUTES configures.
func (s *Service) ClearPinnedProvider(ctx context.Context, promptName string) error {
	return s.deleteOverride(ctx, RouteKey(promptName))
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// RouteRow is one prompt's line on the agent settings page.
type RouteRow struct {
	// PromptName is the routing key itself ("a2a.chat"), shown as-is:
	// it is what APP_AI_ROUTES uses and what appears in the /ai request
	// log, so translating it would only make those harder to connect.
	PromptName string
	// Configured is the chain APP_AI_ROUTES gives this prompt, empty
	// when it falls through to APP_AI_PROVIDER.
	Configured []string
	// Pinned is the provider chosen in settings, "" when none.
	Pinned string
	// Options are the providers this process actually built, so the
	// dropdown cannot offer something that could never answer.
	Options []string
}

// Effective is what the next request for this prompt will use, and where
// that came from.
func (r RouteRow) Effective() (provider, source string) {
	switch {
	case r.Pinned != "":
		return r.Pinned, "pinned"
	case len(r.Configured) > 0:
		return strings.Join(r.Configured, " → "), "config"
	default:
		return "", "default"
	}
}

// RouteRows builds the agent settings page: one row per prompt name,
// sorted, with the providers this process can actually dispatch to.
//
// promptNames comes from the composition root (cmd/jlp), which is the
// only place that knows both the structured and tool-calling prompt
// lists — this package deliberately does not restate them, so a new
// prompt appears here by being registered there rather than by someone
// remembering two lists.
func (s *Service) RouteRows(promptNames, constructed []string) []RouteRow {
	configured, err := config.ParseRoutes(s.cfg.Routes)
	if err != nil {
		// The same string was validated at boot, so this cannot normally
		// fail; treating it as "nothing configured" keeps the page
		// rendering rather than 500ing on a display concern.
		configured = nil
	}

	options := append([]string(nil), constructed...)
	sort.Strings(options)

	names := append([]string(nil), promptNames...)
	sort.Strings(names)

	rows := make([]RouteRow, 0, len(names))
	for _, name := range names {
		rows = append(rows, RouteRow{
			PromptName: name,
			Configured: configured[name],
			Pinned:     s.PinnedProvider(name),
			Options:    options,
		})
	}
	return rows
}
