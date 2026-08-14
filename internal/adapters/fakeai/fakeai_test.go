package fakeai

import (
	"context"
	"encoding/json"
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
