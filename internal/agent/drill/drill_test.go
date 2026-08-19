package drill_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double injected via drill.New(ai.StructuredGenerator); PRD §75 Rule 3 forbids agents reaching real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/agent/drill"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

func testConcept() grammar.Concept {
	return grammar.Concept{
		Slug:        "i-adjective-past",
		Name:        "い-adjective past tense",
		JLPTLevel:   5,
		Description: "The past tense of い-adjectives is formed by dropping い and adding かった.",
		Examples:    []string{"面白かったです。"},
	}
}

// TestGenerateFakeAIHappyPath pins the Step 1 scenario from the task
// brief: Generate returns the canned MCQ, schema-valid, mapped onto an
// exercise.Exercise.
func TestGenerateFakeAIHappyPath(t *testing.T) {
	agent := drill.New(fakeai.New())

	ex, resp, err := agent.Generate(context.Background(), drill.GenerateInput{
		Identity: "learner-a",
		Concept:  testConcept(),
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if ex.Type != exercise.TypeMultipleChoice {
		t.Fatalf("Type = %q, want %q", ex.Type, exercise.TypeMultipleChoice)
	}
	if ex.ConceptSlug != "i-adjective-past" {
		t.Fatalf("ConceptSlug = %q, want i-adjective-past", ex.ConceptSlug)
	}
	if ex.Prompt != "昨日の映画はとても＿＿＿。" {
		t.Fatalf("Prompt = %q, want the canned prompt", ex.Prompt)
	}
	if len(ex.Choices) != 3 {
		t.Fatalf("len(Choices) = %d, want 3: %+v", len(ex.Choices), ex.Choices)
	}
	if ex.Answer != "面白かったです" {
		t.Fatalf("Answer = %q, want 面白かったです", ex.Answer)
	}
	if ex.IdentityID != "learner-a" {
		t.Fatalf("IdentityID = %q, want learner-a", ex.IdentityID)
	}
	if ex.ID != "" {
		t.Fatalf("ID = %q, want empty (persistence assigns it, not the agent)", ex.ID)
	}
	if !ex.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt = %v, want zero (persistence assigns it, not the agent)", ex.CreatedAt)
	}
	if resp.Provider != "fake" {
		t.Fatalf("resp.Provider = %q, want fake", resp.Provider)
	}
}

// TestEvaluateFakeAIHappyPath pins Evaluate returning the canned eval.
func TestEvaluateFakeAIHappyPath(t *testing.T) {
	agent := drill.New(fakeai.New())

	ex := exercise.Exercise{
		Type:        exercise.TypeFreeProduction,
		ConceptSlug: "i-adjective-past",
		Prompt:      "昨日見た映画について書いてください。",
	}
	eval, resp, err := agent.Evaluate(context.Background(), "learner-a", ex, "映画はとても面白かったです。")
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !eval.Correct {
		t.Fatalf("Correct = false, want true")
	}
	if eval.Score != 85 {
		t.Fatalf("Score = %d, want 85", eval.Score)
	}
	if eval.FeedbackJA == "" || eval.FeedbackEN == "" {
		t.Fatalf("Feedback = %+v, want both JA and EN populated", eval)
	}
	if resp.Provider != "fake" {
		t.Fatalf("resp.Provider = %q, want fake", resp.Provider)
	}
}

// spyGen is a local ai.StructuredGenerator test double that records the
// last ai.StructuredRequest it was called with (so a test can inspect
// the fully-rendered System/User prompt text) and always returns a
// fixed, schema-valid response for whichever SchemaName it's asked for —
// mirroring internal/agent/teacher/teacher_test.go's own spyGen.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	switch req.SchemaName {
	case "exercise_eval.v1":
		return ai.StructuredResponse{
			JSON:     []byte(`{"correct":true,"score":90,"feedback":{"ja":"よくできました。","en":"Well done."}}`),
			Provider: "spy",
			Model:    "spy-1",
		}, nil
	default:
		return ai.StructuredResponse{
			JSON:     []byte(`{"type":"free-production","instructions":{"ja":"a","en":"b"},"prompt":"p","concept":"i-adjective-past"}`),
			Provider: "spy",
			Model:    "spy-1",
		}, nil
	}
}

