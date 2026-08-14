package teacher_test

import (
	"context"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double injected via teacher.New(ai.StructuredGenerator); PRD §75 Rule 3 forbids agents reaching real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

func testSession() session.Session {
	return session.Session{
		ID:      "sess-1",
		Purpose: "diary",
		Profile: session.Profile{
			TeacherMode:         "teacher",
			Strictness:          "balanced",
			ExplanationLanguage: "both",
			Register:            "polite",
		},
	}
}

func TestReviewWritingFakeAIHappyPath(t *testing.T) {
	agent := teacher.New(fakeai.New())

	in := teacher.ReviewInput{
		Identity:  "learner-a",
		Session:   testSession(),
		Selection: "とても面白いでした",
		Context:   "とても面白いでした",
	}
	result, resp, err := agent.ReviewWriting(context.Background(), in)
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}

	if len(result.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(result.Corrections), result.Corrections)
	}
	c := result.Corrections[0]
	if c.Replacement != "面白かったです" {
		t.Fatalf("Replacement = %q, want 面白かったです", c.Replacement)
	}
	if c.Original != "面白いでした" {
		t.Fatalf("Original = %q, want 面白いでした", c.Original)
	}
	if c.ID == "" {
		t.Fatal("Correction ID was not assigned")
	}
	if result.Corrected != "とても面白かったです" {
		t.Fatalf("Corrected = %q, want とても面白かったです", result.Corrected)
	}
	if result.Original != in.Selection {
		t.Fatalf("Original = %q, want %q", result.Original, in.Selection)
	}
	if resp.Provider != "fake" {
		t.Fatalf("resp.Provider = %q, want fake", resp.Provider)
	}
}

func TestReviewWritingNaturalSentenceYieldsNoCorrections(t *testing.T) {
	agent := teacher.New(fakeai.New())

	in := teacher.ReviewInput{
		Identity:  "learner-a",
		Session:   testSession(),
		Selection: "今日は晴れです。",
		Context:   "今日は晴れです。",
	}
	result, _, err := agent.ReviewWriting(context.Background(), in)
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}
	if len(result.Corrections) != 0 {
		t.Fatalf("len(Corrections) = %d, want 0: %+v", len(result.Corrections), result.Corrections)
	}
	if result.Corrected != in.Selection {
		t.Fatalf("Corrected = %q, want unchanged %q", result.Corrected, in.Selection)
	}
}

// flakyGen is a local ai.StructuredGenerator test double whose
// GenerateStructured returns a fixed sequence of raw payloads: the Nth
// call returns payloads[N] (clamped to the last entry once exhausted).
// It exists to exercise Rule 4 (schema validation with bounded
// self-healing) without depending on a real provider's flakiness.
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

func testReviewInput() teacher.ReviewInput {
	return teacher.ReviewInput{
		Identity:  "learner-a",
		Session:   testSession(),
		Selection: "とても面白いでした",
		Context:   "とても面白いでした",
	}
}

// TestReviewWritingRepairsFencedJSON pins the Step 1 scenario from the
// task brief: the first call returns markdown-fenced JSON garbage (a
// common way models wrap valid JSON in prose). The constrained-repair
// step (extract the first balanced {...} block, re-validate) resolves
// it without needing the second canned response at all.
func TestReviewWritingRepairsFencedJSON(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("```json\n{\"corrections\":[]}\n```"),
			[]byte(`{"corrections":[]}`),
		},
	}
	agent := teacher.New(gen)

	result, _, err := agent.ReviewWriting(context.Background(), testReviewInput())
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}
	if len(result.Corrections) != 0 {
		t.Fatalf("len(Corrections) = %d, want 0", len(result.Corrections))
	}
	if gen.calls != 1 {
		t.Fatalf("gen.calls = %d, want 1 (constrained repair should resolve fenced JSON without a retry call)", gen.calls)
	}
}

// TestReviewWritingRetriesWhenRepairCannotExtractJSON covers the other
// half of Rule 4: when the first response has no balanced {...} block
// at all, constrained repair can't help, so the agent must fall back to
// exactly one full retry call.
func TestReviewWritingRetriesWhenRepairCannotExtractJSON(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("I'm sorry, I can't help with that."),
			[]byte(`{"corrections":[]}`),
		},
	}
	agent := teacher.New(gen)

	result, _, err := agent.ReviewWriting(context.Background(), testReviewInput())
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}
	if len(result.Corrections) != 0 {
		t.Fatalf("len(Corrections) = %d, want 0", len(result.Corrections))
	}
	if gen.calls != 2 {
		t.Fatalf("gen.calls = %d, want 2 (repair impossible, must fall back to one full retry)", gen.calls)
	}
}

// TestReviewWritingFailsAfterRepairAndRetryExhausted: an always-invalid
// double exhausts both the repair attempt and the one retry call, and
// ReviewWriting must fail rather than loop or silently return zero
// corrections.
func TestReviewWritingFailsAfterRepairAndRetryExhausted(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("nonsense, no braces here"),
			[]byte("still nonsense"),
		},
	}
	agent := teacher.New(gen)

	_, _, err := agent.ReviewWriting(context.Background(), testReviewInput())
	if err == nil {
		t.Fatal("expected an error after repair and retry are exhausted, got nil")
	}
	if gen.calls != 2 {
		t.Fatalf("gen.calls = %d, want 2 (initial call + one retry, then fail)", gen.calls)
	}
}
