package fakeai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

type wantCorrection struct {
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
	Type        string `json:"type"`
	Severity    string `json:"severity"`
	Explanation struct {
		JA string `json:"ja"`
		EN string `json:"en"`
	} `json:"explanation"`
	Concepts []string `json:"concepts,omitempty"`
	Hint     *struct {
		JA string `json:"ja"`
		EN string `json:"en"`
	} `json:"hint,omitempty"`
}

type wantResult struct {
	Corrections []wantCorrection `json:"corrections"`
}

func generate(t *testing.T, user string) (ai.StructuredResponse, wantResult) {
	t.Helper()
	gen := New()
	req := ai.StructuredRequest{
		PromptName:    "teacher.feedback",
		PromptVersion: "v1",
		System:        "system prompt",
		User:          user,
		SchemaName:    "correction_result.v1",
		Agent:         "teacher",
	}
	resp, err := gen.GenerateStructured(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}
	if err := schemas.Validate("correction_result.v1", resp.JSON); err != nil {
		t.Fatalf("response JSON failed schema validation: %v\nJSON: %s", err, resp.JSON)
	}
	if resp.Provider != "fake" {
		t.Errorf("Provider = %q, want %q", resp.Provider, "fake")
	}
	if resp.Model != "fake-1" {
		t.Errorf("Model = %q, want %q", resp.Model, "fake-1")
	}
	wantIn := len([]rune(req.System)) + len([]rune(req.User))
	if resp.InputTokens != wantIn {
		t.Errorf("InputTokens = %d, want %d (rune count of system+user)", resp.InputTokens, wantIn)
	}
	wantOut := len([]rune(string(resp.JSON)))
	if resp.OutputTokens != wantOut {
		t.Errorf("OutputTokens = %d, want %d (rune count of response JSON)", resp.OutputTokens, wantOut)
	}

	var got wantResult
	if err := json.Unmarshal(resp.JSON, &got); err != nil {
		t.Fatalf("unmarshal response JSON: %v", err)
	}
	return resp, got
}

