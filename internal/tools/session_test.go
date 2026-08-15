package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/tools"
)

const testIdentity = learner.IdentityID("test-learner-a")
const otherIdentity = learner.IdentityID("test-learner-b")

func TestGetActiveSessionReturnsMostRecentlyUpdated(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := sessions.NewService(repo)
	older := domsession.Session{ID: "s-old", IdentityID: testIdentity, Title: "old", UpdatedAt: time.Now().Add(-time.Hour)}
	newer := domsession.Session{ID: "s-new", IdentityID: testIdentity, Title: "new", UpdatedAt: time.Now()}
	if err := repo.Create(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(context.Background(), newer); err != nil {
		t.Fatal(err)
	}

	tool := findTool(t, tools.SessionTools(svc), "get_active_session")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["id"] != "s-new" {
		t.Fatalf("got id %v, want s-new (most recently updated): output=%s", got["id"], out)
	}
}

func TestGetActiveSessionNoSessionsReturnsInactive(t *testing.T) {
	svc := sessions.NewService(newFakeSessionRepo())
	tool := findTool(t, tools.SessionTools(svc), "get_active_session")

	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	if out != `{"active":false}` {
		t.Fatalf("Handler() = %q, want {\"active\":false}", out)
	}
}

func TestGetActiveSessionIsIdentityScoped(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := sessions.NewService(repo)
	if err := repo.Create(context.Background(), domsession.Session{ID: "s-a", IdentityID: otherIdentity, Title: "not yours", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, tools.SessionTools(svc), "get_active_session")

	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	if out != `{"active":false}` {
		t.Fatalf("Handler() = %q, want {\"active\":false} — another identity's session must not leak", out)
	}
}

func TestGetSessionContextDelegatesAndReturnsCompactShape(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := sessions.NewService(repo)
	s := domsession.Session{ID: "s-1", IdentityID: testIdentity, Title: "旅行について", Purpose: "diary", UpdatedAt: time.Now()}
	if err := repo.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, tools.SessionTools(svc), "get_session_context")

	out, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"session_id":"s-1"}`))
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["title"] != "旅行について" || got["purpose"] != "diary" {
		t.Fatalf("got %v, want title/purpose from the session: output=%s", got, out)
	}
	// No IdentityID or CreatedAt field — compact, model-facing shape.
	if _, has := got["identity_id"]; has {
		t.Fatalf("output leaks identity_id, want it omitted: %s", out)
	}
}

func TestGetSessionContextWrongIdentityMisses(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := sessions.NewService(repo)
	if err := repo.Create(context.Background(), domsession.Session{ID: "s-1", IdentityID: otherIdentity, Title: "not yours", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	tool := findTool(t, tools.SessionTools(svc), "get_session_context")

	_, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"session_id":"s-1"}`))
	if err == nil {
		t.Fatal("Handler() err = nil, want an error — a different identity's session must not be readable")
	}
}

func TestGetSessionContextMissingArgIsAnError(t *testing.T) {
	svc := sessions.NewService(newFakeSessionRepo())
	tool := findTool(t, tools.SessionTools(svc), "get_session_context")

	_, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Handler() err = nil, want an error when session_id is missing")
	}
}

// findTool locates a Tool by name among ts, failing the test if it's
// absent — every group-constructor test uses this to grab the one
// tool it's testing without hard-coding a slice index.
func findTool(t *testing.T, ts []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tl := range ts {
		if tl.Def.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool named %q among %d tools", name, len(ts))
	return tools.Tool{}
}
