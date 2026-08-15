// Package observability wraps an ai.StructuredGenerator so every call
// through it — success or failure — is measured and recorded. It is
// the single place cost, latency, and provider/model usage are known,
// so every AI capability that goes through it gets that audit trail
// for free, without each capability having to remember to log it.
package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ModelPricing is USD per million tokens, input and output priced
// separately as providers typically do.
type ModelPricing struct{ InPerMTok, OutPerMTok float64 }

type observer struct {
	inner   ai.StructuredGenerator
	repo    storage.AIRequestRepository
	pricing map[string]ModelPricing
}

// NewAIObserver wraps inner so every GenerateStructured call is timed,
// costed, and inserted into repo as an AIRequestRecord — regardless of
// whether inner succeeds. A model absent from pricing costs $0 rather
// than erroring, so an unpriced or free model (the fake provider, for
// instance) doesn't break the observed call.
func NewAIObserver(inner ai.StructuredGenerator, repo storage.AIRequestRepository, pricing map[string]ModelPricing) ai.StructuredGenerator {
	return &observer{inner: inner, repo: repo, pricing: pricing}
}

func (o *observer) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	requestID := uuid.NewString()
	start := time.Now()

	resp, callErr := o.inner.GenerateStructured(ctx, req)
	latency := time.Since(start)

	// Stamp identity/timing onto the response regardless of success so
	// callers (and the record below) always have them. The decorator
	// wraps the whole call, so its own measurement is the authoritative
	// latency — it overrides whatever (if anything) the inner generator
	// self-reported.
	resp.RequestID = requestID
	resp.Latency = latency

	rec := storage.AIRequestRecord{
		ID:            requestID,
		Capability:    "structured-generation",
		Provider:      resp.Provider,
		Model:         resp.Model,
		PromptName:    req.PromptName,
		PromptVersion: req.PromptVersion,
		IdentityID:    req.IdentityID,
		SessionID:     req.SessionID,
		LatencyMS:     int(latency.Milliseconds()),
		InputTokens:   resp.InputTokens,
		OutputTokens:  resp.OutputTokens,
		CostUSD:       cost(o.pricing[resp.Model], resp.InputTokens, resp.OutputTokens),
		Success:       callErr == nil,
		CreatedAt:     start,
		Agent:         req.Agent,
	}
	if callErr != nil {
		rec.Error = callErr.Error()
	}

	// Observability must never break the feature: an insert failure is
	// logged, not surfaced as an error from GenerateStructured.
	if err := o.repo.Insert(ctx, rec); err != nil {
		slog.Error("ai_requests: insert failed", "request_id", requestID, "err", err)
	}

	return resp, callErr
}

func cost(p ModelPricing, inputTokens, outputTokens int) float64 {
	return float64(inputTokens)/1_000_000*p.InPerMTok + float64(outputTokens)/1_000_000*p.OutPerMTok
}

// toolObserver wraps an ai.ToolCaller exactly the way observer above
// wraps an ai.StructuredGenerator: every CallWithTools call, success or
// failure, becomes its own ai_requests row. A multi-turn tool-calling
// conversation therefore produces one row PER TURN (one CallWithTools
// call == one model round trip == one turn), not one row for the whole
// conversation — matching airouter's own "every attempt is its own
// row" convention, just one level up: here every TURN is its own row.
type toolObserver struct {
	inner   ai.ToolCaller
	repo    storage.AIRequestRepository
	pricing map[string]ModelPricing
}

// NewToolObserver wraps inner so every CallWithTools turn is timed,
// costed, and inserted into repo as an AIRequestRecord (Capability
// "tool-calling", distinct from NewAIObserver's "structured-
// generation" so the two capabilities' usage can be told apart in the
// ai_requests audit log) — regardless of whether inner succeeds, same
// never-lose-a-record contract as NewAIObserver.
func NewToolObserver(inner ai.ToolCaller, repo storage.AIRequestRepository, pricing map[string]ModelPricing) ai.ToolCaller {
	return &toolObserver{inner: inner, repo: repo, pricing: pricing}
}

func (o *toolObserver) CallWithTools(ctx context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	requestID := uuid.NewString()
	start := time.Now()

	resp, callErr := o.inner.CallWithTools(ctx, req)
	latency := time.Since(start)

	resp.RequestID = requestID
	resp.Latency = latency

	rec := storage.AIRequestRecord{
		ID:            requestID,
		Capability:    "tool-calling",
		Provider:      resp.Provider,
		Model:         resp.Model,
		PromptName:    req.PromptName,
		PromptVersion: req.PromptVersion,
		IdentityID:    req.IdentityID,
		SessionID:     req.SessionID,
		LatencyMS:     int(latency.Milliseconds()),
		InputTokens:   resp.InputTokens,
		OutputTokens:  resp.OutputTokens,
		CostUSD:       cost(o.pricing[resp.Model], resp.InputTokens, resp.OutputTokens),
		Success:       callErr == nil,
		CreatedAt:     start,
		Agent:         req.Agent,
	}
	if callErr != nil {
		rec.Error = callErr.Error()
	}

	if err := o.repo.Insert(ctx, rec); err != nil {
		slog.Error("ai_requests: insert failed", "request_id", requestID, "err", err)
	}

	return resp, callErr
}
