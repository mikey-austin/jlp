package aiutil_test

import (
	"context"
	"testing"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// pingSchema is a trivial schema this package's own test can validate
// against without depending on any real agent's schema — aiutil is
// schema-agnostic, so any registered schema name works to pin its
// repair/retry control flow. correction_result.v1 is the smallest
// schema already embedded in package schemas.
const pingSchema = "correction_result.v1"

// flakyGen is a local ai.StructuredGenerator test double returning a
// fixed sequence of raw payloads — the same shape
// internal/agent/teacher's own flakyGen used before this package
// existed, now exercised directly against aiutil instead of through the
// teacher agent.
type flakyGen struct {
	calls    int
	payloads [][]byte
}

func (f *flakyGen) GenerateStructured(context.Context, ai.StructuredRequest) (ai.StructuredResponse, error) {
	i := f.calls
	if i >= len(f.payloads) {
		i = len(f.payloads) - 1
	}
	f.calls++
	return ai.StructuredResponse{JSON: f.payloads[i], Provider: "flaky", Model: "flaky-1"}, nil
}

func TestValidateWithRepairAndRetryAcceptsValidResponseWithoutCallingGen(t *testing.T) {
	gen := &flakyGen{payloads: [][]byte{[]byte(`{"corrections":[]}`)}}
	resp := ai.StructuredResponse{JSON: []byte(`{"corrections":[]}`)}

	got, err := aiutil.ValidateWithRepairAndRetry(context.Background(), gen, pingSchema, ai.StructuredRequest{}, resp)
	if err != nil {
		t.Fatalf("ValidateWithRepairAndRetry returned error: %v", err)
	}
	if string(got.JSON) != `{"corrections":[]}` {
		t.Fatalf("JSON = %s, want unchanged", got.JSON)
	}
	if gen.calls != 0 {
		t.Fatalf("gen.calls = %d, want 0 (already-valid response must not trigger a retry)", gen.calls)
	}
}

func TestValidateWithRepairAndRetryRepairsFencedJSONWithoutRetrying(t *testing.T) {
	gen := &flakyGen{payloads: [][]byte{[]byte(`{"corrections":[]}`)}}
	resp := ai.StructuredResponse{JSON: []byte("```json\n{\"corrections\":[]}\n```")}

	got, err := aiutil.ValidateWithRepairAndRetry(context.Background(), gen, pingSchema, ai.StructuredRequest{}, resp)
	if err != nil {
		t.Fatalf("ValidateWithRepairAndRetry returned error: %v", err)
	}
	if string(got.JSON) != `{"corrections":[]}` {
		t.Fatalf("JSON = %s, want the repaired object", got.JSON)
	}
	if gen.calls != 0 {
		t.Fatalf("gen.calls = %d, want 0 (constrained repair resolves fenced JSON without a retry call)", gen.calls)
	}
}

func TestValidateWithRepairAndRetryRetriesWhenRepairCannotExtractJSON(t *testing.T) {
	gen := &flakyGen{payloads: [][]byte{[]byte(`{"corrections":[]}`)}}
	resp := ai.StructuredResponse{JSON: []byte("I'm sorry, I can't help with that.")}

	got, err := aiutil.ValidateWithRepairAndRetry(context.Background(), gen, pingSchema, ai.StructuredRequest{}, resp)
	if err != nil {
		t.Fatalf("ValidateWithRepairAndRetry returned error: %v", err)
	}
	if string(got.JSON) != `{"corrections":[]}` {
		t.Fatalf("JSON = %s, want the retried response", got.JSON)
	}
	if gen.calls != 1 {
		t.Fatalf("gen.calls = %d, want 1 (repair impossible, must fall back to one retry)", gen.calls)
	}
}

func TestValidateWithRepairAndRetryFailsAfterRepairAndRetryExhausted(t *testing.T) {
	gen := &flakyGen{payloads: [][]byte{[]byte("still nonsense")}}
	resp := ai.StructuredResponse{JSON: []byte("nonsense, no braces here")}

	_, err := aiutil.ValidateWithRepairAndRetry(context.Background(), gen, pingSchema, ai.StructuredRequest{}, resp)
	if err == nil {
		t.Fatal("expected an error after repair and retry are exhausted, got nil")
	}
	if gen.calls != 1 {
		t.Fatalf("gen.calls = %d, want 1 (one retry attempted, then fail)", gen.calls)
	}
}
