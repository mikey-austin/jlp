package observability

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// cannedGenerator returns a fixed response (or error) after sleeping
// delay, standing in for a real provider call in these tests.
type cannedGenerator struct {
	delay time.Duration
	resp  ai.StructuredResponse
	err   error
}

func (g *cannedGenerator) GenerateStructured(_ context.Context, _ ai.StructuredRequest) (ai.StructuredResponse, error) {
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	return g.resp, g.err
}

// memRepo is an in-memory storage.AIRequestRepository that captures
// every inserted record, optionally failing Insert to exercise the
// decorator's log-only-on-insert-failure path.
type memRepo struct {
	mu       sync.Mutex
	records  []storage.AIRequestRecord
	insertFn func(storage.AIRequestRecord) error
}

func (r *memRepo) Insert(_ context.Context, rec storage.AIRequestRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insertFn != nil {
		if err := r.insertFn(rec); err != nil {
			return err
		}
	}
	r.records = append(r.records, rec)
	return nil
}

func (r *memRepo) List(_ context.Context, identity learner.IdentityID, limit int) ([]storage.AIRequestRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []storage.AIRequestRecord
	for _, rec := range r.records {
		if rec.IdentityID == identity {
			out = append(out, rec)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) only() storage.AIRequestRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) != 1 {
		panic("memRepo.only: want exactly 1 record")
	}
	return r.records[0]
}

var testPricing = map[string]ModelPricing{
	"fake-1": {InPerMTok: 3.0, OutPerMTok: 15.0},
}

func testRequest() ai.StructuredRequest {
	sid := session.ID("11111111-1111-1111-1111-111111111111")
	return ai.StructuredRequest{
		PromptName:    "teacher.feedback",
		PromptVersion: "v1",
		System:        "sys",
		User:          "user",
		SchemaName:    "correction_result.v1",
		IdentityID:    learner.IdentityID("learner-1"),
		SessionID:     &sid,
		Agent:         "teacher",
	}
}

func TestObserverInsertsRecordAndStampsRequestID(t *testing.T) {
	inner := &cannedGenerator{
		delay: 5 * time.Millisecond,
		resp: ai.StructuredResponse{
			JSON:         json.RawMessage(`{"corrections":[]}`),
			Provider:     "fake",
			Model:        "fake-1",
			InputTokens:  1000,
			OutputTokens: 500,
		},
	}
	repo := &memRepo{}
	observer := NewAIObserver(inner, repo, testPricing)

	resp, err := observer.GenerateStructured(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}

	if resp.RequestID == "" {
		t.Fatal("RequestID is empty, want it stamped by the observer")
	}
	if resp.Latency < 5*time.Millisecond {
		t.Errorf("Latency = %v, want >= 5ms", resp.Latency)
	}

	rec := repo.only()
	if rec.ID != resp.RequestID {
		t.Errorf("record.ID = %q, want it to equal resp.RequestID %q", rec.ID, resp.RequestID)
	}
	if rec.LatencyMS < 5 {
		t.Errorf("record.LatencyMS = %d, want >= 5", rec.LatencyMS)
	}
	if !rec.Success {
		t.Error("record.Success = false, want true")
	}
	if rec.Error != "" {
		t.Errorf("record.Error = %q, want empty", rec.Error)
	}
	if rec.Provider != "fake" || rec.Model != "fake-1" {
		t.Errorf("record Provider/Model = %q/%q, want fake/fake-1", rec.Provider, rec.Model)
	}
	if rec.PromptName != "teacher.feedback" || rec.PromptVersion != "v1" {
		t.Errorf("record PromptName/Version = %q/%q, want teacher.feedback/v1", rec.PromptName, rec.PromptVersion)
	}
	if rec.IdentityID != learner.IdentityID("learner-1") {
		t.Errorf("record.IdentityID = %q, want learner-1", rec.IdentityID)
	}
	if rec.SessionID == nil || *rec.SessionID != *testRequest().SessionID {
		t.Errorf("record.SessionID = %v, want %v", rec.SessionID, testRequest().SessionID)
	}

	// 1000 input tokens + 500 output tokens at {3.0, 15.0} per MTok:
	// 1000/1e6*3.0 + 500/1e6*15.0 = 0.003 + 0.0075 = 0.0105.
	wantCost := 0.0105
	if diff := rec.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("record.CostUSD = %v, want %v", rec.CostUSD, wantCost)
	}
}

func TestObserverRecordsFailureAndPropagatesError(t *testing.T) {
	innerErr := errors.New("provider exploded")
	inner := &cannedGenerator{err: innerErr}
	repo := &memRepo{}
	observer := NewAIObserver(inner, repo, testPricing)

	_, err := observer.GenerateStructured(context.Background(), testRequest())
	if !errors.Is(err, innerErr) {
		t.Fatalf("GenerateStructured error = %v, want it to wrap %v", err, innerErr)
	}

	rec := repo.only()
	if rec.Success {
		t.Error("record.Success = true, want false")
	}
	if rec.Error == "" {
		t.Error("record.Error is empty, want the inner error message")
	}
	if rec.ID == "" {
		t.Error("record.ID is empty even on failure, want a request id")
	}
}

func TestObserverInsertFailureIsLogOnly(t *testing.T) {
	inner := &cannedGenerator{
		resp: ai.StructuredResponse{
			JSON:     json.RawMessage(`{"corrections":[]}`),
			Provider: "fake",
			Model:    "fake-1",
		},
	}
	repo := &memRepo{insertFn: func(storage.AIRequestRecord) error {
		return errors.New("db is down")
	}}
	observer := NewAIObserver(inner, repo, testPricing)

	resp, err := observer.GenerateStructured(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("GenerateStructured returned error even though only the insert failed: %v", err)
	}
	if resp.RequestID == "" {
		t.Error("RequestID is empty, want it stamped even when insert fails")
	}
}

func TestObserverUnknownModelPricingDefaultsToZeroCost(t *testing.T) {
	inner := &cannedGenerator{
		resp: ai.StructuredResponse{
			JSON:         json.RawMessage(`{"corrections":[]}`),
			Provider:     "fake",
			Model:        "unpriced-model",
			InputTokens:  1000,
			OutputTokens: 1000,
		},
	}
	repo := &memRepo{}
	observer := NewAIObserver(inner, repo, testPricing)

	if _, err := observer.GenerateStructured(context.Background(), testRequest()); err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}
	rec := repo.only()
	if rec.CostUSD != 0 {
		t.Errorf("record.CostUSD = %v, want 0 for a model absent from the pricing map", rec.CostUSD)
	}
}
