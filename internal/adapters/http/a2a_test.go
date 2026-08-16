package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

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
	// The card's advertised JSON-RPC endpoint must reflect the path this
	// package actually mounted the adapter at — an A2A client reads the
	// endpoint from here and never has to know APP_A2A_PATH itself.
	if !strings.Contains(rec.Body.String(), `"url":"http://example.com/a2a/v1"`) {
		t.Fatalf("body = %s, want supportedInterfaces to advertise the mounted JSON-RPC endpoint", rec.Body.String())
	}
}

// rpcBody is one JSON-RPC 2.0 request against the adapter's endpoint —
// the only POST shape it serves now that the bespoke REST routes are
// retired (see docs/api/a2a.md).
const rpcBody = `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":` +
	`{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"友達と映画を見ました。"}]}}}`

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

	req := httptest.NewRequest(http.MethodPost, "/a2a/v1", strings.NewReader(rpcBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestA2ATaskRunsAsTheAuthenticatedRequestIdentity pins that a
// SendMessage call actually completes a real run when reached through
// the full HTTP stack (RequireIdentity → CSRFProtect → withA2AIdentity
// → a2a.Server), using testOptions()' "dev" identity — the same
// identity/session context every other authenticated route in this test
// package runs as, never anything the request params could name.
//
// This also pins the CSRF posture the mount depends on: a JSON-RPC POST
// from a non-browser A2A client sends no Origin/Sec-Fetch-Site header
// at all, which csrf.go's csrfReject never rejects (see server.go's
// comment at the mount site). If that ever changed, this test fails
// here rather than silently in production.
func TestA2ATaskRunsAsTheAuthenticatedRequestIdentity(t *testing.T) {
	srv := NewServer(testOptionsWithA2A(t))
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1", strings.NewReader(rpcBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"TASK_STATE_COMPLETED"`) {
		t.Fatalf("body = %s, want a completed Task", rec.Body.String())
	}
}

// TestA2ARPCEndpointAbsentWhenDisabled is the other half of
// TestA2ARoutesAbsentWhenDisabled: it's the JSON-RPC endpoint, not the
// card, that actually does anything, so "dormant unless
// APP_A2A_ENABLED" has to hold for that route specifically.
func TestA2ARPCEndpointAbsentWhenDisabled(t *testing.T) {
	srv := NewServer(testOptions())
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1", strings.NewReader(rpcBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (A2A must be entirely unmounted when Options.A2A is nil): %s", rec.Code, rec.Body.String())
	}
}

// TestMountA2ARecoversAndReportsClearError is the Task 3 code review's
// fix-round-2 pin (Minor 6, part 2): config.validate()'s structural +
// collision checks (internal/config) are a config-time safety net, not
// a compile-time guarantee — a route added to server.go later without
// a matching entry in config's a2aReservedPathPrefixes could still
// make r.Mount panic at boot with chi's own, unhelpful internal
// message. mountA2A must turn THAT panic into a single, clearly worded
// one naming APP_A2A_PATH, so an operator sees an actionable message
// instead of a bare chi stack trace.
//
// This drives mountA2A directly against a bespoke chi.Router (not the
// full production route table config.validate() already guards) so
// the test is independent of which specific collisions
// a2aReservedPathPrefixes does or doesn't already know about — see
// config.A2A.Path's own doc comment on why this recover exists
// ALONGSIDE that config-time check, not instead of it.
func TestMountA2ARecoversAndReportsClearError(t *testing.T) {
	r := chi.NewRouter()
	r.Mount("/collide", http.NotFoundHandler()) // first mount claims the path

	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("expected mountA2A to panic (recovering chi's own panic into a clearer one), got no panic at all")
		}
		msg, ok := p.(string)
		if !ok {
			t.Fatalf("panic value = %#v (%T), want a string", p, p)
		}
		if !strings.Contains(msg, "APP_A2A_PATH") || !strings.Contains(msg, `"/collide"`) {
			t.Fatalf("panic message = %q, want it to name APP_A2A_PATH and the offending path", msg)
		}
	}()
	mountA2A(r, "/collide", http.NotFoundHandler()) // second mount at the SAME path: chi panics ("attempting to Mount() a handler on an existing path")
}
