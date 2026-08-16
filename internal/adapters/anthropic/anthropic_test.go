package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

// cannedToolUseResponse is the exact Anthropic Messages API response
// from the task brief: a tool_use block naming emit_result, whose
// input is the structured correction result we asked for.
const cannedToolUseResponse = `{"id":"msg_test","model":"claude-sonnet-5","role":"assistant","stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"tu_1","name":"emit_result",
   "input":{"corrections":[{"original":"面白いでした","replacement":"面白かったです","type":"conjugation","severity":"incorrect","explanation":{"ja":"×","en":"x"}}]}}],
 "usage":{"input_tokens":210,"output_tokens":96}}`

const cannedTextOnlyResponse = `{"id":"msg_test2","model":"claude-sonnet-5","role":"assistant","stop_reason":"end_turn",
 "content":[{"type":"text","text":"I can't help with that."}],
 "usage":{"input_tokens":50,"output_tokens":10}}`

// cannedWrongToolResponse has a tool_use block, but naming a tool
// other than the one we forced — this must be treated the same as no
// tool_use block at all, since it isn't a call to emit_result.
const cannedWrongToolResponse = `{"id":"msg_test3","model":"claude-sonnet-5","role":"assistant","stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"tu_2","name":"some_other_tool","input":{}}],
 "usage":{"input_tokens":40,"output_tokens":8}}`

