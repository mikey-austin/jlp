// Package a2a implements the A2A (agent-to-agent) protocol adapter
// (PRD §29/§30): it exposes JLP's own agents — a conversational tutor
// (teacher), the Writing Reviewer (teacher), the Learner Analyst
// (summary), and the Lesson Planner (lesson) — as A2A "skills" a remote
// agent can discover (GET .well-known/agent-card.json) and invoke over
// the protocol's JSON-RPC 2.0 binding.
//
// Protocol: A2A **v1.0**, per the specification at
// https://a2a-protocol.org/latest/specification/ (checked 2026-08-16:
// "The latest released version is 1.0.0") and cross-checked against the
// official client, @a2a-js/sdk@1.0.1, which is what actually has to
// parse what this package emits. types.go carries the data model and
// the three protobuf-JSON details that a from-memory implementation
// reliably gets wrong; jsonrpc.go carries the envelope and the method
// names. This package previously served a bespoke REST shape that no
// A2A client could speak (POST /tasks with {skill,input}); that shape
// is retired, not maintained alongside — see docs/api/a2a.md.
//
// Rule 13 is the point of this whole package (PRD §30): "agents own
// reasoning; application services own state. A remote agent should not
// become a second source of truth for learner state." A remote caller
// gets no capability a local agent-run doesn't already have — every
// task this Server runs goes through the EXACT SAME
// application/agentrun.Runner + internal/tools.Registry allowlists any
// local caller (e.g. the agentic teacher, Phase 4 Task 2) goes
// through. This is enforced by construction, not by convention: Server
// holds a *agentrun.Runner and a *tools.Registry and NOTHING else that
// could reach learner state — no storage.XxxRepository, no
// application-layer service, no direct AI generator. If a skill ever
// seemed to need a tool its underlying agent isn't Allow()ed (in
// cmd/jlp/main.go) to call, the fix is to widen that allowlist at the
// one place it's declared — never to give this package its own side
// door around it. See docs/api/a2a.md for the human-readable version
// of this contract.
//
// Identity: single-learner LAN posture. This package never reads an
// identity from a task payload — WithIdentity/IdentityFrom below are
// the ONLY way a request's identity reaches a handler here, and the
// caller that sets it (internal/adapters/http/server.go, which mounts
// Routes() inside its already-authenticated route group) always reads
// it from the SAME request-scoped identity every other authenticated
// route uses (RequireIdentity + the configured auth.Authenticator),
// never from anything the HTTP client's body controls. See
// TestSendMessageIgnoresForgedIdentityInParams.
package a2a

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// Server is the A2A adapter. Its only two collaborators —
// *agentrun.Runner and *tools.Registry — are exactly what the package
// doc comment's Rule 13 argument depends on: there is no field here a
// future change could wire a repository into without it being an
// obviously out-of-place addition to this struct.
type Server struct {
	runner agentRunner
	// consulted records which specialists each in-flight run has already
	// asked, so a coordinator cannot ask the same one twice. Guarded by
	// mu, and cleared when the run it belongs to ends — see endRun.
	consulted map[string]map[string]bool
	// delegated collects the tool calls each in-flight run's SPECIALISTS
	// made, keyed by the consulting run's id. A coordinator's own tool
	// calls are all consult_specialist, which renders as nothing; the
	// structured data a client can draw was read one level down, and
	// without this it dies with the delegated run. Guarded by mu, and
	// handed to the finished task by endRun.
	delegated map[string][]agentrun.ToolCall
	// asyncSlots caps background runs in flight — see async.go. Buffered
	// to maxConcurrentAsyncRuns; a send that would block means we are at
	// the cap.
	asyncSlots chan struct{}
	reg        *tools.Registry
	cfg        config.A2A

	// mu/tasks/order back the GetTask method: a small cache of tasks THIS
	// process produced — never a second source of truth for the run
	// itself, which remains the agent_runs row application/agentrun.Runner
	// already persisted (viewable at /ai/agents). A process restart
	// loses this cache; the underlying agent_runs row does not.
	//
	// A background run (async.go) additionally lives here while it is
	// still going, which is what a returnImmediately caller polls. That
	// makes the map process state as well as a cache — but not a source
	// of truth even then: losing it to a restart loses the ability to
	// poll a run, not the run's own record.
	//
	// order is the insertion order of the ids in tasks, so the cache can
	// be capped at maxCachedTasks: every Task holds a full model
	// response, and the conversational default skill (task.go's
	// defaultSkill) means a chat client can create them indefinitely, so
	// an uncapped map is a slow leak in a long-running process. Evicting
	// the oldest is safe precisely BECAUSE this is a cache and not the
	// source of truth: a GetTask for an evicted id answers "task not
	// found", the same answer a process restart already gives, while the
	// agent_runs/tool_calls trace remains at /ai/agents either way.
	mu    sync.RWMutex
	tasks map[string]taskRecord
	order []string
}

