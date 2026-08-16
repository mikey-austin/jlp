package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/mikeyaustin/jlp/internal/config"
)

// ModelLister implements ports/ai.ModelLister against a local Ollama
// server's GET /api/tags (Phase 4 Task M) — a separate, lazily-invoked
// capability from generator's own /api/chat calls, and a separate type
// rather than a second method on generator so constructing one never
// implies constructing (or dialing) the other.
type ModelLister struct {
	url    string
	client *http.Client
}

// NewModelLister returns a ModelLister for cfg.URL. Like generator's
// own New, this never dials out at construction — GET /api/tags only
// happens inside ListModels, called lazily from /settings, never at
// boot.
func NewModelLister(cfg config.Ollama) *ModelLister {
	return &ModelLister{url: cfg.URL, client: &http.Client{}}
}

// tagsResponse/tagsModel mirror /api/tags' response shape, confirmed
// live on this machine (task brief): each entry carries a
// "capabilities" array alongside "details" — completion-capable chat
// models advertise "completion" there, while a pure embedding model
// (e.g. mxbai-embed-large) advertises only "embedding" and nothing
// else. Only the fields this lister reads.
type tagsResponse struct {
	Models []tagsModel `json:"models"`
}

type tagsModel struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

// ListModels returns every model /api/tags reports as "completion"
// capable, in the server's own listed order — an embedding-only model
// is filtered out rather than offered as a nonsensical chat-model
// choice (see tagsModel's doc comment on why "capabilities" is the
// signal, not "details.family" guesswork). ctx bounds this single HTTP
// call; the caller (application/settings.Service) is expected to
// attach a short timeout.
func (l *ModelLister) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("ollama: new request: %w", err)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama: list models: status %d", resp.StatusCode)
	}

	var body tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("ollama: decode /api/tags: %w", err)
	}

	names := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		if !slices.Contains(m.Capabilities, "completion") {
			continue
		}
		names = append(names, m.Name)
	}
	return names, nil
}
