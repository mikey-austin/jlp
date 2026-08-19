package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// A coordinator's OWN tool calls are all consult_specialist, which
// renders as nothing. The structured data a client can draw is read one
// level down, by the specialist — so a coordinated answer is the one
// answer that could never carry a card until its delegates' tool calls
// were adopted.
//
// The fake below is the shape of a real coordinated run: the coordinator
// consults, the specialist reads corrections, the coordinator writes
// prose from the answer.
type delegatingRunner struct {
	srv *Server
	// consultAs is the skill the fake coordinator asks for.
	consultAs string
	// specialistCalls is what the specialist "read".
	specialistCalls []agentrun.ToolCall
	// coordinatorCalls is what the coordinator itself read, if anything.
	coordinatorCalls []agentrun.ToolCall
}

func (r *delegatingRunner) Run(ctx context.Context, in agentrun.RunInput) (agentrun.RunOutput, error) {
	if in.Agent != coordinatorAgent {
		// The specialist leg.
		return agentrun.RunOutput{RunID: "run-child", Text: "助詞の間違いが二つあります。", ToolCalls: r.specialistCalls}, nil
	}
	// The coordinator leg. The real Runner puts the run id in the
	// context it hands its tools; Consult reads it to know who is
	// asking, so the fake has to do the same or the delegated calls are
	// filed under nobody.
	toolCtx := agentrun.WithRunIDForTest(ctx, "run-parent")
	if _, err := r.srv.Consult(toolCtx, "mikey", r.consultAs, "この文を見てください"); err != nil {
		return agentrun.RunOutput{}, err
	}
	return agentrun.RunOutput{RunID: "run-parent", Text: "助詞に気をつけましょう。", ToolCalls: r.coordinatorCalls}, nil
}

func coordinatedServer(t *testing.T, r *delegatingRunner) *Server {
	t.Helper()
	srv := New(r, tools.NewRegistry(), config.A2A{Enabled: true, Path: "/a2a"})
	r.srv = srv
	if r.consultAs == "" {
		r.consultAs = "review_writing"
	}
	return srv
}

// coordinate sends one message on the coordinate skill and returns the
// finished task.
func coordinate(t *testing.T, srv *Server, accepted []string) Task {
	t.Helper()
	msg := map[string]any{
		"message": map[string]any{
			"parts":    []any{map[string]any{"text": "この文を直して、何を勉強すべきか教えてください。"}},
			"metadata": map[string]any{"skill": "coordinate"},
		},
	}
	if accepted != nil {
		msg["configuration"] = map[string]any{"acceptedOutputModes": accepted}
	}
	params, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	res, rpcErr := srv.handleSendMessage(context.Background(), "mikey", params)
	if rpcErr != nil {
		t.Fatalf("SendMessage: %+v", rpcErr)
	}
	out, ok := res.(sendMessageResult)
	if !ok || out.Task == nil {
		t.Fatalf("result is %T, want a task", res)
	}
	return *out.Task
}

const ungatedCorrections = `[{"id":"c1","original":"映画を見行った","replacement":"映画を見に行った","type":"particle","severity":"minor","explanation_en":"needs に"}]`

func TestASpecialistsToolCallsBecomeCardsOnTheCoordinatorsAnswer(t *testing.T) {
	srv := coordinatedServer(t, &delegatingRunner{
		specialistCalls: []agentrun.ToolCall{{Name: "get_recent_errors", Result: ungatedCorrections}},
	})

	task := coordinate(t, srv, []string{"text/plain", mediaCorrection})
	if len(task.Artifacts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(task.Artifacts))
	}
	parts := task.Artifacts[0].Parts

	if parts[0].Text == nil || *parts[0].Text == "" {
		t.Error("the artifact does not lead with prose; a client that ignores data parts would get nothing")
	}
	var media []string
	for _, p := range parts[1:] {
		media = append(media, p.MediaType)
	}
	if len(media) != 1 || media[0] != mediaCorrection {
		t.Fatalf("data parts = %v, want just %q — the specialist read corrections and they died with its run",
			media, mediaCorrection)
	}
}

// The whole point of collecting raw calls rather than finished parts:
// the delegated data has to pass the same client-capability filter as
// direct data, or a coordinated answer hands a text-only client a blob
// it can only print as JSON.
func TestADelegatedCardObeysWhatTheClientSaidItCanRender(t *testing.T) {
	srv := coordinatedServer(t, &delegatingRunner{
		specialistCalls: []agentrun.ToolCall{{Name: "get_recent_errors", Result: ungatedCorrections}},
	})

	task := coordinate(t, srv, []string{"text/plain"})
	for _, p := range task.Artifacts[0].Parts {
		if p.MediaType != "" {
			t.Errorf("sent a %q part to a client that accepted only text/plain", p.MediaType)
		}
	}
}

