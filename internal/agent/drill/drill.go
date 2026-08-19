// Package drill implements the Drill agent (PRD §17.2, §58): generating
// one targeted practice exercise for a grammar concept, and evaluating a
// learner's free-production response to one. Like internal/agent/teacher,
// it is a pure reviewer/generator — internal/application/practice owns
// authorization, persistence, and events.
//
// Rule 3: this package imports ports/ai + prompts + schemas + domain
// ONLY — no storage, no adapters, no application. It returns
// domain/exercise types rather than anything from application/practice
// specifically to avoid an import cycle — see that package's doc
// comment for the full explanation.
package drill

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	generatePromptName = "drill.generate"
	evaluatePromptName = "drill.evaluate"
	passagePromptName  = "drill.passage"
	promptVersion      = "v1"
	exerciseSchemaName = "exercise.v1"
	evalSchemaName     = "exercise_eval.v1"
	passageSchemaName  = "passage.v1"
	agentName          = "drill"
	maxTokens          = 1024
)

// Agent generates drill exercises and evaluates free-production
// responses to them by asking gen for schema-validated JSON. It holds
// no state beyond the generator: every call is a single, independent
// request.
type Agent struct {
	gen ai.StructuredGenerator
}

// New returns a Drill agent backed by gen (fakeai for tests/offline dev,
// the Anthropic adapter in production, always through the observability
// decorator).
func New(gen ai.StructuredGenerator) *Agent {
	return &Agent{gen: gen}
}

// GenerateInput is everything Generate needs to render the
// drill.generate prompt and scope the AI request.
type GenerateInput struct {
	Identity learner.IdentityID
	Concept  grammar.Concept
	// RecentErrors are human-readable weakness summaries (see
	// application/planner.Planner and application/feedback.Service's own
	// use of the same shape) the drill.generate.v1 prompt weaves in when
	// they fit the concept. May be empty.
	RecentErrors []string
	// ProviderOverride, when non-empty, is threaded straight through to
	// ai.StructuredRequest.ProviderOverride, exactly as
	// teacher.FeedbackInput does: the 練習 page's adapter dropdown picks
	// which adapter generates THIS drill, for this request only. An
	// explicit override does not fall back, so a failure with one is the
	// answer rather than a silent hop to another provider.
	ProviderOverride string
}

// generatePromptData mirrors exactly what
// templates/drill.generate.v1.*.md range/index over.
type generatePromptData struct {
	ConceptSlug        string
	ConceptName        string
	ConceptDescription string
	ConceptExamples    []string
	RecentErrors       []string
}

// evaluatePromptData mirrors exactly what
// templates/drill.evaluate.v1.*.md range/index over.
type evaluatePromptData struct {
	ConceptSlug string
	Prompt      string
	Response    string
}

// instructionsDTO mirrors schemas/defs/exercise.v1.json's "instructions"
// object and exercise_eval.v1.json's "feedback" object — both are the
// same {ja, en} shape.
type instructionsDTO struct {
	JA string `json:"ja"`
	EN string `json:"en"`
}

// exerciseDTO mirrors schemas/defs/exercise.v1.json field-for-field, so
// unmarshaling a schema-valid response can never silently drop or
// misname a field.
type exerciseDTO struct {
	Type         string          `json:"type"`
	Instructions instructionsDTO `json:"instructions"`
	Prompt       string          `json:"prompt"`
	Choices      []string        `json:"choices,omitempty"`
	Answer       string          `json:"answer,omitempty"`
	Acceptable   []string        `json:"acceptable,omitempty"`
	Concept      string          `json:"concept"`
}

// evalDTO mirrors schemas/defs/exercise_eval.v1.json field-for-field.
type evalDTO struct {
	Correct  bool            `json:"correct"`
	Score    int             `json:"score"`
	Feedback instructionsDTO `json:"feedback"`
}

