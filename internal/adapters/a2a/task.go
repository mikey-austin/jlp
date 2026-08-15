package a2a

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// skillDef is everything one A2A skill needs to drive an
// agentrun.RunInput: which local agent it maps onto (and therefore
// which internal/tools.Registry allowlist governs it — see card.go's
// SkillCard.Tools) and what system prompt frames the task. PromptName/
// PromptVersion are metadata only (agent_runs columns, for the
// /ai/agents trace viewer) — deliberately new, "a2a.*" names rather
// than reusing e.g. internal/agent/teacher's private "teacher.agentic"
// prompt/template: this package renders no internal/prompts template
// of its own (Rule 3 keeps that machinery inside internal/agent/*),
// it just hands the runner a System string directly, same shape, own
// identity in the trace.
type skillDef struct {
	Name, Description string
	// Agent is the internal/tools.Registry agent name this skill runs
	// as — MUST match one already Allow()ed in cmd/jlp/main.go (today:
	// only "teacher"). A skill mapped onto an agent with no allowlist
	// entry still runs (see Run's own tolerance of an empty
	// reg.DefsFor result) — it just can't call any tool, which is
	// itself a correct, unwidened permission boundary, not a bug in
	// this file. See docs/api/a2a.md's "known consequence" note.
	Agent                     string
	PromptName, PromptVersion string
	// SystemWithTools/SystemWithoutTools are the two system prompts
	// systemFor chooses between, at request time, based on whether
	// Agent CURRENTLY has any tool Allow()ed (reg.DefsFor(Agent) — the
	// exact same live signal card.go's SkillCard.Tools already reads).
	// Kept as two separate, fully-written prompts rather than one
	// templated string: Task 3 code review Minor 1 flagged
	// SystemWithTools' "investigate using your available tools"
	// instruction as unfollowable for analyse_learner/plan_lesson,
	// whose agents ("summary"/"lesson") cmd/jlp/main.go hasn't
	// Allow()ed any tool for — the system prompt must agree with what
	// the card honestly discloses, not just the card.
	SystemWithTools, SystemWithoutTools string
}

// skillOrder is the card's/lookup's deterministic ordering — a plain
// map has none, and the agent card should list skills the same way on
// every call.
var skillOrder = []string{"review_writing", "analyse_learner", "plan_lesson"}

// skillDefs maps every skill ID Routes() accepts onto its definition.
// Exactly three entries, matching the task brief's PRD §30 example
// (Writing Coach/Learner Analyst/Lesson Planner) onto the three local
// agents that already exist for them (internal/agent/teacher,
// internal/agent/summary, internal/agent/lesson).
var skillDefs = map[string]skillDef{
	"review_writing": {
		Name:               "Writing Reviewer",
		Description:        "Reviews a piece of Japanese writing and reports corrections and feedback, after investigating the learner's own history through the Teacher agent's permitted tools.",
		Agent:              "teacher",
		PromptName:         "a2a.review_writing",
		PromptVersion:      "v1",
		SystemWithTools:    "You are the Writing Reviewer agent (JLP's Teacher). First investigate the learner's own history using your available tools, then review the Japanese writing given as input: report the corrections and feedback you would give, in prose.",
		SystemWithoutTools: "You are the Writing Reviewer agent (JLP's Teacher). You have no tools available for this task right now, so review the Japanese writing given as input directly, from the input alone: report the corrections and feedback you would give, in prose.",
	},
	"analyse_learner": {
		Name:               "Learner Analyst",
		Description:        "Analyses the learner's recent progress, strengths, and weaknesses, after investigating their history through the Learner Analyst agent's permitted tools.",
		Agent:              "summary",
		PromptName:         "a2a.analyse_learner",
		PromptVersion:      "v1",
		SystemWithTools:    "You are the Learner Analyst agent. First investigate the learner's own history using your available tools, then analyse their recent progress, strengths, and weaknesses in light of the input given.",
		SystemWithoutTools: "You are the Learner Analyst agent. You have no tools available for this task right now, so analyse the learner's recent progress, strengths, and weaknesses using only the input given, without further investigation.",
	},
	"plan_lesson": {
		Name:               "Lesson Planner",
		Description:        "Proposes what a human tutor's next lesson should cover, after investigating the learner's history through the Lesson Planner agent's permitted tools.",
		Agent:              "lesson",
		PromptName:         "a2a.plan_lesson",
		PromptVersion:      "v1",
		SystemWithTools:    "You are the Lesson Planner agent. First investigate the learner's own history using your available tools, then propose what a human tutor's next lesson should cover, given the input as context.",
		SystemWithoutTools: "You are the Lesson Planner agent. You have no tools available for this task right now, so propose what a human tutor's next lesson should cover using only the input given as context, without further investigation.",
	},
}