// maxCachedTasks caps Server.tasks. Sized for the single-learner LAN
// posture this whole adapter is built for — far more than any real
// conversation needs to page back through, small enough that the
// worst-case retained model output stays bounded.
const maxCachedTasks = 512

// New wires an A2A Server over runner/reg — the same instances
// cmd/jlp/main.go already constructed for the local agent-run path —
// and cfg (APP_A2A_ENABLED/APP_A2A_PATH). New never fails: like every
// other optional feature in this codebase (Anki, MQTT, Summary), main
// only calls New at all when cfg.Enabled is true; internal/adapters/http
// only mounts Routes() when it was given a non-nil Server.
// agentRunner is the runner seam.
//
// An interface rather than *agentrun.Runner so a test can observe WHAT
// this adapter asks to run — which agent, whose identity, what turn
// budget. Those are the properties delegation is about: a delegated run
// executing as the specialist rather than the caller is not something to
// verify by reading the code twice.
//
// cmd/jlp passes the real *agentrun.Runner, which satisfies it.
type agentRunner interface {
	Run(ctx context.Context, in agentrun.RunInput) (agentrun.RunOutput, error)
}

func New(runner agentRunner, reg *tools.Registry, cfg config.A2A) *Server {
	return &Server{
		runner:     runner,
		reg:        reg,
		cfg:        cfg,
		tasks:      make(map[string]taskRecord),
		consulted:  make(map[string]map[string]bool),
		delegated:  make(map[string][]agentrun.ToolCall),
		asyncSlots: make(chan struct{}, maxConcurrentAsyncRuns),
	}
}

// remember caches task under id as identity's, evicting the oldest
// entry once the cache is full. Callers must not hold s.mu.
func (s *Server) remember(id string, rec taskRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tasks[id]; !exists {
		s.order = append(s.order, id)
	}
	s.tasks[id] = rec
	for len(s.order) > maxCachedTasks {
		delete(s.tasks, s.order[0])
		s.order = s.order[1:]
	}
}

// settle records a background run's terminal task, unless the task has
// already reached a terminal state — which means CancelTask got there
// first, and the run's own answer (a context-cancelled error) is a
// consequence of that cancel rather than news about the work.
//
// The check and the write are one critical section: a cancel arriving
// between a read and a write would otherwise be silently overwritten.
func (s *Server) settle(id string, task Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, found := s.tasks[id]
	if !found {
		return // evicted mid-run; nothing left to update
	}
	if isTerminalState(rec.Task.Status.State) {
		return
	}
	rec.Task = task
	rec.cancel = nil // the run is over; nothing left to cancel
	s.tasks[id] = rec
}

// rpcRoute is the JSON-RPC endpoint's path, relative to the adapter's
// mount prefix. "/v1" matches the spec's own worked examples
// ("https://api.example.com/a2a/v1") and, more usefully, keeps the
// binding's major version in the URL rather than in a header nobody
// reads. card.go's interfaceURL is what advertises it; a client never
// has to guess it.
const rpcRoute = "/v1"

// Routes returns the adapter's own sub-router, relative to whatever
// prefix the caller mounts it under (cfg.Path, by convention — see
// internal/adapters/http/server.go):
//
//	GET  /.well-known/agent-card.json — the agent card (card.go)
//	POST /v1                          — the JSON-RPC 2.0 endpoint (jsonrpc.go)
//
// Two routes, not five: in A2A, every operation is a method on the one
// JSON-RPC endpoint. The bespoke POST /tasks + GET /tasks/{id} pair
// this replaced is gone rather than kept alongside — it had no external
// consumers and no A2A client could speak it.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/.well-known/agent-card.json", s.handleAgentCard)
	r.Post(rpcRoute, s.handleRPC)
	return r
}

// identityCtxKey is an unexported type so no other package can collide
// with this context key by accident (the standard Go context-key
// idiom).
type identityCtxKey struct{}

// WithIdentity returns a context carrying id as the request's
// authenticated identity — called exactly once per request, by
// whatever mounts Routes() (internal/adapters/http/server.go), never
// by a handler in this package. This is the ONLY path an identity
// reaches this package's handlers through; see the package doc
// comment's Identity section.
func WithIdentity(ctx context.Context, id learner.IdentityID) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFrom reads back the identity WithIdentity attached, if any.
func IdentityFrom(ctx context.Context) (learner.IdentityID, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(learner.IdentityID)
	return id, ok
}

// writeJSON encodes v as the response body with the given status —
// this package's own copy of internal/adapters/http's identical
// helper: a2a deliberately shares no code with that package (nothing
// to import that would blur "this adapter's only collaborators are
// runner/reg" back into something less obviously true by inspection).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("a2a: write response", "err", err)
	}
}