// Coordinator and specialist can read the same kind of thing. Two cards
// for one media type is the stacked-widget bug widgetParts already
// solves — this pins that delegated calls go through it rather than
// around it.
func TestOneCardPerMediaTypeAcrossTheCoordinatorAndItsSpecialist(t *testing.T) {
	srv := coordinatedServer(t, &delegatingRunner{
		coordinatorCalls: []agentrun.ToolCall{{Name: "get_correction_history", Result: ungatedCorrections}},
		specialistCalls:  []agentrun.ToolCall{{Name: "get_recent_errors", Result: ungatedCorrections}},
	})

	task := coordinate(t, srv, []string{"text/plain", mediaCorrection})
	cards := 0
	for _, p := range task.Artifacts[0].Parts {
		if p.MediaType == mediaCorrection {
			cards++
		}
	}
	if cards != 1 {
		t.Errorf("got %d correction cards, want 1 — delegated calls are bypassing widgetParts' dedup", cards)
	}
}

// The ninth-surface guard, now that delegation is a route to a data
// part. A specialist's tool results are redacted by internal/tools
// exactly as a direct agent's are, and this pins that the delegated
// route does not acquire its own un-redacted copy on the way up.
func TestAGatedCorrectionStaysGatedThroughDelegation(t *testing.T) {
	// Exactly what internal/tools emits for a gated correction: the
	// answer-bearing fields are already blank.
	gated := `[{"id":"c1","original":"昨日、映画を見行った","type":"particle","severity":"minor","status":"presented","attempts":2}]`
	srv := coordinatedServer(t, &delegatingRunner{
		specialistCalls: []agentrun.ToolCall{{Name: "get_recent_errors", Result: gated}},
	})

	task := coordinate(t, srv, []string{"text/plain", mediaCorrection})

	// Assert the card is THERE before asserting what it lacks. A test
	// that only checks for absent strings passes just as happily when
	// no data part was produced at all, and would then be guarding
	// nothing.
	var card *Part
	for i, p := range task.Artifacts[0].Parts {
		if p.MediaType == mediaCorrection {
			card = &task.Artifacts[0].Parts[i]
		}
	}
	if card == nil {
		t.Fatal("no correction card, so this test proves nothing about what a card may carry")
	}

	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, leak := range []string{"replacement", "explanation_en", "spans"} {
		if strings.Contains(body, leak) {
			t.Errorf("a delegated gated correction carries %q, handing the learner an answer they have not earned:\n%s", leak, body)
		}
	}
}

// The map is keyed by the consulting run, and a run that has produced
// its task is done. Left behind, these entries accumulate one payload
// per question ever asked — and payloads are whole tool results, not
// flags.
func TestDelegatedCallsDoNotOutliveTheRunThatCollectedThem(t *testing.T) {
	srv := coordinatedServer(t, &delegatingRunner{
		specialistCalls: []agentrun.ToolCall{{Name: "get_recent_errors", Result: ungatedCorrections}},
	})

	coordinate(t, srv, []string{"text/plain", mediaCorrection})

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	if len(srv.delegated) != 0 {
		t.Errorf("%d run(s) still hold delegated tool results after finishing", len(srv.delegated))
	}
}

// A direct Consult — no coordinating run above it — has no task to
// attach cards to. Filing them under "" would mean they are never
// collected and never dropped.
func TestConsultOutsideARunKeepsNothing(t *testing.T) {
	// A runner that DOES return tool calls: with an empty one the
	// "nothing was kept" assertion holds for the wrong reason — there
	// was nothing to keep — and the guard being tested never runs.
	r := &delegatingRunner{specialistCalls: []agentrun.ToolCall{{Name: "get_recent_errors", Result: ungatedCorrections}}}
	srv := coordinatedServer(t, r)

	if _, err := srv.Consult(context.Background(), learner.IdentityID("mikey"), "review_writing", "これを見て"); err != nil {
		t.Fatalf("consult: %v", err)
	}
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	if len(srv.delegated) != 0 {
		t.Errorf("kept %d entry/entries for a consultation with no run to attach to", len(srv.delegated))
	}
}
