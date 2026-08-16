package ai

// ModelResolver resolves the model (and, for the two CLI providers,
// effort) an adapter should use for the NEXT call — checked at call
// time, not baked into the adapter at construction. This is the
// mechanism that lets an operator change a provider's model from the
// /settings page (Phase 4 Task S) and have it take effect on the very
// next AI request, with no process restart: every adapter
// (adapters/ollama, adapters/anthropic, adapters/clicmd's two CLI
// generators) holds a ModelResolver instead of a fixed model string,
// and calls Model again on every GenerateStructured/CallWithTools
// invocation rather than reading a value captured once at boot.
//
// provider is one of "ollama", "anthropic", "claudecli", "codexcli" —
// the same provider names config.AI's routing already uses. effort is
// always "" for a provider with no such concept (ollama, anthropic);
// model or effort may individually be "" to mean "no override for
// this one, fall back to APP_AI_* config" — the implementation (see
// application/settings.Service) is what actually applies that
// fallback, not the adapter.
//
// Implementations must be safe for concurrent use: Model is called
// from every HTTP request goroutine that triggers an AI call, while a
// settings change (a different goroutine, via the /settings handlers)
// can write concurrently.
type ModelResolver interface {
	Model(provider string) (model, effort string)
}
