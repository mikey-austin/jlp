package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/mikeyaustin/jlp/internal/config"
)

// ErrNoAPIKey is what ListModels returns when cfg.APIKey was empty at
// construction (task brief: "yes, when a key is set" — no key means
// there is nothing to authenticate the call with, so it is never even
// attempted).
var ErrNoAPIKey = errors.New("anthropic: no API key configured, cannot list models")

// defaultBaseURL mirrors config's own APP_AI_ANTHROPIC_BASEURL default
// (internal/config.Load's "ai.anthropic.baseurl" viper default) — kept
// here too so a ModelLister built with a zero-value config.Anthropic
// (e.g. in a test that only sets APIKey) still targets the real API
// rather than an empty string.
const defaultBaseURL = "https://api.anthropic.com"

// anthropicAPIVersion is the exact header value anthropic-sdk-go sends
// on every request (internal/requestconfig.go in that module) — this
// lister makes its own plain HTTP call rather than pulling in the SDK's
// much larger generated ModelInfo type (deeply nested "capabilities"
// fields this feature has no use for), so it repeats just this one
// header rather than the whole client.
const anthropicAPIVersion = "2023-06-01"

// ModelLister implements ports/ai.ModelLister against GET
// {baseurl}/v1/models (Phase 4 Task M) — a separate, lazily-invoked
// capability from generator's own Messages API calls, and a separate
// type (with its own plain http.Client, not generator's SDK client) so
// constructing one never implies constructing the other.
type ModelLister struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewModelLister returns a ModelLister for cfg. Like generator's own
// New, this never dials out at construction — GET /v1/models only
// happens inside ListModels, called lazily from /settings, never at
// boot.
func NewModelLister(cfg config.Anthropic) *ModelLister {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &ModelLister{baseURL: baseURL, apiKey: cfg.APIKey, client: &http.Client{}}
}

// modelsListResponse is the subset of GET /v1/models' response shape
// this lister reads — just the id of each entry.
type modelsListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels calls GET /v1/models with the configured API key, or
// returns ErrNoAPIKey immediately (no network call at all) when none
// was configured. Anthropic lists models most-recently-released first
// and this lister asks for the maximum page size (1000) in one request
// rather than paginating, since a single bounded call is what
// application/settings.Service's short per-attempt timeout assumes.
// ctx bounds this call; the caller is expected to attach that timeout.
func (l *ModelLister) ListModels(ctx context.Context) ([]string, error) {
	if l.apiKey == "" {
		return nil, ErrNoAPIKey
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+"/v1/models?limit=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic: new request: %w", err)
	}
	req.Header.Set("x-api-key", l.apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic: list models: status %d", resp.StatusCode)
	}

	var body modelsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("anthropic: decode /v1/models: %w", err)
	}

	names := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		names = append(names, m.ID)
	}
	return names, nil
}
