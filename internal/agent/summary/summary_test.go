package summary_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double injected via summary.New(ai.StructuredGenerator); PRD §75 Rule 3 forbids agents reaching real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/agent/summary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

func testInput() summary.SummaryInput {
	return summary.SummaryInput{
		Identity:       "learner-a",
		StatsSummary:   []string{"Written 500 characters across 3 sessions so far."},
		Priorities:     []string{"i-adjective-past (concept weakness, score 2.5): recurring weakness: 5 occurrences in 30d"},
		NewExpressions: []string{"それはそれとして — 話題を切り替える際に使う表現"},
	}
}

// TestGenerateFakeAIHappyPath pins the Step 1 scenario: fakeai's one
// canned weekly_summary.v1 response comes back as a composed subject +
// plain-text body containing the brief's pinned canned content
// (面白かったです and それはそれとして).
func TestGenerateFakeAIHappyPath(t *testing.T) {
	agent := summary.New(fakeai.New())

	subject, body, resp, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if resp.Provider != "fake" {
		t.Fatalf("resp.Provider = %q, want fake", resp.Provider)
	}
	if subject == "" {
		t.Fatal("subject is empty")
	}
	if !strings.Contains(body, "面白かったです") {
		t.Fatalf("body missing 面白かったです: %s", body)
	}
	if !strings.Contains(body, "それはそれとして") {
		t.Fatalf("body missing それはそれとして: %s", body)
	}
	// The body is composed Go-side with fixed section headings (PRD
	// §21/§56) — this pins that composition actually ran, not just that
	// the raw JSON's content leaked through some other way.
	if !strings.Contains(body, "What you accomplished this week:") {
		t.Fatalf("body missing composed accomplishments heading: %s", body)
	}
	if !strings.Contains(body, "Recommended focus for next week:") {
		t.Fatalf("body missing composed recommended-focus heading: %s", body)
	}
}

// spyGen is a local ai.StructuredGenerator test double that records the
// last ai.StructuredRequest it was called with — mirrors
// internal/agent/lesson's own spyGen.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	return ai.StructuredResponse{
		JSON: []byte(`{"subject":"s","accomplishments":["a"],"improvements":["b"],` +
			`"persistent_weaknesses":["c"],"new_expressions":[],"recommended_focus":["d"]}`),
		Provider: "spy",
		Model:    "spy-1",
	}, nil
}

// TestGenerateRendersPromptWithContext pins the prompt wiring: every
// context slice is rendered into the user prompt, and the request is
// sent as summary.generate/v1/weekly_summary.v1.
func TestGenerateRendersPromptWithContext(t *testing.T) {
	gen := &spyGen{}
	agent := summary.New(gen)

	_, _, _, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	if gen.req.PromptName != "summary.generate" {
		t.Fatalf("PromptName = %q, want summary.generate", gen.req.PromptName)
	}
	if gen.req.PromptVersion != "v1" {
		t.Fatalf("PromptVersion = %q, want v1", gen.req.PromptVersion)
	}
	if gen.req.SchemaName != "weekly_summary.v1" {
		t.Fatalf("SchemaName = %q, want weekly_summary.v1", gen.req.SchemaName)
	}
	if gen.req.IdentityID != "learner-a" {
		t.Fatalf("IdentityID = %q, want learner-a", gen.req.IdentityID)
	}
	if !strings.Contains(gen.req.User, "Written 500 characters") {
		t.Fatalf("User prompt missing StatsSummary: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "i-adjective-past (concept weakness") {
		t.Fatalf("User prompt missing Priorities: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "それはそれとして") {
		t.Fatalf("User prompt missing NewExpressions: %s", gen.req.User)
	}
}

// TestGenerateEmptyContextRendersWithoutError pins that an identity with
// no stats/priorities/expressions yet (a brand-new identity) still
// renders a valid prompt — every section in the user template is
// guarded by {{if}}.
func TestGenerateEmptyContextRendersWithoutError(t *testing.T) {
	agent := summary.New(fakeai.New())
	_, _, _, err := agent.Generate(context.Background(), summary.SummaryInput{Identity: "learner-b"})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
}

// TestGenerateOmitsEmptySectionsAndOptionalChallenge pins composeBody's
// contract via its externally-visible effect: a validated response with
// an empty new_expressions array and no challenge produces a body with
// no "New expressions" heading and no challenge line at all — the
// section is skipped outright, not rendered as an empty header.
func TestGenerateOmitsEmptySectionsAndOptionalChallenge(t *testing.T) {
	gen := &spyGen{} // canned response above has new_expressions:[] and no challenge
	agent := summary.New(gen)

	_, body, _, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if strings.Contains(body, "New expressions") {
		t.Fatalf("body should omit the New expressions heading when new_expressions is empty: %s", body)
	}
	if strings.Contains(body, "challenge") {
		t.Fatalf("body should omit the challenge line when challenge is absent: %s", body)
	}
}

// flakyGen is a local ai.StructuredGenerator test double whose
// GenerateStructured returns a fixed sequence of raw payloads — mirrors
// internal/agent/lesson's own flakyGen, exercising Rule 4's bounded
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

const validSummaryJSON = `{"subject":"s","accomplishments":["a"],"improvements":["b"],` +
	`"persistent_weaknesses":["c"],"new_expressions":["d"],"recommended_focus":["e"],"challenge":"f"}`

// TestGenerateRepairsFencedJSON pins the constrained-repair path: a
// markdown-fenced JSON payload resolves without a retry call.
func TestGenerateRepairsFencedJSON(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("```json\n" + validSummaryJSON + "\n```"),
			[]byte(validSummaryJSON),
		},
	}
	agent := summary.New(gen)

	subject, body, _, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if subject != "s" {
		t.Fatalf("subject = %q, want s (from the repaired first response)", subject)
	}
	if !strings.Contains(body, "f") { // the challenge line
		t.Fatalf("body missing composed challenge: %s", body)
	}
	if gen.calls != 1 {
		t.Fatalf("gen.calls = %d, want 1 (constrained repair should resolve fenced JSON without a retry)", gen.calls)
	}
}

// TestGenerateFailsAfterRepairAndRetryExhausted: an always-invalid
// double exhausts both the repair attempt and the one retry call, and
// Generate must fail rather than loop or return zero-value strings.
func TestGenerateFailsAfterRepairAndRetryExhausted(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("nonsense, no braces here"),
			[]byte("still nonsense"),
		},
	}
	agent := summary.New(gen)

	_, _, _, err := agent.Generate(context.Background(), testInput())
	if err == nil {
		t.Fatal("expected an error after repair and retry are exhausted, got nil")
	}
	if gen.calls != 2 {
		t.Fatalf("gen.calls = %d, want 2 (initial call + one retry, then fail)", gen.calls)
	}
}