func TestFakeAdapterOmoshiroiPastTense(t *testing.T) {
	_, got := generate(t, "友達と映画を見ました。とても面白いでした。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	c := got.Corrections[0]
	if c.Original != "面白いでした" {
		t.Errorf("Original = %q, want %q", c.Original, "面白いでした")
	}
	if c.Replacement != "面白かったです" {
		t.Errorf("Replacement = %q, want %q", c.Replacement, "面白かったです")
	}
	if c.Type != "conjugation" {
		t.Errorf("Type = %q, want %q", c.Type, "conjugation")
	}
	if c.Severity != "incorrect" {
		t.Errorf("Severity = %q, want %q", c.Severity, "incorrect")
	}
	wantJA := "い形容詞の過去形は「〜かった」を使います。「面白い」→「面白かった」。"
	wantEN := "い-adjectives form the past tense with 〜かった, so 面白いでした must be 面白かったです."
	if c.Explanation.JA != wantJA {
		t.Errorf("Explanation.JA = %q, want %q", c.Explanation.JA, wantJA)
	}
	if c.Explanation.EN != wantEN {
		t.Errorf("Explanation.EN = %q, want %q", c.Explanation.EN, wantEN)
	}
}

func TestFakeAdapterTanoshiiPastTense(t *testing.T) {
	_, got := generate(t, "旅行はとても楽しいでした。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	c := got.Corrections[0]
	if c.Original != "楽しいでした" {
		t.Errorf("Original = %q, want %q", c.Original, "楽しいでした")
	}
	if c.Replacement != "楽しかったです" {
		t.Errorf("Replacement = %q, want %q", c.Replacement, "楽しかったです")
	}
	if c.Type != "conjugation" || c.Severity != "incorrect" {
		t.Errorf("Type/Severity = %q/%q, want conjugation/incorrect", c.Type, c.Severity)
	}
	wantJA := "い形容詞の過去形は「〜かった」を使います。「楽しい」→「楽しかった」。"
	wantEN := "い-adjectives form the past tense with 〜かった, so 楽しいでした must be 楽しかったです."
	if c.Explanation.JA != wantJA {
		t.Errorf("Explanation.JA = %q, want %q", c.Explanation.JA, wantJA)
	}
	if c.Explanation.EN != wantEN {
		t.Errorf("Explanation.EN = %q, want %q", c.Explanation.EN, wantEN)
	}
}

func TestFakeAdapterWoIkimasuParticle(t *testing.T) {
	_, got := generate(t, "明日、学校を行きます。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	c := got.Corrections[0]
	if c.Original != "を" {
		t.Errorf("Original = %q, want %q", c.Original, "を")
	}
	if c.Replacement != "に" {
		t.Errorf("Replacement = %q, want %q", c.Replacement, "に")
	}
	if c.Type != "particle" {
		t.Errorf("Type = %q, want %q", c.Type, "particle")
	}
	if c.Severity != "incorrect" {
		t.Errorf("Severity = %q, want %q", c.Severity, "incorrect")
	}
	if len(c.Concepts) != 1 || c.Concepts[0] != "particle-ni-direction" {
		t.Errorf("Concepts = %v, want [particle-ni-direction] (must match the real catalog slug, not the retired particle-ni-destination)", c.Concepts)
	}
}

func TestFakeAdapterNoMatchReturnsEmptyCorrections(t *testing.T) {
	_, got := generate(t, "今日はいい天気ですね。公園を散歩しました。")
	if len(got.Corrections) != 0 {
		t.Fatalf("Corrections = %+v, want empty", got.Corrections)
	}
}

func TestFakeAdapterUnsupportedSchemaErrors(t *testing.T) {
	gen := New()
	req := ai.StructuredRequest{
		PromptName:    "some.other.capability",
		PromptVersion: "v1",
		System:        "system prompt",
		User:          "hello",
		SchemaName:    "some_other_schema.v1",
	}
	resp, err := gen.GenerateStructured(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for unsupported schema name, got nil")
	}
	// Provider/Model are known regardless of outcome, so the caller (and
	// the observability decorator wrapping it) can still tell which
	// provider/model the failed call went through.
	if resp.Provider != "fake" || resp.Model != "fake-1" {
		t.Errorf("Provider/Model on error = %q/%q, want fake/fake-1", resp.Provider, resp.Model)
	}
}

func TestFakeAdapterEmptySchemaNameIsAccepted(t *testing.T) {
	// A caller that omits SchemaName gets the (only) fake response shape
	// rather than an error — the guard only rejects an explicit mismatch.
	if _, err := New().GenerateStructured(context.Background(), ai.StructuredRequest{User: "hello"}); err != nil {
		t.Fatalf("GenerateStructured with empty SchemaName returned error: %v", err)
	}
}

// generateV2 mirrors generate above but against schemaV2, and prefixes
// user with the same "Teacher mode: <mode>" line
// internal/agent/teacher's teacher.feedback.v3 USER template renders —
// see fakeai.go's socraticMarker doc comment for why this specific line
// (not the v3 SYSTEM template's separate, always-present "Teacher mode
// 'socratic': ..." instruction sentence) is what GenerateStructured
// keys its hint-attachment decision off of.
func generateV2(t *testing.T, teacherMode, user string) (ai.StructuredResponse, wantResult) {
	t.Helper()
	gen := New()
	req := ai.StructuredRequest{
		PromptName:    "teacher.feedback",
		PromptVersion: "v3",
		System:        "system prompt",
		User:          "Teacher mode: " + teacherMode + "\n\n" + user,
		SchemaName:    "correction_result.v2",
		Agent:         "teacher",
	}
	resp, err := gen.GenerateStructured(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}
	if err := schemas.Validate("correction_result.v2", resp.JSON); err != nil {
		t.Fatalf("response JSON failed schema validation: %v\nJSON: %s", err, resp.JSON)
	}
	var got wantResult
	if err := json.Unmarshal(resp.JSON, &got); err != nil {
		t.Fatalf("unmarshal response JSON: %v", err)
	}
	return resp, got
}

// TestFakeAdapterSocraticModeAttachesHint pins Step 1 of the Task 8
// brief: a schemaV2 request whose rendered User carries "Teacher mode:
// socratic" gets the brief's exact pinned hint text on the
// い-adjective-past correction, and the response still validates
// against correction_result.v2 (hint-ful corrections are the whole
// point of the v2 schema over v1).
func TestFakeAdapterSocraticModeAttachesHint(t *testing.T) {
	_, got := generateV2(t, "socratic", "友達と映画を見ました。とても面白いでした。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	c := got.Corrections[0]
	if c.Hint == nil {
		t.Fatal("Hint was not attached in socratic mode")
	}
	wantJA := "い形容詞の過去形の作り方を思い出してください。"
	wantEN := "Recall how い-adjectives form the past tense."
	if c.Hint.JA != wantJA {
		t.Errorf("Hint.JA = %q, want %q", c.Hint.JA, wantJA)
	}
	if c.Hint.EN != wantEN {
		t.Errorf("Hint.EN = %q, want %q", c.Hint.EN, wantEN)
	}
	// The hint must never give away the answer: the corrected form must
	// not appear anywhere in the hint text itself.
	if strings.Contains(c.Hint.JA, "面白かった") || strings.Contains(c.Hint.EN, "面白かった") {
		t.Fatalf("Hint leaks the corrected form: %+v", c.Hint)
	}
}

// TestFakeAdapterNonSocraticModeOmitsHint: the same schemaV2 request,
// but a non-socratic teacher mode, must come back with no hint at all —
// v2 accepting a "hint" property doesn't mean every mode gets one.
func TestFakeAdapterNonSocraticModeOmitsHint(t *testing.T) {
	_, got := generateV2(t, "teacher", "友達と映画を見ました。とても面白いでした。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	if got.Corrections[0].Hint != nil {
		t.Fatalf("Hint = %+v, want nil for a non-socratic teacher mode", got.Corrections[0].Hint)
	}
}

// TestFakeAdapterPurposeContainingSocraticDoesNotFalsePositive pins the
// code-review fix: session.Session.Purpose is learner-supplied free
// text, rendered verbatim into the SAME v3 USER template as the
// "Teacher mode: {{.TeacherMode}}" line (see teacher.feedback.v3.user.md's
// "Session purpose: {{.Purpose}}" line, right below it) — a "teacher"
// mode session whose Purpose happens to contain the word "socratic"
// (e.g. "practicing the socratic method") must NOT get hints attached.
// Before the fix, socraticMarker was a bare "socratic" substring check
// over the whole rendered User, which this Purpose text alone would
// have satisfied regardless of the actual TeacherMode.
func TestFakeAdapterPurposeContainingSocraticDoesNotFalsePositive(t *testing.T) {
	gen := New()
	req := ai.StructuredRequest{
		PromptName:    "teacher.feedback",
		PromptVersion: "v3",
		System:        "system prompt",
		User:          "Teacher mode: teacher\n\nSession purpose: practicing the socratic method\n\nSelection to review:\nとても面白いでした",
		SchemaName:    "correction_result.v2",
		Agent:         "teacher",
	}
	resp, err := gen.GenerateStructured(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}
	if err := schemas.Validate("correction_result.v2", resp.JSON); err != nil {
		t.Fatalf("response JSON failed schema validation: %v\nJSON: %s", err, resp.JSON)
	}
	var got wantResult
	if err := json.Unmarshal(resp.JSON, &got); err != nil {
		t.Fatalf("unmarshal response JSON: %v", err)
	}
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	if got.Corrections[0].Hint != nil {
		t.Fatalf("Hint = %+v, want nil — TeacherMode is \"teacher\", the word \"socratic\" only appears inside the unrelated Purpose text", got.Corrections[0].Hint)
	}
}

// TestFakeAdapterSchemaV1StillSupported: fakeai must keep answering
// schemaV1 requests exactly as before Task 8 — v1's item schema has no
// "hint" property at all, so a v1-shaped response must never carry one,
// regardless of teacher mode.
func TestFakeAdapterSchemaV1StillSupported(t *testing.T) {
	_, got := generate(t, "Teacher mode: socratic\n\nとても面白いでした。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	if got.Corrections[0].Hint != nil {
		t.Fatalf("Hint = %+v, want nil for a v1 schema request even when the prompt mentions socratic", got.Corrections[0].Hint)
	}
}

// conversationTurnResult mirrors schemas/defs/conversation_turn.v1.json
// field-for-field — this test file's own DTO, kept separate from
// wantResult (correction_result's own shape) since conversation_turn.v1
// wraps the same corrections array inside a reply/reply_en/followup
// envelope.
type conversationTurnResult struct {
	Reply       string           `json:"reply"`
	ReplyEN     string           `json:"reply_en"`
	Corrections []wantCorrection `json:"corrections"`
	Followup    string           `json:"followup"`
}

func generateConversationTurn(t *testing.T, teacherMode, user string) (ai.StructuredResponse, conversationTurnResult) {
	t.Helper()
	gen := New()
	req := ai.StructuredRequest{
		PromptName:    "conversation.turn",
		PromptVersion: "v1",
		System:        "system prompt",
		User:          "Teacher mode: " + teacherMode + "\n\nSession purpose: Casual conversation practice\n\nThe learner just said:\n" + user + "\n",
		SchemaName:    "conversation_turn.v1",
		Agent:         "conversation",
	}
	resp, err := gen.GenerateStructured(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}
	if err := schemas.Validate("conversation_turn.v1", resp.JSON); err != nil {
		t.Fatalf("response JSON failed schema validation: %v\nJSON: %s", err, resp.JSON)
	}
	var got conversationTurnResult
	if err := json.Unmarshal(resp.JSON, &got); err != nil {
		t.Fatalf("unmarshal response JSON: %v", err)
	}
	return resp, got
}

// TestFakeAdapterConversationTurnCleanMessageHasNoCorrections pins the
// Phase 4 Task 6 fixture: a message with no known mistake gets a reply
// and no corrections.
func TestFakeAdapterConversationTurnCleanMessageHasNoCorrections(t *testing.T) {
	_, got := generateConversationTurn(t, "teacher", "今日はいい天気ですね。")
	if got.Reply == "" {
		t.Fatal("Reply is empty")
	}
	if len(got.Corrections) != 0 {
		t.Fatalf("Corrections = %+v, want none", got.Corrections)
	}
}

// TestFakeAdapterConversationTurnKnownMistakeYieldsCorrection pins the
// same i-adjective-past fixture every other schema in this file keys
// off of, now reached through conversation_turn.v1.
func TestFakeAdapterConversationTurnKnownMistakeYieldsCorrection(t *testing.T) {
	_, got := generateConversationTurn(t, "teacher", "昨日の映画はとても面白いでした。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	if got.Corrections[0].Original != "面白いでした" || got.Corrections[0].Replacement != "面白かったです" {
		t.Fatalf("Corrections[0] = %+v, want 面白いでした -> 面白かったです", got.Corrections[0])
	}
	if got.Corrections[0].Hint != nil {
		t.Fatalf("Hint = %+v, want nil in non-socratic teacher mode", got.Corrections[0].Hint)
	}
}

// TestFakeAdapterConversationTurnSocraticAttachesHint mirrors
// TestFakeAdapterSocraticModeAttachesHint for the conversation_turn.v1
// path.
func TestFakeAdapterConversationTurnSocraticAttachesHint(t *testing.T) {
	_, got := generateConversationTurn(t, "socratic", "昨日の映画はとても面白いでした。")
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	if got.Corrections[0].Hint == nil {
		t.Fatal("Hint is nil, want a socratic hint attached")
	}
}

// TestFakeAdapterConversationTurnHistoryDoesNotDoubleCount pins the
// bug fix in currentMessage: a prior turn's mistake, quoted verbatim in
// the "Conversation so far:" history section, must NOT be re-detected
// as a fresh correction on a later turn whose own new message is clean.
func TestFakeAdapterConversationTurnHistoryDoesNotDoubleCount(t *testing.T) {
	gen := New()
	req := ai.StructuredRequest{
		PromptName:    "conversation.turn",
		PromptVersion: "v1",
		System:        "system prompt",
		User: "Teacher mode: teacher\n\nSession purpose: Casual conversation practice\n\n" +
			"Conversation so far:\n学習者: 昨日の映画はとても面白いでした。\nあなた: なるほど、教えてくれてありがとうございます。\n\n" +
			"The learner just said:\n映画について話しましょう。\n",
		SchemaName: "conversation_turn.v1",
		Agent:      "conversation",
	}
	resp, err := gen.GenerateStructured(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}
	var got conversationTurnResult
	if err := json.Unmarshal(resp.JSON, &got); err != nil {
		t.Fatalf("unmarshal response JSON: %v", err)
	}
	if len(got.Corrections) != 0 {
		t.Fatalf("Corrections = %+v, want none (the mistake belongs to the quoted history, not this turn's own message)", got.Corrections)
	}
}
