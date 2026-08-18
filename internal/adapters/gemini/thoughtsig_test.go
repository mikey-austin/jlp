package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// Gemini 3 signs every functionCall part it emits and rejects the next
// request outright if that signature does not come back:
//
//	Function call is missing a thought_signature in functionCall parts.
//	This is required for tools to work correctly.
//
// So an agentic run against Gemini failed on its SECOND round trip —
// the moment a tool result was fed back — which is why it looked like
// the A2A chat simply did not work while single-shot generation was
// fine.
func TestThoughtSignatureSurvivesAToolCallRoundTrip(t *testing.T) {
	const sig = "sig-abc123"

	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_vocabulary_history","args":{"filter":"all"}},"thoughtSignature":"` + sig + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5},"modelVersion":"gemini-3-flash-preview"}`)
	srv, _ := capturingServer(t, http.StatusOK, body)
	g := New(testConfig(srv.URL), nil)

	resp, err := g.CallWithTools(context.Background(), ai.ToolRequest{
		System:   "investigate",
		Messages: []ai.ToolMessage{{Role: "user", Text: "何を覚えましたか"}},
		Tools: []ai.ToolDef{{
			Name:   "get_vocabulary_history",
			Schema: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(resp.Turn.Invocations) != 1 {
		t.Fatalf("got %d invocations, want 1", len(resp.Turn.Invocations))
	}
	if got := resp.Turn.Invocations[0].ProviderState; got != sig {
		t.Fatalf("the thought signature was dropped on the way in: got %q, want %q", got, sig)
	}

	// Second round trip: the same invocation is replayed in history,
	// alongside its result. The signature must be on the wire.
	srv2, captured := capturingServer(t, http.StatusOK,
		[]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"わかりました"}]},"finishReason":"STOP"}],"usageMetadata":{},"modelVersion":"gemini-3-flash-preview"}`))
	g2 := New(testConfig(srv2.URL), nil)

	_, err = g2.CallWithTools(context.Background(), ai.ToolRequest{
		System: "investigate",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "何を覚えましたか"},
			{Role: "assistant", Invocations: resp.Turn.Invocations},
			{Role: "tool", Results: []ai.ToolResult{{ID: resp.Turn.Invocations[0].ID, Content: "[]"}}},
		},
		Tools: []ai.ToolDef{{
			Name:   "get_vocabulary_history",
			Schema: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !strings.Contains(string(captured.body), sig) {
		t.Fatalf("the replayed functionCall carries no thoughtSignature, so Gemini would reject it:\n%s", captured.body)
	}
}

// A provider with no such concept must not have an empty signature
// serialized onto its parts — omitempty is what keeps the wire clean.
func TestNoThoughtSignatureIsNotSentAsEmpty(t *testing.T) {
	parts, err := json.Marshal(part{FunctionCall: &functionCall{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(parts), "thoughtSignature") {
		t.Errorf("an unsigned functionCall serialized a thoughtSignature key: %s", parts)
	}
}