// Generate renders the drill.generate prompt for in, asks gen for an
// exercise.v1-shaped response (Rule 4: schema validation with bounded
// repair/retry via aiutil.ValidateWithRepairAndRetry), then maps the
// result onto an exercise.Exercise. The returned Exercise has ID and
// CreatedAt left zero, and IdentityID set from in.Identity — persistence
// (assigning ID/CreatedAt, writing the row) is
// application/practice.Service.Start's job, not this agent's.
func (a *Agent) Generate(ctx context.Context, in GenerateInput) (exercise.Exercise, ai.StructuredResponse, error) {
	data := generatePromptData{
		ConceptSlug:        in.Concept.Slug,
		ConceptName:        in.Concept.Name,
		ConceptDescription: in.Concept.Description,
		ConceptExamples:    in.Concept.Examples,
		RecentErrors:       in.RecentErrors,
	}
	rendered, err := prompts.Render(generatePromptName, promptVersion, data)
	if err != nil {
		return exercise.Exercise{}, ai.StructuredResponse{}, fmt.Errorf("drill: render generate prompt: %w", err)
	}

	schema, err := schemas.Get(exerciseSchemaName)
	if err != nil {
		return exercise.Exercise{}, ai.StructuredResponse{}, fmt.Errorf("drill: get schema: %w", err)
	}

	req := ai.StructuredRequest{
		PromptName:       generatePromptName,
		PromptVersion:    promptVersion,
		System:           rendered.System,
		User:             rendered.User,
		SchemaName:       exerciseSchemaName,
		Schema:           schema,
		MaxTokens:        maxTokens,
		IdentityID:       in.Identity,
		Agent:            agentName,
		ProviderOverride: in.ProviderOverride,
	}

	resp, err := a.gen.GenerateStructured(ctx, req)
	if err != nil {
		return exercise.Exercise{}, resp, fmt.Errorf("drill: generate: %w", err)
	}

	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, exerciseSchemaName, req, resp)
	if err != nil {
		return exercise.Exercise{}, resp, err
	}

	var dto exerciseDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return exercise.Exercise{}, resp, fmt.Errorf("drill: unmarshal exercise: %w", err)
	}

	// ConceptSlug comes from in.Concept.Slug — what the caller actually
	// asked to be drilled — not dto.Concept. dto.Concept is the model's
	// own echo of the schema's required "concept" field, useful for
	// observability/consistency checking, but Generate was given exactly
	// one concept to write for (unlike the teacher agent's multi-candidate
	// tagging), so the caller's own input is the authoritative source of
	// truth, not a self-reported field a model could omit-drift or
	// hallucinate.
	ex := exercise.Exercise{
		IdentityID:     in.Identity,
		ConceptSlug:    in.Concept.Slug,
		Type:           dto.Type,
		InstructionsJA: dto.Instructions.JA,
		InstructionsEN: dto.Instructions.EN,
		Prompt:         dto.Prompt,
		Choices:        dto.Choices,
		Answer:         dto.Answer,
		Acceptable:     dto.Acceptable,
	}
	return ex, resp, nil
}

// Evaluate renders the drill.evaluate prompt for ex/response, asks gen
// for an exercise_eval.v1-shaped response, and maps the result onto an
// exercise.Evaluation. Only meaningful for ex.Type ==
// exercise.TypeFreeProduction — application/practice.Service.Answer is
// what decides whether to call this at all versus scoring
// deterministically; this method itself does not check ex.Type.
func (a *Agent) Evaluate(ctx context.Context, identity learner.IdentityID, ex exercise.Exercise, response string) (exercise.Evaluation, ai.StructuredResponse, error) {
	data := evaluatePromptData{
		ConceptSlug: ex.ConceptSlug,
		Prompt:      ex.Prompt,
		Response:    response,
	}
	rendered, err := prompts.Render(evaluatePromptName, promptVersion, data)
	if err != nil {
		return exercise.Evaluation{}, ai.StructuredResponse{}, fmt.Errorf("drill: render evaluate prompt: %w", err)
	}

	schema, err := schemas.Get(evalSchemaName)
	if err != nil {
		return exercise.Evaluation{}, ai.StructuredResponse{}, fmt.Errorf("drill: get schema: %w", err)
	}

	req := ai.StructuredRequest{
		PromptName:    evaluatePromptName,
		PromptVersion: promptVersion,
		System:        rendered.System,
		User:          rendered.User,
		SchemaName:    evalSchemaName,
		Schema:        schema,
		MaxTokens:     maxTokens,
		IdentityID:    identity,
		Agent:         agentName,
	}

	resp, err := a.gen.GenerateStructured(ctx, req)
	if err != nil {
		return exercise.Evaluation{}, resp, fmt.Errorf("drill: evaluate: %w", err)
	}

	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, evalSchemaName, req, resp)
	if err != nil {
		return exercise.Evaluation{}, resp, err
	}

	var dto evalDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return exercise.Evaluation{}, resp, fmt.Errorf("drill: unmarshal evaluation: %w", err)
	}

	return exercise.Evaluation{
		Correct:    dto.Correct,
		Score:      dto.Score,
		FeedbackJA: dto.Feedback.JA,
		FeedbackEN: dto.Feedback.EN,
	}, resp, nil
}

// WordRef is one vocabulary item a passage must use — the shape
// GeneratePassage needs, with no dependency on the vocabulary domain
// (Rule 3: an agent imports ports/ai, prompts, schemas and domain only,
// and application/practice is what holds the real items).
type WordRef struct {
	Expression string
	Reading    string
	Meaning    string
}