// cannedAPIErrorResponse is the Anthropic error envelope shape for a
// non-2xx response.
const cannedAPIErrorResponse = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`

type wantExplanation struct {
	JA string `json:"ja"`
	EN string `json:"en"`
}

type wantCorrection struct {
	Original    string          `json:"original"`
	Replacement string          `json:"replacement"`
	Type        string          `json:"type"`
	Severity    string          `json:"severity"`
	Explanation wantExplanation `json:"explanation"`
}

type wantResult struct {
	Corrections []wantCorrection `json:"corrections"`
}

// requestAssertion is a captured, decoded copy of the single HTTP
// request the adapter sent, so tests can inspect it after the call
// completes.
type requestAssertion struct {
	Path   string
	APIKey string
	Body   map[string]any
}

// newTestServer starts an httptest server that records the request it
// receives into captured, asserts the wire-level invariants common to
// every GenerateStructured call, and replies with respBody.
func newTestServer(t *testing.T, captured *requestAssertion, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Path = r.URL.Path
		captured.APIKey = r.Header.Get("x-api-key")

		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request body is not valid JSON: %v\nbody: %s", err, raw)
		}
		captured.Body = body

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
}

func TestGenerateStructuredForcesToolUseAndParsesResult(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedToolUseResponse)
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL}, nil)
	req := ai.StructuredRequest{
		PromptName:    "teacher.feedback",
		PromptVersion: "v1",
		System:        "You are a Japanese writing teacher.",
		User:          "友達と映画を見ました。とても面白いでした。",
		SchemaName:    "correction_result.v1",
		Schema:        schema,
		Agent:         "teacher",
	}

	resp, err := gen.GenerateStructured(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}

	// --- Wire-level assertions on the single outgoing request ---
	if captured.Path != "/v1/messages" {
		t.Errorf("request path = %q, want /v1/messages", captured.Path)
	}
	if captured.APIKey != "sk-test" {
		t.Errorf("x-api-key header = %q, want sk-test", captured.APIKey)
	}

	toolChoice, _ := captured.Body["tool_choice"].(map[string]any)
	if toolChoice["type"] != "tool" || toolChoice["name"] != "emit_result" {
		t.Errorf("tool_choice = %#v, want {type: tool, name: emit_result}", captured.Body["tool_choice"])
	}

	tools, _ := captured.Body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want exactly one tool", captured.Body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "emit_result" {
		t.Errorf("tools[0].name = %v, want emit_result", tool["name"])
	}
	var wantSchema any
	if err := json.Unmarshal(schema, &wantSchema); err != nil {
		t.Fatalf("unmarshal reference schema: %v", err)
	}
	if !reflect.DeepEqual(tool["input_schema"], wantSchema) {
		t.Errorf("tools[0].input_schema = %#v, want %#v (req.Schema verbatim)", tool["input_schema"], wantSchema)
	}

	if captured.Body["model"] != "claude-sonnet-5" {
		t.Errorf("model = %v, want claude-sonnet-5", captured.Body["model"])
	}
	if captured.Body["max_tokens"] != float64(2048) {
		t.Errorf("max_tokens = %v, want 2048 (default when req.MaxTokens is 0)", captured.Body["max_tokens"])
	}
	system, _ := captured.Body["system"].([]any)
	if len(system) != 1 {
		t.Fatalf("system = %#v, want one block", captured.Body["system"])
	}
	sysBlock, _ := system[0].(map[string]any)
	if sysBlock["text"] != req.System {
		t.Errorf("system[0].text = %v, want %q", sysBlock["text"], req.System)
	}
	messages, _ := captured.Body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one message", captured.Body["messages"])
	}
	msg, _ := messages[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("messages[0].role = %v, want user", msg["role"])
	}
	msgContent, _ := msg["content"].([]any)
	if len(msgContent) != 1 {
		t.Fatalf("messages[0].content = %#v, want one block", msg["content"])
	}
	msgBlock, _ := msgContent[0].(map[string]any)
	if msgBlock["text"] != req.User {
		t.Errorf("messages[0].content[0].text = %v, want %q", msgBlock["text"], req.User)
	}

	// --- Response mapping assertions ---
	if err := schemas.Validate("correction_result.v1", resp.JSON); err != nil {
		t.Fatalf("response JSON failed schema validation: %v\nJSON: %s", err, resp.JSON)
	}
	var got wantResult
	if err := json.Unmarshal(resp.JSON, &got); err != nil {
		t.Fatalf("unmarshal response JSON: %v", err)
	}
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	if want := "面白かったです"; got.Corrections[0].Replacement != want {
		t.Errorf("corrections[0].replacement = %q, want %q", got.Corrections[0].Replacement, want)
	}
	if resp.InputTokens != 210 {
		t.Errorf("InputTokens = %d, want 210", resp.InputTokens)
	}
	if resp.OutputTokens != 96 {
		t.Errorf("OutputTokens = %d, want 96", resp.OutputTokens)
	}
	if resp.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", resp.Provider)
	}
	if resp.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want claude-sonnet-5", resp.Model)
	}
	// RequestID is stamped by the observability decorator, not the
	// adapter — the adapter must leave it empty.
	if resp.RequestID != "" {
		t.Errorf("RequestID = %q, want empty (stamped by observability decorator)", resp.RequestID)
	}
}

func TestGenerateStructuredErrorsWhenNoToolUseBlock(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedTextOnlyResponse)
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL}, nil)
	_, err = gen.GenerateStructured(context.Background(), ai.StructuredRequest{
		System:     "system",
		User:       "user",
		SchemaName: "correction_result.v1",
		Schema:     schema,
	})
	if err == nil {
		t.Fatal("expected error for a response with no tool_use block, got nil")
	}
	if !strings.Contains(err.Error(), "tool_use") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "tool_use")
	}
}

func TestGenerateStructuredErrorsWhenToolUseBlockNamesWrongTool(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedWrongToolResponse)
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL}, nil)
	_, err = gen.GenerateStructured(context.Background(), ai.StructuredRequest{
		System:     "system",
		User:       "user",
		SchemaName: "correction_result.v1",
		Schema:     schema,
	})
	if err == nil {
		t.Fatal("expected error for a tool_use block naming a different tool, got nil")
	}
	if !strings.Contains(err.Error(), "tool_use") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "tool_use")
	}
}

func TestGenerateStructuredWrapsNon2xxHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(cannedAPIErrorResponse))
	}))
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL}, nil)
	resp, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{
		System:     "system",
		User:       "user",
		SchemaName: "correction_result.v1",
		Schema:     schema,
	})
	if err == nil {
		t.Fatal("expected error for a non-2xx response, got nil")
	}
	if !strings.Contains(err.Error(), "anthropic: messages.new") {
		t.Errorf("error = %q, want it wrapped with %q", err.Error(), "anthropic: messages.new")
	}
	// Provider/Model are known regardless of outcome, so the
	// observability decorator wrapping this call can still record which
	// provider/model a failed call went through.
	if resp.Provider != "anthropic" || resp.Model != "claude-sonnet-5" {
		t.Errorf("Provider/Model on error = %q/%q, want anthropic/claude-sonnet-5", resp.Provider, resp.Model)
	}
}

func TestGenerateStructuredMaxTokensPassedThrough(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedToolUseResponse)
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL}, nil)
	_, err = gen.GenerateStructured(context.Background(), ai.StructuredRequest{
		System:     "system",
		User:       "user",
		SchemaName: "correction_result.v1",
		Schema:     schema,
		MaxTokens:  512,
	})
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}
	if captured.Body["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want 512 (explicit req.MaxTokens, not the 2048 default)", captured.Body["max_tokens"])
	}
}

// fakeResolver is a minimal ai.ModelResolver test double whose answer
// can be changed between calls.
type fakeResolver struct{ model, effort string }

func (r *fakeResolver) Model(_ string) (string, string) { return r.model, r.effort }

// TestResolverOverrideReachesNextCallWithoutReconstruction is the task
// brief's Step 1 requirement, asserted directly against this adapter:
// construct ONE generator with a resolver, change what the resolver
// reports between two calls, and confirm the SECOND call actually used
// the new model — no second New() call anywhere in this test.
func TestResolverOverrideReachesNextCallWithoutReconstruction(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedToolUseResponse)
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	resolver := &fakeResolver{} // starts with no override
	gen := New(config.Anthropic{APIKey: "sk-test", Model: "claude-sonnet-5", BaseURL: srv.URL}, resolver)
	req := ai.StructuredRequest{System: "s", User: "u", Schema: schema}

	if _, err := gen.GenerateStructured(context.Background(), req); err != nil {
		t.Fatalf("GenerateStructured (before override): %v", err)
	}
	if got := captured.Body["model"]; got != "claude-sonnet-5" {
		t.Errorf("model before override = %v, want the config value claude-sonnet-5", got)
	}

	// Change what the resolver reports — simulating a /settings save —
	// WITHOUT touching gen itself.
	resolver.model = "claude-opus-5"

	if _, err := gen.GenerateStructured(context.Background(), req); err != nil {
		t.Fatalf("GenerateStructured (after override): %v", err)
	}
	if got := captured.Body["model"]; got != "claude-opus-5" {
		t.Errorf("model after override = %v, want claude-opus-5 — the SAME generator must pick up the new resolver value with no restart/reconstruction", got)
	}
}