// systemFor returns def's system prompt, chosen by whether reg
// currently Allow()s def.Agent any tool at all — see skillDef's
// SystemWithTools/SystemWithoutTools doc comment. Reads reg live, on
// every call, the same as card.go's SkillCard.Tools: if
// cmd/jlp/main.go's allowlist for this agent ever changes, the very
// next task picks up the matching prompt automatically.
func (s *Server) systemFor(def skillDef) string {
	if len(s.reg.DefsFor(def.Agent)) > 0 {
		return def.SystemWithTools
	}
	return def.SystemWithoutTools
}

// taskInputSchema/taskOutputSchema are the (identical, across all
// three skills) JSON Schemas SkillCard advertises: every skill takes
// the same {input, session_id?} shape and returns the same
// {task_id, status, output?, error?} shape (see taskRequest/
// taskResponse below) — the skills differ in what System frames the
// conversation as and which agent/allowlist runs it, not in wire
// shape.
var taskInputSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"Free-text task input — e.g. the writing to review, or context for the analysis/plan."},"session_id":{"type":"string","description":"Optional existing session id to scope the underlying agent-run to."}},"required":["input"],"additionalProperties":true}`)

var taskOutputSchema = json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"},"status":{"type":"string","enum":["completed","failed"]},"output":{"type":"string"},"error":{"type":"string"}},"required":["task_id","status"]}`)

// taskRequest is POST {path}/tasks' body. Deliberately has NO Identity
// field: identity always comes from request context (see
// WithIdentity/IdentityFrom in server.go), never the payload — see the
// package doc comment's Identity section. A caller that includes an
// "identity" field anyway is harmless: encoding/json.Unmarshal simply
// has no struct field to put it in, so it's silently dropped, exactly
// like any other unrecognized key — see
// TestCreateTaskIgnoresForgedIdentityInBody.
type taskRequest struct {
	Skill     string `json:"skill"`
	Input     string `json:"input"`
	SessionID string `json:"session_id,omitempty"`
}

// taskResponse is both POST {path}/tasks' response body and GET
// {path}/tasks/{task_id}'s — the same shape either way, since GET
// just reads back what POST already computed (see handleCreateTask's
// doc comment on synchronous execution).
type taskResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// maxTaskRequestBodyBytes caps POST {path}/tasks' body — the same 1
// MiB cap internal/adapters/http applies to every JSON body it reads
// (see that package's api.go maxRequestBodyBytes doc comment, and
// documents.go/words.go, which apply it the same way this handler
// does). task input is forwarded verbatim into an LLM call, so an
// unbounded body is both a memory problem and a token-spend problem —
// same reasoning as every sibling handler, same number, not a new one
// invented for this package (Task 3 code review, Important 2).
const maxTaskRequestBodyBytes = 1 << 20

