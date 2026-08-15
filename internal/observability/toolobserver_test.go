package observability

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// cannedToolCaller mirrors cannedGenerator (airequests_test.go) for
// ai.ToolCaller.
type cannedToolCaller struct {
	resp ai.ToolResponse
	err  error
}

func (c *cannedToolCaller) CallWithTools(_ context.Context, _ ai.ToolRequest) (ai.ToolResponse, error) {
	return c.resp, c.err
}

func testToolRequest() ai.ToolRequest {
	sid := session.ID("11111111-1111-1111-1111-111111111111")
	return ai.ToolRequest{
		PromptName:    "teacher.agentic",
		PromptVersion: "v1",
		System:        "sys",
		Messages:      []ai.ToolMessage{{Role: "user", Text: "hi"}},
		IdentityID:    learner.IdentityID("learner-1"),
		SessionID:     &sid,
		Agent:         "teacher",
	}
}

func TestToolObserverInsertsOneRecordPerTurnAndStampsRequestID(t *testing.T) {
	repo := &memRepo{}
	inner := &cannedToolCaller{resp: ai.ToolResponse{
		Provider: "fake", Model: "fake-1",
		Turn:        ai.ToolTurn{Invocations: []ai.ToolInvocation{{ID: "1", Name: "get_learning_priorities"}}},
		InputTokens: 100, OutputTokens: 10,
	}}
	obs := NewToolObserver(inner, repo, testPricing)

	resp, err := obs.CallWithTools(context.Background(), testToolRequest())
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if resp.RequestID == "" {
		t.Fatal("RequestID is empty, want the decorator to stamp one")
	}

	rec := repo.only()
	if rec.Capability != "tool-calling" {
		t.Errorf("Capability = %q, want tool-calling", rec.Capability)
	}
	if rec.PromptName != "teacher.agentic" || rec.Agent != "teacher" {
		t.Errorf("PromptName/Agent = %q/%q, want teacher.agentic/teacher", rec.PromptName, rec.Agent)
	}
	if rec.Success != true {
		t.Errorf("Success = %v, want true", rec.Success)
	}
	if rec.ID != resp.RequestID {
		t.Errorf("rec.ID = %q, want it to match resp.RequestID %q", rec.ID, resp.RequestID)
	}
}

func TestToolObserverRecordsFailureAndPropagatesError(t *testing.T) {
	repo := &memRepo{}
	wantErr := errors.New("boom")
	inner := &cannedToolCaller{resp: ai.ToolResponse{Provider: "fake"}, err: wantErr}
	obs := NewToolObserver(inner, repo, testPricing)

	_, err := obs.CallWithTools(context.Background(), testToolRequest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("CallWithTools() err = %v, want %v", err, wantErr)
	}

	rec := repo.only()
	if rec.Success {
		t.Error("Success = true, want false for a failed call")
	}
	if rec.Error != wantErr.Error() {
		t.Errorf("Error = %q, want %q", rec.Error, wantErr.Error())
	}
}

func TestToolObserverInsertFailureIsLogOnly(t *testing.T) {
	repo := &memRepo{insertFn: func(storage.AIRequestRecord) error { return errors.New("db down") }}
	inner := &cannedToolCaller{resp: ai.ToolResponse{Provider: "fake", Model: "fake-1"}}
	obs := NewToolObserver(inner, repo, testPricing)

	_, err := obs.CallWithTools(context.Background(), testToolRequest())
	if err != nil {
		t.Fatalf("CallWithTools() err = %v, want nil — an insert failure must not surface as a call failure", err)
	}
}