// TestGenerateRendersConceptAndRecentErrors pins the prompt-rendering
// contract: Generate's GenerateInput.Concept and RecentErrors both flow
// through to the rendered drill.generate.v1 user prompt, and the request
// carries the exercise.v1 schema/prompt name/version.
func TestGenerateRendersConceptAndRecentErrors(t *testing.T) {
	gen := &spyGen{}
	agent := drill.New(gen)

	_, _, err := agent.Generate(context.Background(), drill.GenerateInput{
		Identity:     "learner-a",
		Concept:      testConcept(),
		RecentErrors: []string{"i-adjective-past (concept weakness, score 7.5): recurring weakness"},
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	if gen.req.PromptName != "drill.generate" {
		t.Fatalf("PromptName = %q, want drill.generate", gen.req.PromptName)
	}
	if gen.req.PromptVersion != "v1" {
		t.Fatalf("PromptVersion = %q, want v1", gen.req.PromptVersion)
	}
	if gen.req.SchemaName != "exercise.v1" {
		t.Fatalf("SchemaName = %q, want exercise.v1", gen.req.SchemaName)
	}
	if !strings.Contains(gen.req.User, "i-adjective-past") {
		t.Fatalf("User prompt missing the concept slug: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "い-adjective past tense") {
		t.Fatalf("User prompt missing the concept name: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "recurring weakness") {
		t.Fatalf("User prompt missing the recent-errors line: %s", gen.req.User)
	}
}

// TestEvaluateRendersPromptAndResponse pins Evaluate's own prompt
// rendering: the exercise's Prompt and the learner's response both flow
// through, and the request carries the exercise_eval.v1 schema.
func TestEvaluateRendersPromptAndResponse(t *testing.T) {
	gen := &spyGen{}
	agent := drill.New(gen)

	ex := exercise.Exercise{
		Type:        exercise.TypeFreeProduction,
		ConceptSlug: "i-adjective-past",
		Prompt:      "昨日見た映画について書いてください。",
	}
	_, _, err := agent.Evaluate(context.Background(), "learner-a", ex, "とても面白かったです。")
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}

	if gen.req.PromptName != "drill.evaluate" {
		t.Fatalf("PromptName = %q, want drill.evaluate", gen.req.PromptName)
	}
	if gen.req.SchemaName != "exercise_eval.v1" {
		t.Fatalf("SchemaName = %q, want exercise_eval.v1", gen.req.SchemaName)
	}
	if !strings.Contains(gen.req.User, "昨日見た映画について書いてください。") {
		t.Fatalf("User prompt missing the exercise prompt: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "とても面白かったです。") {
		t.Fatalf("User prompt missing the learner's response: %s", gen.req.User)
	}
}

// flakyGen is a local ai.StructuredGenerator test double whose
// GenerateStructured returns a fixed sequence of raw payloads: the Nth
// call returns payloads[N] (clamped to the last entry once exhausted) —
// mirroring internal/agent/teacher/teacher_test.go's own flakyGen, now
// exercising the SAME aiutil.ValidateWithRepairAndRetry path through the
// drill agent instead.
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
	return ai.StructuredResponse{JSON: f.payloads[i], Provider: "flaky", Model: "flaky-1"}, nil
}

// TestGenerateRepairsFencedJSON pins the Step 1 scenario from the task
// brief: a malformed-then-valid stub exercises the repair path reused
// from the teacher agent (now internal/agent/aiutil).
func TestGenerateRepairsFencedJSON(t *testing.T) {
	valid := `{"type":"multiple-choice","instructions":{"ja":"a","en":"b"},"prompt":"p","choices":["x","y"],"answer":"x","concept":"i-adjective-past"}`
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("```json\n" + valid + "\n```"),
			[]byte(valid),
		},
	}
	agent := drill.New(gen)

	ex, _, err := agent.Generate(context.Background(), drill.GenerateInput{Identity: "learner-a", Concept: testConcept()})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if ex.ConceptSlug != "i-adjective-past" {
		t.Fatalf("ConceptSlug = %q, want i-adjective-past", ex.ConceptSlug)
	}
	if gen.calls != 1 {
		t.Fatalf("gen.calls = %d, want 1 (constrained repair should resolve fenced JSON without a retry call)", gen.calls)
	}
}

// TestGenerateFailsAfterRepairAndRetryExhausted: an always-invalid
// double exhausts both the repair attempt and the one retry call, and
// Generate must fail rather than loop or silently return a zero-value
// exercise.
func TestGenerateFailsAfterRepairAndRetryExhausted(t *testing.T) {
	gen := &flakyGen{
		payloads: [][]byte{
			[]byte("nonsense, no braces here"),
			[]byte("still nonsense"),
		},
	}
	agent := drill.New(gen)

	_, _, err := agent.Generate(context.Background(), drill.GenerateInput{Identity: "learner-a", Concept: testConcept()})
	if err == nil {
		t.Fatal("expected an error after repair and retry are exhausted, got nil")
	}
	if gen.calls != 2 {
		t.Fatalf("gen.calls = %d, want 2 (initial call + one retry, then fail)", gen.calls)
	}
}

// The answer must be one of the choices, or the learner cannot pick it
// and the deterministic grader can never return correct. The schema
// cannot express that relationship, so the agent checks it — and a
// silent pass here would ship an unanswerable question.
func TestGeneratePassageRejectsAnAnswerThatIsNotAChoice(t *testing.T) {
	// Schema-valid, and still unanswerable: the relationship between
	// answer and choices is not something JSON Schema can express.
	gen := &flakyGen{payloads: [][]byte{[]byte(`{
	  "passage": "きのうは忙しかったです。",
	  "question": {"ja": "どうでしたか。", "en": "How was it?"},
	  "choices": ["ひまだった", "楽しかった", "疲れた"],
	  "answer": "存在しない選択肢"
	}`)}}

	_, _, err := drill.New(gen).GeneratePassage(context.Background(), drill.PassageInput{
		Identity: "learner-a",
		Words:    []drill.WordRef{{Expression: "忙しい"}},
	})
	if err == nil {
		t.Fatal("accepted a passage whose answer is not among its choices — unanswerable by construction")
	}
	if !strings.Contains(err.Error(), "not among its choices") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}

// A passage with no words is a caller bug, and writing prose about
// nothing is not a useful fallback.
func TestGeneratePassageRefusesAnEmptyWordList(t *testing.T) {
	_, _, err := drill.New(fakeai.New()).GeneratePassage(context.Background(), drill.PassageInput{Identity: "learner-a"})
	if err == nil {
		t.Fatal("accepted a passage request with no words")
	}
}

// The passage becomes a multiple-choice exercise: the paragraph is the
// prompt, the question is the instruction, and the choices are graded by
// the same deterministic comparison every other choice question uses.
func TestGeneratePassageShapesAChoiceExercise(t *testing.T) {
	ex, _, err := drill.New(fakeai.New()).GeneratePassage(context.Background(), drill.PassageInput{
		Identity: "learner-a",
		Words:    []drill.WordRef{{Expression: "映画"}, {Expression: "友達"}},
	})
	if err != nil {
		t.Fatalf("GeneratePassage: %v", err)
	}
	if ex.Type != exercise.TypePassageChoice {
		t.Errorf("Type = %q, want %q", ex.Type, exercise.TypePassageChoice)
	}
	if ex.Prompt == "" {
		t.Error("no passage")
	}
	if ex.InstructionsJA == "" {
		t.Error("no question")
	}
	if len(ex.Choices) < 3 {
		t.Errorf("choices = %v, want at least three", ex.Choices)
	}
	if !slices.Contains(ex.Choices, ex.Answer) {
		t.Errorf("answer %q is not among choices %v", ex.Answer, ex.Choices)
	}
}

// Models routinely answer "B" or "2" for a list they were never given
// labels for, because that is how multiple choice looks in their
// training data. Refusing those throws away a passage where the model
// picked the RIGHT option and merely named it the way it had seen
// options named — observed on a local model, which answered "B".
func TestGeneratePassageResolvesALabelledAnswer(t *testing.T) {
	for _, tc := range []struct{ name, answer, want string }{
		{"letter", `"B"`, "楽しかった"},
		{"lowercase letter", `"c"`, "疲れた"},
		{"index", `"1"`, "ひまだった"},
		{"exact text", `"疲れた"`, "疲れた"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen := &flakyGen{payloads: [][]byte{[]byte(`{
			  "passage": "きのうは忙しかったです。",
			  "question": {"ja": "どうでしたか。", "en": "How was it?"},
			  "choices": ["ひまだった", "楽しかった", "疲れた"],
			  "answer": ` + tc.answer + `
			}`)}}

			ex, _, err := drill.New(gen).GeneratePassage(context.Background(), drill.PassageInput{
				Identity: "learner-a",
				Words:    []drill.WordRef{{Expression: "忙しい"}},
			})
			if err != nil {
				t.Fatalf("GeneratePassage: %v", err)
			}
			if ex.Answer != tc.want {
				t.Errorf("answer = %q, want %q", ex.Answer, tc.want)
			}
			if !slices.Contains(ex.Choices, ex.Answer) {
				t.Errorf("resolved answer %q is still not among choices %v", ex.Answer, ex.Choices)
			}
		})
	}
}

// A label out of range is a genuine mismatch, not something to guess at:
// grading against the wrong option is worse than refusing the passage.
func TestGeneratePassageStillRejectsAnUnresolvableAnswer(t *testing.T) {
	for _, answer := range []string{`"Z"`, `"9"`, `"まったく別の答え"`} {
		gen := &flakyGen{payloads: [][]byte{[]byte(`{
		  "passage": "きのうは忙しかったです。",
		  "question": {"ja": "どうでしたか。", "en": "How was it?"},
		  "choices": ["ひまだった", "楽しかった", "疲れた"],
		  "answer": ` + answer + `
		}`)}}
		if _, _, err := drill.New(gen).GeneratePassage(context.Background(), drill.PassageInput{
			Identity: "learner-a",
			Words:    []drill.WordRef{{Expression: "忙しい"}},
		}); err == nil {
			t.Errorf("accepted unresolvable answer %s", answer)
		}
	}
}