// PassageInput asks for a reading passage that revisits several words.
type PassageInput struct {
	Identity learner.IdentityID
	// Words are the items the passage must use. All of them: a passage
	// that quietly drops one is a revision slot the learner did not get.
	Words            []WordRef
	ProviderOverride string
}

// passageDTO mirrors schemas/defs/passage.v1.json exactly.
type passageDTO struct {
	Passage  string `json:"passage"`
	Question struct {
		JA string `json:"ja"`
		EN string `json:"en"`
	} `json:"question"`
	Choices []string `json:"choices"`
	Answer  string   `json:"answer"`
}

// GeneratePassage writes a short passage using every word in in.Words and
// one comprehension question about it.
//
// The point is a SECOND encounter in context: a word met once in reading
// and then only ever seen alone on a flip card never gets the second
// exposure that fixes it. The question deliberately asks what the passage
// conveys rather than which word appeared where — the latter is a search
// task, and the learner has already found the words by reading them.
//
// The result is an exercise.Exercise shaped like a multiple-choice drill:
// the passage is the Prompt, the question is the instruction, and the
// answer is graded by the same deterministic comparison every other
// choice question uses.
func (a *Agent) GeneratePassage(ctx context.Context, in PassageInput) (exercise.Exercise, ai.StructuredResponse, error) {
	if len(in.Words) == 0 {
		return exercise.Exercise{}, ai.StructuredResponse{}, fmt.Errorf("drill: passage needs at least one word")
	}

	rendered, err := prompts.Render(passagePromptName, promptVersion, struct{ Words []WordRef }{Words: in.Words})
	if err != nil {
		return exercise.Exercise{}, ai.StructuredResponse{}, fmt.Errorf("drill: render passage prompt: %w", err)
	}
	schema, err := schemas.Get(passageSchemaName)
	if err != nil {
		return exercise.Exercise{}, ai.StructuredResponse{}, fmt.Errorf("drill: get schema: %w", err)
	}

	req := ai.StructuredRequest{
		PromptName:       passagePromptName,
		PromptVersion:    promptVersion,
		System:           rendered.System,
		User:             rendered.User,
		SchemaName:       passageSchemaName,
		Schema:           schema,
		MaxTokens:        maxTokens,
		IdentityID:       in.Identity,
		Agent:            agentName,
		ProviderOverride: in.ProviderOverride,
	}

	resp, err := a.gen.GenerateStructured(ctx, req)
	if err != nil {
		return exercise.Exercise{}, resp, fmt.Errorf("drill: generate passage: %w", err)
	}
	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, passageSchemaName, req, resp)
	if err != nil {
		return exercise.Exercise{}, resp, err
	}

	var dto passageDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return exercise.Exercise{}, resp, fmt.Errorf("drill: unmarshal passage: %w", err)
	}

	// The answer must be one of the choices, or the learner cannot pick
	// it and the deterministic grader can never return correct. The
	// schema cannot express that relationship, so it is checked here.
	answer, ok := resolveChoice(dto.Choices, dto.Answer)
	if !ok {
		return exercise.Exercise{}, resp, fmt.Errorf("drill: passage answer %q is not among its choices", dto.Answer)
	}

	return exercise.Exercise{
		IdentityID:     in.Identity,
		Type:           exercise.TypePassageChoice,
		InstructionsJA: dto.Question.JA,
		InstructionsEN: dto.Question.EN,
		Prompt:         dto.Passage,
		Choices:        dto.Choices,
		Answer:         answer,
	}, resp, nil
}

// resolveChoice maps a model's stated answer onto one of the choices.
//
// Exact text first, then a LABEL: models routinely answer "B" or "2" for
// a list they were never given labels for, because multiple-choice
// questions in their training data have them. Refusing those outright
// throws away a passage that is otherwise correct — the model picked the
// right option and merely named it the way it had seen options named.
//
// Only unambiguous labels are accepted: a single letter within range, or
// a 1-based index. Anything else is a genuine mismatch and fails, because
// guessing at what a model meant is how an exercise ends up graded
// against the wrong option.
func resolveChoice(choices []string, answer string) (string, bool) {
	answer = strings.TrimSpace(answer)
	if slices.Contains(choices, answer) {
		return answer, true
	}
	// A letter: A/B/C/D, either case. Skipped when a choice is literally
	// that letter, which the exact match above would already have taken.
	if len([]rune(answer)) == 1 {
		r := unicode.ToUpper([]rune(answer)[0])
		if i := int(r - 'A'); r >= 'A' && i < len(choices) {
			return choices[i], true
		}
	}
	// A 1-based index.
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(choices) {
		return choices[n-1], true
	}
	return "", false
}
