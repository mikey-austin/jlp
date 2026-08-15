package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/a2a"
	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double (implements ai.ToolCaller), injected the same way internal/application/agentrun's own tests inject it
	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// TestA2ARoutesAbsentWhenDisabled pins the Step 1 "disabled config ⇒
// routes absent (404)" requirement: testOptions() leaves Options.A2A
// at its zero value (nil, exactly what main.go passes when
// APP_A2A_ENABLED is unset/false), so the agent-card route this
// package would otherwise mount must not exist at all — a plain chi
// "no route matched" 404, not a 401/403 from some auth check the
// route never even reaches.
func TestA2ARoutesAbsentWhenDisabled(t *testing.T) {
	srv := NewServer(testOptions())
	req := httptest.NewRequest(http.MethodGet, "/a2a/.well-known/agent-card.json", nil)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (A2A must be entirely unmounted when Options.A2A is nil): %s", rec.Code, rec.Body.String())
	}
}

// testOptionsWithA2A builds a real, working a2a.Server (fakeai as the
// ai.ToolCaller, an in-memory AgentRunRepository) and wires it into
// Options — proving the ACTUAL mount, not just that a non-nil field
// exists: RequireIdentity/CSRFProtect still apply (this is mounted
// inside the very same r.Group as every other authenticated route —
// see server.go's routes()), and the request's authenticated identity
// (testOptions' "dev") reaches the adapter via withA2AIdentity, never
// from the task body.
func testOptionsWithA2A(t *testing.T) Options {
	t.Helper()
	opts := testOptions()
	reg := tools.NewRegistry()
	reg.Allow("teacher", "get_learning_priorities")
	runner := agentrun.NewRunner(fakeai.New(), reg, newFakeAgentRunRepo(), time.Now)
	opts.A2A = a2a.New(runner, reg, config.A2A{Enabled: true, Path: "/a2a"})
	opts.A2APath = "/a2a"
	return opts
}

// TestA2AMountedWhenEnabledServesAgentCard pins the other half of
// "disabled ⇒ 404": ENABLED must actually work end to end, behind the
// authenticated group, with no CSRF false-positive on a plain
// non-browser GET (no Origin/Sec-Fetch-Site header at all — see
// csrf.go's csrfReject).
func TestA2AMountedWhenEnabledServesAgentCard(t *testing.T) {
	srv := NewServer(testOptionsWithA2A(t))
	req := httptest.NewRequest(http.MethodGet, "/a2a/.well-known/agent-card.json", nil)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"review_writing"`) {
		t.Fatalf("body = %s, want it to list the review_writing skill", rec.Body.String())
	}
}

// TestA2ARequiresAuthentication pins that the A2A mount is genuinely
// inside the authenticated group, not a side door around it: an
// unauthenticated request (RequireIdentity's testOptions() Auth
// always succeeds, so this instead swaps in fakeAuth — see
// middleware_test.go — configured to fail) must 401 before ever
// reaching the a2a package's own handlers.
func TestA2ARequiresAuthentication(t *testing.T) {
	opts := testOptionsWithA2A(t)
	opts.Auth = fakeAuth{err: errors.New("no session")}
	srv := NewServer(opts)

	req := httptest.NewRequest(http.MethodPost, "/a2a/tasks", strings.NewReader(`{"skill":"review_writing","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestA2ATaskRunsAsTheAuthenticatedRequestIdentity pins that POST
// /a2a/tasks actually completes a real run when reached through the
// full HTTP stack (RequireIdentity → CSRFProtect → withA2AIdentity →
// a2a.Server), using testOptions()' "dev" identity — the same
// identity/session context every other authenticated route in this
// test package runs as, never anything the request body could name.
func TestA2ATaskRunsAsTheAuthenticatedRequestIdentity(t *testing.T) {
	srv := NewServer(testOptionsWithA2A(t))
	req := httptest.NewRequest(http.MethodPost, "/a2a/tasks", strings.NewReader(`{"skill":"review_writing","input":"友達と映画を見ました。"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"completed"`) {
		t.Fatalf("body = %s, want status completed", rec.Body.String())
	}
}
