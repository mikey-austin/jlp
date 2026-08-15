package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// echoTool records the identity/sessionID/args it was actually called
// with, so a test can assert on what Invoke handed it independently of
// whatever the model's JSON arguments claimed.
type echoCall struct {
	identity  learner.IdentityID
	sessionID *session.ID
	args      json.RawMessage
}

func newEchoTool(name string, calls *[]echoCall) tools.Tool {
	return tools.Tool{
		Def: ai.ToolDef{Name: name, Description: "echoes the identity it was called with", Schema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(_ context.Context, identity learner.IdentityID, sessionID *session.ID, args json.RawMessage) (string, error) {
			*calls = append(*calls, echoCall{identity: identity, sessionID: sessionID, args: args})
			return `{"ok":true}`, nil
		},
	}
}

func TestInvokeRefusesUnknownTool(t *testing.T) {
	r := tools.NewRegistry()
	r.Allow("teacher", "get_active_session")

	result := r.Invoke(context.Background(), "teacher", "mikey", nil, ai.ToolInvocation{ID: "1", Name: "no_such_tool"})

	if !result.IsError {
		t.Fatal("Invoke() IsError = false, want true for an unknown tool name")
	}
	if result.ID != "1" {
		t.Fatalf("Invoke() ID = %q, want %q (echoed from the invocation)", result.ID, "1")
	}
}

func TestInvokeRefusesToolNotAllowedForAgent(t *testing.T) {
	r := tools.NewRegistry()
	var calls []echoCall
	r.Register(newEchoTool("get_active_session", &calls))
	// "teacher" is never Allow()ed to call it — only a different agent is.
	r.Allow("drill", "get_active_session")

	result := r.Invoke(context.Background(), "teacher", "mikey", nil, ai.ToolInvocation{ID: "1", Name: "get_active_session"})

	if !result.IsError {
		t.Fatal("Invoke() IsError = false, want true for a tool this agent isn't allowed to call")
	}
	if len(calls) != 0 {
		t.Fatalf("handler was called %d times, want 0 — a disallowed tool must never run", len(calls))
	}
}

// TestInvokeIgnoresForgedIdentityInArguments pins the single most
// important security property of this package (see registry.go's doc
// comment): a tool handler's identity comes from Invoke's own
// identity parameter, never from the model's JSON arguments — even
// when those arguments carry a field literally named "identity"
// naming someone else.
func TestInvokeIgnoresForgedIdentityInArguments(t *testing.T) {
	r := tools.NewRegistry()
	var calls []echoCall
	r.Register(newEchoTool("get_active_session", &calls))
	r.Allow("teacher", "get_active_session")

	forgedArgs := json.RawMessage(`{"identity":"someone-else","session_id":"forged-session"}`)
	result := r.Invoke(context.Background(), "teacher", "real-caller", nil, ai.ToolInvocation{ID: "1", Name: "get_active_session", Arguments: forgedArgs})

	if result.IsError {
		t.Fatalf("Invoke() IsError = true, want false: %s", result.Content)
	}
	if len(calls) != 1 {
		t.Fatalf("handler was called %d times, want 1", len(calls))
	}
	if calls[0].identity != "real-caller" {
		t.Fatalf("handler identity = %q, want %q (the caller's real identity, not the forged JSON field)", calls[0].identity, "real-caller")
	}
}

func TestInvokeSucceedsForAllowedRegisteredTool(t *testing.T) {
	r := tools.NewRegistry()
	var calls []echoCall
	r.Register(newEchoTool("get_active_session", &calls))
	r.Allow("teacher", "get_active_session")

	result := r.Invoke(context.Background(), "teacher", "mikey", nil, ai.ToolInvocation{ID: "42", Name: "get_active_session", Arguments: json.RawMessage(`{}`)})

	if result.IsError {
		t.Fatalf("Invoke() IsError = true, want false: %s", result.Content)
	}
	if result.Content != `{"ok":true}` {
		t.Fatalf("Invoke() Content = %q, want the handler's own output", result.Content)
	}
	if result.ID != "42" {
		t.Fatalf("Invoke() ID = %q, want %q", result.ID, "42")
	}
}

func TestInvokeNeverPanicsOnHandlerPanic(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(tools.Tool{
		Def: ai.ToolDef{Name: "panics"},
		Handler: func(context.Context, learner.IdentityID, *session.ID, json.RawMessage) (string, error) {
			panic("boom")
		},
	})
	r.Allow("teacher", "panics")

	result := r.Invoke(context.Background(), "teacher", "mikey", nil, ai.ToolInvocation{ID: "1", Name: "panics"})

	if !result.IsError {
		t.Fatal("Invoke() IsError = false, want true after a handler panic")
	}
}

func TestInvokeReturnsHandlerErrorAsToolError(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(tools.Tool{
		Def: ai.ToolDef{Name: "fails"},
		Handler: func(context.Context, learner.IdentityID, *session.ID, json.RawMessage) (string, error) {
			return "", errHandler
		},
	})
	r.Allow("teacher", "fails")

	result := r.Invoke(context.Background(), "teacher", "mikey", nil, ai.ToolInvocation{ID: "1", Name: "fails"})

	if !result.IsError {
		t.Fatal("Invoke() IsError = false, want true when the handler returns an error")
	}
	if result.Content != errHandler.Error() {
		t.Fatalf("Invoke() Content = %q, want the handler's error message %q", result.Content, errHandler.Error())
	}
}

func TestDefsForReturnsOnlyAllowedRegisteredToolsSortedByName(t *testing.T) {
	r := tools.NewRegistry()
	var calls []echoCall
	r.Register(newEchoTool("zebra", &calls))
	r.Register(newEchoTool("alpha", &calls))
	r.Register(newEchoTool("unallowed", &calls))
	r.Allow("teacher", "zebra", "alpha")
	// "ghost" is allowed but never registered — must be silently omitted.
	r.Allow("teacher", "ghost")

	defs := r.DefsFor("teacher")

	if len(defs) != 2 {
		t.Fatalf("DefsFor() returned %d defs, want 2: %+v", len(defs), defs)
	}
	if defs[0].Name != "alpha" || defs[1].Name != "zebra" {
		t.Fatalf("DefsFor() = %v, want [alpha zebra] (sorted)", []string{defs[0].Name, defs[1].Name})
	}
}

func TestDefsForUnknownAgentReturnsEmpty(t *testing.T) {
	r := tools.NewRegistry()
	if got := r.DefsFor("nobody"); len(got) != 0 {
		t.Fatalf("DefsFor(unknown agent) = %v, want empty", got)
	}
}

var errHandler = &stubError{"handler exploded"}

type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }
