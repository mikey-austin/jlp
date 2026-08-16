package ai

import "context"

// ModelLister is an OPTIONAL capability (Phase 4 Task M): a provider
// adapter implements it only when it has a genuine, verifiable source
// of truth for which models it can currently serve — Ollama's
// /api/tags, the agy CLI's `agy models`, Anthropic's /v1/models when an
// API key is configured. application/settings.Service holds one
// ModelLister per provider that has one (never all five: the Claude
// Code and Codex CLIs have no way to enumerate — see that package's own
// doc comment) and uses it to decide whether /settings renders a
// dropdown for that provider or falls back to free text.
//
// Contract for any implementation:
//   - Never called at boot. The only caller is /settings rendering, so
//     a slow or absent backend must never delay or fail startup.
//   - ctx bounds a single attempt; ListModels must not retry or spawn a
//     goroutine that outlives ctx. The caller (application/settings.Service)
//     is expected to attach its own short timeout, not rely on this
//     method to self-limit.
//   - A returned list must be genuinely observed from the provider,
//     never a hand-maintained guess — an implementation that cannot
//     verify what it returns should return an error instead.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}
