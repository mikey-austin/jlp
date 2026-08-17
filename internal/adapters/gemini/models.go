package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/mikeyaustin/jlp/internal/config"
)

// ErrNoAPIKey is what ListModels returns when no key was configured:
// there is nothing to authenticate the call with, so it is never
// attempted — same contract as adapters/anthropic's lister.
var ErrNoAPIKey = errors.New("gemini: no API key configured, cannot list models")

// ModelLister implements ports/ai.ModelLister against GET
// {baseurl}/v1beta/models (Phase 4 Task M) — a separate, lazily-invoked
// capability from generator's own generateContent calls, and a separate
// type with its own client, so constructing one never implies
// constructing the other.
type ModelLister struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewModelLister returns a ModelLister for cfg. Like New, this never
// dials out at construction — the GET only happens inside ListModels,
// called lazily from /settings, never at boot.
func NewModelLister(cfg config.Gemini) *ModelLister {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &ModelLister{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: cfg.APIKey, client: &http.Client{}}
}

// modelsListResponse is the subset of GET /v1beta/models this lister
// reads, confirmed against the captured live body in testdata/models.json
// (53 entries for this deployment's key). Each entry's name is a
// RESOURCE name, "models/gemini-3-flash-preview"; the bare id after the
// prefix is what APP_AI_GEMINI_MODEL and the generateContent path both
// want.
type modelsListResponse struct {
	Models []struct {
		Name                       string   `json:"name"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
}

// ListModels returns every model the API reports as supporting
// generateContent, in the order it lists them. Embedding, image and
// AQA models are filtered out rather than offered as nonsensical chat
// choices — the same "capabilities are the signal" rule
// adapters/ollama's lister applies, and here it removes 16 of the 53
// entries the live account sees.
//
// One request, no pagination: pageSize=1000 returned the whole list
// with no nextPageToken, and a single bounded call is what
// application/settings.Service's short per-attempt timeout assumes. ctx
// bounds this call; the caller attaches that timeout.
func (l *ModelLister) ListModels(ctx context.Context) ([]string, error) {
	if l.apiKey == "" {
		return nil, ErrNoAPIKey
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+"/v1beta/models?pageSize=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("gemini: new request: %w", err)
	}
	req.Header.Set("x-goog-api-key", l.apiKey)

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: list models: %s", l.redactKey(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Status only, no body: /settings renders this string beside the
		// provider's row, and a body is one more place a credential could
		// ride along.
		return nil, fmt.Errorf("gemini: list models: status %d", resp.StatusCode)
	}

	var body modelsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("gemini: decode /v1beta/models: %w", err)
	}

	names := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		if !slices.Contains(m.SupportedGenerationMethods, "generateContent") {
			continue
		}
		names = append(names, strings.TrimPrefix(m.Name, "models/"))
	}
	return names, nil
}

// redactKey mirrors generator.redactKey for the lister's own errors.
func (l *ModelLister) redactKey(s string) string {
	if l.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, l.apiKey, "[REDACTED]")
}
