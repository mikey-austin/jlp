package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

// cannedChatResponse is the exact Ollama /api/chat response shape from
// the task brief: message.content is a JSON *string* (the model's
// generated text), sitting alongside token counts and the model that
// actually served the request.
const cannedChatResponse = `{"message":{"content":"{\"corrections\":[{\"original\":\"面白いでした\",\"replacement\":\"面白かったです\",\"type\":\"conjugation\",\"severity\":\"incorrect\",\"explanation\":{\"ja\":\"×\",\"en\":\"x\"}}]}"},
 "prompt_eval_count":140,"eval_count":60,"model":"qwen3:4b"}`

// cannedInvalidContentResponse has message.content that is NOT valid
// JSON at all — the adapter must still pass it through as raw bytes,
// since schema validation belongs to the agent layer, not the adapter
// (see the task brief's Step 1).
const cannedInvalidContentResponse = `{"message":{"content":"not json at all"},
 "prompt_eval_count":10,"eval_count":5,"model":"qwen3:4b"}`

// requestAssertion is a captured, decoded copy of the single HTTP
// request the adapter sent, so tests can inspect it after the call
// completes — mirrors adapters/anthropic's test style.
type requestAssertion struct {
	Path   string
	Method string
	Body   map[string]any
}

func newTestServer(t *testing.T, captured *requestAssertion, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Path = r.URL.Path
		captured.Method = r.Method

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

func TestGenerateStructuredSendsWireContractAndParsesResult(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedChatResponse)
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Ollama{URL: srv.URL, Model: "qwen3:4b"})
	req := ai.StructuredRequest{
		PromptName: "teacher.feedback",
		System:     "You are a Japanese writing teacher.",
		User:       "友達と映画を見ました。とても面白いでした。",
		SchemaName: "correction_result.v1",
		Schema:     schema,
		Agent:      "teacher",
	}

	resp, err := gen.GenerateStructured(context.Background(), req)
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v", err)
	}

	// --- Wire-level assertions on the single outgoing request ---
	if captured.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.Method)
	}
	if captured.Path != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", captured.Path)
	}
	if captured.Body["model"] != "qwen3:4b" {
		t.Errorf("model = %v, want qwen3:4b", captured.Body["model"])
	}
	if captured.Body["stream"] != false {
		t.Errorf("stream = %v, want false", captured.Body["stream"])
	}

	messages, _ := captured.Body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want exactly 2 (system, user)", captured.Body["messages"])
	}
	sysMsg, _ := messages[0].(map[string]any)
	if sysMsg["role"] != "system" || sysMsg["content"] != req.System {
		t.Errorf("messages[0] = %#v, want {role: system, content: %q}", sysMsg, req.System)
	}
	userMsg, _ := messages[1].(map[string]any)
	if userMsg["role"] != "user" || userMsg["content"] != req.User {
		t.Errorf("messages[1] = %#v, want {role: user, content: %q}", userMsg, req.User)
	}

	var wantFormat any
	if err := json.Unmarshal(schema, &wantFormat); err != nil {
		t.Fatalf("unmarshal reference schema: %v", err)
	}
	gotFormat, ok := captured.Body["format"]
	if !ok {
		t.Fatalf("request body has no %q field: %#v", "format", captured.Body)
	}
	gotJSON, _ := json.Marshal(gotFormat)
	wantJSON, _ := json.Marshal(wantFormat)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("format = %s, want %s (req.Schema verbatim as a JSON object)", gotJSON, wantJSON)
	}

	// --- Response mapping assertions ---
	if err := schemas.Validate("correction_result.v1", resp.JSON); err != nil {
		t.Fatalf("response JSON failed schema validation: %v\nJSON: %s", err, resp.JSON)
	}
	var got struct {
		Corrections []struct {
			Replacement string `json:"replacement"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(resp.JSON, &got); err != nil {
		t.Fatalf("unmarshal response JSON: %v", err)
	}
	if len(got.Corrections) != 1 {
		t.Fatalf("Corrections = %+v, want exactly 1", got.Corrections)
	}
	if want := "面白かったです"; got.Corrections[0].Replacement != want {
		t.Errorf("corrections[0].replacement = %q, want %q", got.Corrections[0].Replacement, want)
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q, want ollama", resp.Provider)
	}
	if resp.Model != "qwen3:4b" {
		t.Errorf("Model = %q, want qwen3:4b (from response body, not just cfg.Model)", resp.Model)
	}
	if resp.InputTokens != 140 {
		t.Errorf("InputTokens = %d, want 140 (prompt_eval_count)", resp.InputTokens)
	}
	if resp.OutputTokens != 60 {
		t.Errorf("OutputTokens = %d, want 60 (eval_count)", resp.OutputTokens)
	}
	// RequestID is stamped by the observability decorator, not the
	// adapter — the adapter must leave it empty.
	if resp.RequestID != "" {
		t.Errorf("RequestID = %q, want empty (stamped by observability decorator)", resp.RequestID)
	}
}

func TestGenerateStructuredPassesThroughInvalidContentJSONRaw(t *testing.T) {
	// Schema validation is the agent layer's job, not the adapter's: an
	// Ollama model that ignores the "format" instruction and returns
	// prose instead of JSON must still come back as bytes, not an
	// adapter-level error.
	var captured requestAssertion
	srv := newTestServer(t, &captured, cannedInvalidContentResponse)
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Ollama{URL: srv.URL, Model: "qwen3:4b"})
	resp, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{
		System:     "system",
		User:       "user",
		SchemaName: "correction_result.v1",
		Schema:     schema,
	})
	if err != nil {
		t.Fatalf("GenerateStructured returned error: %v (adapter must pass invalid content through, not fail)", err)
	}
	if string(resp.JSON) != "not json at all" {
		t.Errorf("JSON = %q, want the raw message.content bytes %q", resp.JSON, "not json at all")
	}
}

func TestGenerateStructuredWrapsNon2xxHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"model is loading"}`))
	}))
	defer srv.Close()

	schema, err := schemas.Get("correction_result.v1")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}

	gen := New(config.Ollama{URL: srv.URL, Model: "qwen3:4b"})
	resp, err := gen.GenerateStructured(context.Background(), ai.StructuredRequest{
		System:     "system",
		User:       "user",
		SchemaName: "correction_result.v1",
		Schema:     schema,
	})
	if err == nil {
		t.Fatal("expected error for a non-2xx response, got nil")
	}
	if !strings.Contains(err.Error(), "ollama") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "ollama")
	}
	// Provider/Model are known regardless of outcome, so the
	// observability decorator wrapping this call can still record which
	// provider/model a failed call went through.
	if resp.Provider != "ollama" || resp.Model != "qwen3:4b" {
		t.Errorf("Provider/Model on error = %q/%q, want ollama/qwen3:4b", resp.Provider, resp.Model)
	}
}
