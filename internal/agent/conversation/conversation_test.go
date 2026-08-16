package conversation_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double injected via conversation.New(ai.StructuredGenerator); PRD §75 Rule 3 forbids agents reaching real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/agent/conversation"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

func testSession() session.Session {
	return session.Session{
		ID:      "sess-1",
		Purpose: "Casual conversation practice",
		Profile: session.Profile{
			TeacherMode:         "teacher",
			Strictness:          "balanced",
			ExplanationLanguage: "both",
			Register:            "polite",
			FeedbackTiming:      "end",
		},
	}
}

func TestTurnFakeAIHappyPathReplies(t *testing.T) {
	agent := conversation.New(fakeai.New())

	turn, resp, err := agent.Turn(context.Background(), conversation.TurnInput{
		Identity: "learner-a",
		Session:  testSession(),
		Message:  "今日はいい天気ですね。",
	})
	if err != nil {
		t.Fatalf("Turn returned error: %v", err)
	}
	if turn.Reply == "" {
		t.Fatal("Turn.Reply is empty")
	}
	if len(turn.Corrections) != 0 {
		t.Fatalf("len(Corrections) = %d, want 0: %+v", len(turn.Corrections), turn.Corrections)
	}
	if resp.Provider != "fake" {
		t.Fatalf("resp.Provider = %q, want fake", resp.Provider)
	}
}

func TestTurnFakeAIKnownMistakeYieldsCorrection(t *testing.T) {
	agent := conversation.New(fakeai.New())

	turn, _, err := agent.Turn(context.Background(), conversation.TurnInput{
		Identity: "learner-a",
		Session:  testSession(),
		Message:  "昨日の映画はとても面白いでした。",
	})
	if err != nil {
		t.Fatalf("Turn returned error: %v", err)
	}
	if len(turn.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(turn.Corrections), turn.Corrections)
	}
	c := turn.Corrections[0]
	if c.Original != "面白いでした" || c.Replacement != "面白かったです" {
		t.Fatalf("Corrections[0] = %+v, want 面白いでした -> 面白かったです", c)
	}
	if c.ID == "" {
		t.Fatal("Correction ID was not assigned")
	}
	if c.HasHint() {
		t.Fatalf("non-socratic correction unexpectedly has a hint: %+v", c)
	}
}

func TestTurnFakeAISocraticIncludesHintNotReplacement(t *testing.T) {
	agent := conversation.New(fakeai.New())

	sess := testSession()
	sess.Profile.TeacherMode = "socratic"
	turn, _, err := agent.Turn(context.Background(), conversation.TurnInput{
		Identity: "learner-a",
		Session:  sess,
		Message:  "昨日の映画はとても面白いでした。",
	})
	if err != nil {
		t.Fatalf("Turn returned error: %v", err)
	}
	if len(turn.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(turn.Corrections), turn.Corrections)
	}
	c := turn.Corrections[0]
	if !c.HasHint() {
		t.Fatalf("socratic correction has no hint: %+v", c)
	}
	if c.Hint.JA == "" || c.Hint.EN == "" {
		t.Fatalf("Hint = %+v, want both JA and EN populated", c.Hint)
	}
}

// spyGen records the last request it saw, so a test can inspect the
// fully-rendered System/User prompt text — same pattern
// agent/teacher's own tests use.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	return ai.StructuredResponse{JSON: []byte(`{"reply":"了解です。"}`), Provider: "spy", Model: "spy-1"}, nil
}

func TestTurnRendersHistoryConceptCandidatesAndExpressions(t *testing.T) {
	gen := &spyGen{}
	agent := conversation.New(gen)

	_, _, err := agent.Turn(context.Background(), conversation.TurnInput{
		Identity: "learner-a",
		Session:  testSession(),
		History: []conversation.HistoryTurn{
			{LearnerText: "こんにちは。", Reply: "こんにちは！元気ですか？"},
		},
		Message:                "映画を見ました。",
		ConceptCandidates:      []string{"i-adjective-past — い-adjective past tense (〜かった)"},
		ExpressionsToEncourage: []string{"それはそれとして — that aside; setting that aside for now"},
	})
	if err != nil {
		t.Fatalf("Turn returned error: %v", err)
	}

	if gen.req.PromptName != "conversation.turn" || gen.req.PromptVersion != "v1" {
		t.Fatalf("PromptName/Version = %q/%q, want conversation.turn/v1", gen.req.PromptName, gen.req.PromptVersion)
	}
	if gen.req.SchemaName != "conversation_turn.v1" {
		t.Fatalf("SchemaName = %q, want conversation_turn.v1", gen.req.SchemaName)
	}
	if !strings.Contains(gen.req.User, "こんにちは。") || !strings.Contains(gen.req.User, "こんにちは！元気ですか？") {
		t.Fatalf("User prompt missing conversation history: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "i-adjective-past — い-adjective past tense (〜かった)") {
		t.Fatalf("User prompt missing concept candidate line: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "それはそれとして — that aside; setting that aside for now") {
		t.Fatalf("User prompt missing expression-to-encourage line: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "映画を見ました。") {
		t.Fatalf("User prompt missing the learner's new message: %s", gen.req.User)
	}
}

func TestTurnFailsAfterRepairAndRetryExhausted(t *testing.T) {
	gen := &flakyGen{payloads: [][]byte{
		[]byte("nonsense, no braces here"),
		[]byte("still nonsense"),
	}}
	agent := conversation.New(gen)

	_, _, err := agent.Turn(context.Background(), conversation.TurnInput{
		Identity: "learner-a",
		Session:  testSession(),
		Message:  "こんにちは。",
	})
	if err == nil {
		t.Fatal("expected an error after repair and retry are exhausted, got nil")
	}
	if gen.calls != 2 {
		t.Fatalf("gen.calls = %d, want 2 (initial call + one retry, then fail)", gen.calls)
	}
}

// flakyGen returns a fixed sequence of raw payloads, same double
// agent/teacher's own tests use to exercise Rule 4's bounded
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
	return ai.StructuredResponse{JSON: f.payloads[i], Provider: "flaky", Model: "flaky-1"}, nil
}
