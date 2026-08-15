package lesson_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double injected via lesson.New(ai.StructuredGenerator); PRD §75 Rule 3 forbids agents reaching real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/agent/lesson"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

func testInput() lesson.GenerateInput {
	return lesson.GenerateInput{
		Identity:              "learner-a",
		Priorities:            []string{"i-adjective-past (concept weakness, score 2.5): recurring weakness: 5 occurrences in 30d"},
		ActivationExpressions: []string{"それはそれとして — 話題を切り替える際に使う表現"},
		RecentCorrections:     []string{"面白いでした → 面白かったです (conjugation)"},
		ObservationSummaries:  []string{"i-adjective-past (weakness, concept): confidence 0.8"},
	}
}

// TestGenerateFakeAIHappyPath pins the Step 1 scenario: fakeai's one
// canned lesson_plan.v1 response comes back verbatim as planJSON, and
// it validates against the schema.
func TestGenerateFakeAIHappyPath(t *testing.T) {
	agent := lesson.New(fakeai.New())

	planJSON, resp, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if resp.Provider != "fake" {
		t.Fatalf("resp.Provider = %q, want fake", resp.Provider)
	}

	var plan struct {
		LevelSummary        string   `json:"level_summary"`
		Strengths           []string `json:"strengths"`
		Weaknesses          []string `json:"weaknesses"`
		Focus               []string `json:"focus"`
		Vocabulary          []string `json:"vocabulary"`
		GrammarConcepts     []string `json:"grammar_concepts"`
		ConversationPrompts []string `json:"conversation_prompts"`
		Exercises           []string `json:"exercises"`
		RecentExamples      []string `json:"recent_examples"`
		QuestionsForTutor   []string `json:"questions_for_tutor"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		t.Fatalf("planJSON did not unmarshal: %v (%s)", err, planJSON)
	}
	if plan.LevelSummary == "" {
		t.Fatal("plan.LevelSummary is empty")
	}
	joined := strings.Join(plan.Weaknesses, " ") + strings.Join(plan.GrammarConcepts, " ")
	if !strings.Contains(joined, "i-adjective-past") {
		t.Fatalf("plan weaknesses/grammar_concepts missing i-adjective-past: %+v", plan)
	}
	vocabJoined := strings.Join(plan.Vocabulary, " ")
	if !strings.Contains(vocabJoined, "それはそれとして") {
		t.Fatalf("plan.Vocabulary missing それはそれとして: %+v", plan.Vocabulary)
	}
}

// spyGen is a local ai.StructuredGenerator test double that records the
// last ai.StructuredRequest it was called with — mirrors
// internal/agent/anki's own spyGen.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	return ai.StructuredResponse{
		JSON: []byte(`{"level_summary":"s","strengths":["a"],"weaknesses":["b"],"focus":["c"],` +
			`"vocabulary":["d"],"grammar_concepts":["e"],"conversation_prompts":["f"],` +
			`"exercises":["g"],"recent_examples":["h"],"questions_for_tutor":["i"]}`),
		Provider: "spy",
		Model:    "spy-1",
	}, nil
}

// TestGenerateRendersPromptWithContext pins the prompt wiring: every
// context slice is rendered into the user prompt, and the request is
// sent as lesson.generate/v1/lesson_plan.v1.
func TestGenerateRendersPromptWithContext(t *testing.T) {
	gen := &spyGen{}
	agent := lesson.New(gen)

	_, _, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	if gen.req.PromptName != "lesson.generate" {
		t.Fatalf("PromptName = %q, want lesson.generate", gen.req.PromptName)
	}
	if gen.req.PromptVersion != "v1" {
		t.Fatalf("PromptVersion = %q, want v1", gen.req.PromptVersion)
	}
	if gen.req.SchemaName != "lesson_plan.v1" {
		t.Fatalf("SchemaName = %q, want lesson_plan.v1", gen.req.SchemaName)
	}
	if gen.req.IdentityID != "learner-a" {
		t.Fatalf("IdentityID = %q, want learner-a", gen.req.IdentityID)
	}
	if !strings.Contains(gen.req.User, "i-adjective-past (concept weakness") {
		t.Fatalf("User prompt missing Priorities: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "それはそれとして") {
		t.Fatalf("User prompt missing ActivationExpressions: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "面白いでした → 面白かったです") {
		t.Fatalf("User prompt missing RecentCorrections: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "confidence 0.8") {
		t.Fatalf("User prompt missing ObservationSummaries: %s", gen.req.User)
	}
}

// TestGenerateEmptyContextRendersWithoutError pins that an identity with
// no priorities/expressions/corrections/observations yet (a brand-new
// identity) still renders a valid prompt — every section in the user
// template is guarded by {{if}}.
func TestGenerateEmptyContextRendersWithoutError(t *testing.T) {
	agent := lesson.New(fakeai.New())
	_, _, err := agent.Generate(context.Background(), lesson.GenerateInput{Identity: "learner-b"})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
}

// flakyGen is a local ai.StructuredGenerator test double whose
// GenerateStructured returns a fixed sequence of raw payloads — mirrors
// internal/agent/anki's own flakyGen, exercising Rule 4's bounded
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

const validPlanJSON = `{"level_summary":"s","strengths":["a"],"weaknesses":["b"],"focus":["c"],` +
	`"vocabulary":["d"],"grammar_concepts":["e"],"conversation_prompts":["f"],` +
	`"exercises":["g"],"recent_examples":["h"],"questions_for_tutor":["i"]}`

// TestGenerateRepairsFencedJSON pins the constrained-repair path: a
// markdown-fenced JSON payload resolves without a retry call.
func TestGenerateRepairsFencedJSON(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("```json\n" + validPlanJSON + "\n```"),
			[]byte(validPlanJSON),
		},
	}
	agent := lesson.New(gen)

	planJSON, _, err := agent.Generate(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if string(planJSON) != validPlanJSON {
		t.Fatalf("planJSON = %s, want the repaired first response", planJSON)
	}
	if gen.calls != 1 {
		t.Fatalf("gen.calls = %d, want 1 (constrained repair should resolve fenced JSON without a retry)", gen.calls)
	}
}

// TestGenerateFailsAfterRepairAndRetryExhausted: an always-invalid
// double exhausts both the repair attempt and the one retry call, and
// Generate must fail rather than loop or return a zero-value plan.
func TestGenerateFailsAfterRepairAndRetryExhausted(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("nonsense, no braces here"),
			[]byte("still nonsense"),
		},
	}
	agent := lesson.New(gen)

	_, _, err := agent.Generate(context.Background(), testInput())
	if err == nil {
		t.Fatal("expected an error after repair and retry are exhausted, got nil")
	}
	if gen.calls != 2 {
		t.Fatalf("gen.calls = %d, want 2 (initial call + one retry, then fail)", gen.calls)
	}
}
