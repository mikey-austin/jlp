package teacher_test

import (
	"context"
	"strings"
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

// spyGen is a local ai.StructuredGenerator test double that records the
// last ai.StructuredRequest it was called with (so a test can inspect
// the fully-rendered System/User prompt text) and always returns a
// fixed, schema-valid empty-corrections response.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	return ai.StructuredResponse{
		JSON:     []byte(`{"corrections":[]}`),
		Provider: "spy",
		Model:    "spy-1",
	}, nil
}

// TestReviewWritingRendersV3PromptWithConceptCandidates pins the Step 1
// scenario from the task brief: ReviewInput.ConceptCandidates flows
// through to the rendered teacher.feedback.v3 prompt — the user half
// lists each candidate line, and the system half carries the
// tag-from-candidates-only instruction — and the request is sent as
// prompt version "v3" (Phase 2 Task 8: teacher always renders v3 now,
// schema correction_result.v2), not "v2" or "v1".
func TestReviewWritingRendersV3PromptWithConceptCandidates(t *testing.T) {
	gen := &spyGen{}
	agent := teacher.New(gen)

	in := testReviewInput()
	in.ConceptCandidates = []string{"i-adjective-past — い-adjective past tense (〜かった)"}

	_, _, err := agent.ReviewWriting(context.Background(), in)
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}

	if gen.req.PromptVersion != "v3" {
		t.Fatalf("PromptVersion = %q, want v3", gen.req.PromptVersion)
	}
	if gen.req.PromptName != "teacher.feedback" {
		t.Fatalf("PromptName = %q, want teacher.feedback", gen.req.PromptName)
	}
	if gen.req.SchemaName != "correction_result.v2" {
		t.Fatalf("SchemaName = %q, want correction_result.v2", gen.req.SchemaName)
	}
	if !strings.Contains(gen.req.User, "i-adjective-past — い-adjective past tense (〜かった)") {
		t.Fatalf("User prompt missing the candidate line: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.System, "chosen ONLY from the provided candidate list") {
		t.Fatalf("System prompt missing the tag-from-candidates instruction: %s", gen.req.System)
	}
}

// TestReviewWritingRendersSocraticHintInstructionAndTeacherMode pins
// Task 8's prompt v3 addition: the system half carries the exact
// socratic hint instruction sentence, and the user half's opening line
// carries the session's actual TeacherMode value — the signal
// adapters/fakeai keys its hint attachment off of (see that package's
// socraticMarker doc comment).
func TestReviewWritingRendersSocraticHintInstructionAndTeacherMode(t *testing.T) {
	gen := &spyGen{}
	agent := teacher.New(gen)

	in := testReviewInput()
	in.Session.Profile.TeacherMode = "socratic"

	_, _, err := agent.ReviewWriting(context.Background(), in)
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}

	wantInstruction := "Teacher mode 'socratic': for each correction ALSO provide a `hint`"
	if !strings.Contains(gen.req.System, wantInstruction) {
		t.Fatalf("System prompt missing the socratic hint instruction: %s", gen.req.System)
	}
	if !strings.Contains(gen.req.User, "Teacher mode: socratic") {
		t.Fatalf("User prompt missing the rendered Teacher mode line: %s", gen.req.User)
	}
}

// TestReviewWritingFakeAISocraticHappyPathIncludesHint is an
// end-to-end (teacher.Agent + real fakeai) pin that a socratic session
// reviewing the known-bad conjugation gets back a Correction whose Hint
// is populated — proving the DTO→domain mapping teacher.go's
// ReviewWriting does for the new `hint` field actually works, not just
// that fakeai emits the right JSON shape (fakeai_test.go's job) or that
// the prompt renders the right marker (the test above's job).
func TestReviewWritingFakeAISocraticHappyPathIncludesHint(t *testing.T) {
	agent := teacher.New(fakeai.New())

	sess := testSession()
	sess.Profile.TeacherMode = "socratic"
	in := teacher.ReviewInput{
		Identity:  "learner-a",
		Session:   sess,
		Selection: "とても面白いでした",
		Context:   "とても面白いでした",
	}
	result, _, err := agent.ReviewWriting(context.Background(), in)
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}
	if len(result.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(result.Corrections), result.Corrections)
	}
	c := result.Corrections[0]
	if !c.HasHint() {
		t.Fatalf("Correction has no hint in socratic mode: %+v", c)
	}
	if c.Hint.JA == "" || c.Hint.EN == "" {
		t.Fatalf("Hint = %+v, want both JA and EN populated", c.Hint)
	}
	if strings.Contains(c.Hint.JA, c.Replacement) || strings.Contains(c.Hint.EN, c.Replacement) {
		t.Fatalf("Hint leaks the corrected form %q: %+v", c.Replacement, c.Hint)
	}
}

// TestReviewWritingFakeAINonSocraticOmitsHint is the same end-to-end
// shape as above but for the default "teacher" mode: no hint at all.
func TestReviewWritingFakeAINonSocraticOmitsHint(t *testing.T) {
	agent := teacher.New(fakeai.New())

	in := teacher.ReviewInput{
		Identity:  "learner-a",
		Session:   testSession(), // TeacherMode: "teacher"
		Selection: "とても面白いでした",
		Context:   "とても面白いでした",
	}
	result, _, err := agent.ReviewWriting(context.Background(), in)
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}
	if len(result.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(result.Corrections), result.Corrections)
	}
	if result.Corrections[0].HasHint() {
		t.Fatalf("Correction unexpectedly has a hint in non-socratic mode: %+v", result.Corrections[0])
	}
}

// TestReviewWritingRendersExpressionsToEncourage pins Task 7's PRD
// §55/§17.5 addition: ReviewInput.ExpressionsToEncourage flows through
// to the rendered teacher.feedback.v2 USER prompt as a bulleted list
// under the "gently encourage one" instruction.
func TestReviewWritingRendersExpressionsToEncourage(t *testing.T) {
	gen := &spyGen{}
	agent := teacher.New(gen)

	in := testReviewInput()
	in.ExpressionsToEncourage = []string{"それはそれとして — that aside; setting that aside for now"}

	_, _, err := agent.ReviewWriting(context.Background(), in)
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}

	if !strings.Contains(gen.req.User, "gently encourage one") {
		t.Fatalf("User prompt missing the encourage-block instruction: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "- それはそれとして — that aside; setting that aside for now") {
		t.Fatalf("User prompt missing the bulleted candidate line: %s", gen.req.User)
	}
}

// TestReviewWritingOmitsExpressionsToEncourageSectionWhenEmpty: an
// empty/unset ExpressionsToEncourage must produce no encourage section
// at all (the template's {{if .ExpressionsToEncourage}} guard), not an
// empty-but-present header.
func TestReviewWritingOmitsExpressionsToEncourageSectionWhenEmpty(t *testing.T) {
	gen := &spyGen{}
	agent := teacher.New(gen)

	_, _, err := agent.ReviewWriting(context.Background(), testReviewInput())
	if err != nil {
		t.Fatalf("ReviewWriting returned error: %v", err)
	}
	if strings.Contains(gen.req.User, "gently encourage") {
		t.Fatalf("User prompt unexpectedly contains the encourage section with no ExpressionsToEncourage set: %s", gen.req.User)
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