// taskRecord is what Server.tasks caches per task_id — see that
// field's own doc comment on why this is a response cache, not a
// second source of truth. Identity is the identity that CREATED the
// task (never one from the request body — see taskRequest's doc
// comment) and is what scopes handleGetTask: Output is model text
// derived from that identity's own learner history (corrections,
// priorities, session context), so a different authenticated identity
// reading it back would be a cross-identity leak — the one read path
// in this codebase that would otherwise not be identity-scoped, unlike
// every storage.*Repository method and the agent_runs table itself
// (Task 3 code review, Important 1).
type taskRecord struct {
	Identity learner.IdentityID
	Skill    string
	Status   string
	Output   string
	Error    string
}

// handleCreateTask runs skill req.Skill's underlying agent through
// runner.Run, synchronously: by the time this handler responds, the
// run has already completed (or failed) and status is already
// "completed"/"failed" — never "running" or "pending". This is a
// deliberate simplification (documented in docs/api/a2a.md) rather
// than a queue-and-poll design: JLP's agent-run loop is already a
// single bounded (MaxTurns-capped) synchronous call, so there is no
// long-running work here to hand a task_id back early for. GET
// {path}/tasks/{task_id} still exists — for protocol compatibility
// with A2A clients that always poll after creating a task — it just
// always finds the task already done.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskRequestBodyBytes)
	var req taskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusBadRequest, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	def, ok := skillDefs[req.Skill]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown skill "+strconv.Quote(req.Skill))
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeError(w, http.StatusBadRequest, `"input" is required`)
		return
	}

	// See the package doc comment's Identity section and
	// WithIdentity/IdentityFrom in server.go: the ONLY source of
	// identity here is request context, set by whoever mounted
	// Routes() inside their own authenticated group — never req above,
	// which has no Identity field to have read one from in the first
	// place.
	identity, ok := IdentityFrom(r.Context())
	if !ok || identity == "" {
		writeError(w, http.StatusUnauthorized, "no authenticated identity")
		return
	}

	var sessionID *session.ID
	if req.SessionID != "" {
		sid := session.ID(req.SessionID)
		sessionID = &sid
	}

	out, runErr := s.runner.Run(r.Context(), agentrun.RunInput{
		Agent:         def.Agent,
		PromptName:    def.PromptName,
		PromptVersion: def.PromptVersion,
		System:        s.systemFor(def),
		Messages:      []ai.ToolMessage{{Role: "user", Text: req.Input}},
		Identity:      identity,
		SessionID:     sessionID,
	})

	taskID := out.RunID
	if taskID == "" {
		// Only reachable when Run failed before even generating/
		// persisting a run id (e.g. the agent_runs Start write itself
		// failed) — see agentrun.Runner.Run's doc comment. Fabricate an
		// id so the caller still gets a task resource back; it's cached
		// below like any other, so GET on it still works — there's just
		// no agent_runs row behind it (Start never got that far), unlike
		// every other task_id this handler ever hands out.
		taskID = uuid.NewString()
	}

	rec := taskRecord{Identity: identity, Skill: req.Skill}
	if runErr != nil {
		rec.Status = "failed"
		rec.Error = runErr.Error()
	} else {
		rec.Status = "completed"
		rec.Output = out.Text
	}

	s.mu.Lock()
	s.tasks[taskID] = rec
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, taskResponse{TaskID: taskID, Status: rec.Status, Output: rec.Output, Error: rec.Error})
}

// handleGetTask reads back a task this Server's own process already
// computed (see Server.tasks' doc comment) — protocol compatibility
// for a client that always polls after POSTing a task, per
// handleCreateTask's doc comment.
//
// Identity-scoped, same as every other read in this codebase (see
// taskRecord's doc comment): 404 for BOTH "no such task_id" and "that
// task_id exists but belongs to a different identity" — never 403 —
// so the response can never be used to confirm a task_id belongs to
// someone else.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "task_id")

	identity, ok := IdentityFrom(r.Context())
	if !ok || identity == "" {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	s.mu.RLock()
	rec, found := s.tasks[taskID]
	s.mu.RUnlock()
	if !found || rec.Identity != identity {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	writeJSON(w, http.StatusOK, taskResponse{TaskID: taskID, Status: rec.Status, Output: rec.Output, Error: rec.Error})
}
