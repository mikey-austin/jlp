package httpx

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// agentRunsPageLimit caps the /ai/agents list at the identity's most
// recent 50 runs — an audit view, not a paginated archive, same
// convention as aiRequestsPageLimit in ai.go.
const agentRunsPageLimit = 50

// agentRunView is one storage.AgentRun as the /ai/agents list and the
// detail page's header render it. Duration is EndedAt.Sub(StartedAt)
// for a finished run, zero for one still "running" — the template
// renders that as an em dash rather than a bogus "0s".
type agentRunView struct {
	ID, Agent, PromptName, PromptVersion, Status, Error string
	Turns                                               int
	StartedAt, EndedAt                                  time.Time
	Duration                                            time.Duration
}

func toAgentRunView(run storage.AgentRun) agentRunView {
	view := agentRunView{
		ID:            run.ID,
		Agent:         run.Agent,
		PromptName:    run.PromptName,
		PromptVersion: run.PromptVersion,
		Status:        run.Status,
		Error:         run.Error,
		Turns:         run.Turns,
		StartedAt:     run.StartedAt,
		EndedAt:       run.EndedAt,
	}
	if !run.EndedAt.IsZero() {
		view.Duration = run.EndedAt.Sub(run.StartedAt)
	}
	return view
}

func toAgentRunViews(runs []storage.AgentRun) []agentRunView {
	views := make([]agentRunView, 0, len(runs))
	for _, r := range runs {
		views = append(views, toAgentRunView(r))
	}
	return views
}

// toolCallView is one storage.ToolCall as the detail page's trace
// renders it. Arguments/Result are model-generated text — the
// template MUST render them escaped (Go's html/template does this by
// default; see web/templates/agent_run_detail.html.tmpl, which never
// wraps either in template.HTML).
type toolCallView struct {
	ToolName, Arguments, Result string
	IsError                     bool
	DurationMS                  int
	CreatedAt                   time.Time
}

func toToolCallView(c storage.ToolCall) toolCallView {
	return toolCallView{
		ToolName:   c.ToolName,
		Arguments:  c.Arguments,
		Result:     c.Result,
		IsError:    c.IsError,
		DurationMS: c.DurationMS,
		CreatedAt:  c.CreatedAt,
	}
}

// traceEventView is one entry in the detail page's chronological trace
// — either a model turn's text (Kind "turn") or a tool call's
// arguments/result/error (Kind "tool_call") — interleaved by
// CreatedAt so the page shows the conversation the way it actually
// happened, one event after another, instead of two disconnected
// lists. This is what closes the brief's "each model turn ... each
// tool call" requirement (PRD §50): Task 2's first pass only persisted
// the FINAL turn's text (AgentRun.Output), so a run that failed
// MaxTurns — exactly the case someone opens this page to diagnose —
// showed no model text at all; every turn is now recorded via
// AgentRunRepository.RecordTurn and rendered here in sequence.
type traceEventView struct {
	Kind       string // "turn" | "tool_call"
	TurnNumber int
	Text       string // Kind == "turn"; empty when the model produced no prose that turn
	ToolCall   toolCallView
	CreatedAt  time.Time
}

func toTraceEvents(turns []storage.AgentTurn, calls []storage.ToolCall) []traceEventView {
	events := make([]traceEventView, 0, len(turns)+len(calls))
	for _, t := range turns {
		events = append(events, traceEventView{Kind: "turn", TurnNumber: t.TurnNumber, Text: t.Text, CreatedAt: t.CreatedAt})
	}
	for _, c := range calls {
		events = append(events, traceEventView{Kind: "tool_call", ToolCall: toToolCallView(c), CreatedAt: c.CreatedAt})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt.Before(events[j].CreatedAt) })
	return events
}

// agentRunsList handles GET /ai/agents: the trace list (PRD §27/§50),
// linked from /ai rather than a new top-level nav item (see
// layout.html.tmpl) — agent runs are an AI-observability concern, the
// same page family aiRequests already lives in.
func (s *Server) agentRunsList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	runs, err := s.opts.AgentRuns.List(r.Context(), ident.ID, agentRunsPageLimit)
	if err != nil {
		http.Error(w, "could not load agent runs", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "agent_runs", map[string]any{
		"Title":    "Agent Runs",
		"Identity": ident,
		"Runs":     toAgentRunViews(runs),
	})
}

// agentRunsDetail handles GET /ai/agents/{id}: one run's full trace —
// input context (System/Input), every model turn interleaved with
// every tool call it made (arguments/result/error), and the final
// output, exactly what PRD §50 draws for the agent-run trace viewer.
// Identity-scoped via storage.AgentRunRepository.Get, same
// cross-identity-miss-is-404 convention every other detail page in
// this package uses (see lessonsDetail).
func (s *Server) agentRunsDetail(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")

	run, calls, turns, err := s.opts.AgentRuns.Get(r.Context(), ident.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load agent run", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "agent_run_detail", map[string]any{
		"Title":       "Agent Run",
		"Identity":    ident,
		"Run":         toAgentRunView(run),
		"System":      run.System,
		"Input":       run.Input,
		"Output":      run.Output,
		"TraceEvents": toTraceEvents(turns, calls),
	})
}
