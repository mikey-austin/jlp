package anki_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double injected via anki.New(ai.StructuredGenerator); PRD §75 Rule 3 forbids agents reaching real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/agent/anki"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

func testInput() anki.GenerateInput {
	return anki.GenerateInput{
		Identity:      "learner-a",
		SourceType:    "correction",
		SourceText:    "面白いでした",
		CorrectedText: "面白かったです",
		ExplanationJA: "い形容詞の過去形は「〜かった」を使います。",
		ExplanationEN: "い-adjectives form the past tense with 〜かった.",
	}
}

// TestGenerateFakeAIHappyPath pins the Step 1 scenario: fakeai's one
// canned anki_card.v1 response comes back mapped onto front/back/notes
// directly.
func TestGenerateFakeAIHappyPath(t *testing.T) {
	agent := anki.New(fakeai.New())

	front, back, notes, resp, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if front != "「とても面白いでした」— 何が不自然？" {
		t.Fatalf("front = %q, want the canned front", front)
	}
	if !strings.Contains(back, "「とても面白かったです」") {
		t.Fatalf("back = %q, want it to contain the canned corrected form", back)
	}
	if !strings.Contains(back, "理由") {
		t.Fatalf("back = %q, want it to contain a 理由 (reason) section", back)
	}
	if notes != "i-adjective-past" {
		t.Fatalf("notes = %q, want i-adjective-past", notes)
	}
	if resp.Provider != "fake" {
		t.Fatalf("resp.Provider = %q, want fake", resp.Provider)
	}
}

// spyGen is a local ai.StructuredGenerator test double that records the
// last ai.StructuredRequest it was called with, so a test can inspect
// the fully-rendered System/User prompt text — mirrors
// internal/agent/teacher's own spyGen.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	return ai.StructuredResponse{
		JSON:     []byte(`{"front":"f","back":"b"}`),
		Provider: "spy",
		Model:    "spy-1",
	}, nil
}

// TestGenerateRendersPromptWithSourceAndExplanations pins the prompt
// wiring: the user half carries the source/corrected text and both
// explanations, and the request is sent as anki.generate/v1/
// anki_card.v1.
func TestGenerateRendersPromptWithSourceAndExplanations(t *testing.T) {
	gen := &spyGen{}
	agent := anki.New(gen)

	_, _, _, _, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	if gen.req.PromptName != "anki.generate" {
		t.Fatalf("PromptName = %q, want anki.generate", gen.req.PromptName)
	}
	if gen.req.PromptVersion != "v1" {
		t.Fatalf("PromptVersion = %q, want v1", gen.req.PromptVersion)
	}
	if gen.req.SchemaName != "anki_card.v1" {
		t.Fatalf("SchemaName = %q, want anki_card.v1", gen.req.SchemaName)
	}
	if !strings.Contains(gen.req.User, "面白いでした") {
		t.Fatalf("User prompt missing SourceText: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "面白かったです") {
		t.Fatalf("User prompt missing CorrectedText: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "い形容詞の過去形は「〜かった」を使います。") {
		t.Fatalf("User prompt missing ExplanationJA: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "い-adjectives form the past tense with 〜かった.") {
		t.Fatalf("User prompt missing ExplanationEN: %s", gen.req.User)
	}
}

// flakyGen is a local ai.StructuredGenerator test double whose
// GenerateStructured returns a fixed sequence of raw payloads — mirrors
// internal/agent/teacher's own flakyGen, exercising Rule 4's bounded
// self-healing.
type flakyGen struct {
	calls    int
	payloads [][]byte
}

func (f *flakyGen) GenerateStructured(_ context.Context, _ ai.StructuredRequest) (ai.StructuredResponse, error) {
	i := f.calls
	if i >= len(f.payloads) {
		i = len(f.payloads) - 1
	}
	f.calls++
	return ai.StructuredResponse{
		JSON:     f.payloads[i],
		Provider: "flaky",
		Model:    "flaky-1",
	}, nil
}

// TestGenerateRepairsFencedJSON pins the constrained-repair path: a
// markdown-fenced JSON payload resolves without a retry call.
func TestGenerateRepairsFencedJSON(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("```json\n{\"front\":\"f\",\"back\":\"b\"}\n```"),
			[]byte(`{"front":"f2","back":"b2"}`),
		},
	}
	agent := anki.New(gen)

	front, back, _, _, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if front != "f" || back != "b" {
		t.Fatalf("front/back = %q/%q, want f/b (repaired from the first response)", front, back)
	}
	if gen.calls != 1 {
		t.Fatalf("gen.calls = %d, want 1 (constrained repair should resolve fenced JSON without a retry)", gen.calls)
	}
}

// TestGenerateFailsAfterRepairAndRetryExhausted: an always-invalid
// double exhausts both the repair attempt and the one retry call, and
// Generate must fail rather than loop or return a zero-value card.
func TestGenerateFailsAfterRepairAndRetryExhausted(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("nonsense, no braces here"),
			[]byte("still nonsense"),
		},
	}
	agent := anki.New(gen)

	_, _, _, _, err := agent.Generate(context.Background(), testInput())
	if err == nil {
		t.Fatal("expected an error after repair and retry are exhausted, got nil")
	}
	if gen.calls != 2 {
		t.Fatalf("gen.calls = %d, want 2 (initial call + one retry, then fail)", gen.calls)
	}
}
