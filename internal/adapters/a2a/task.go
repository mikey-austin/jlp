package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// skillDef is everything one A2A skill needs to drive an
// agentrun.RunInput: which local agent it maps onto (and therefore
// which internal/tools.Registry allowlist governs it — see card.go's
// toolTags) and what system prompt frames the task. PromptName/
// PromptVersion are metadata only (agent_runs columns, for the
// /ai/agents trace viewer) — deliberately "a2a.*" names rather than
// reusing e.g. internal/agent/teacher's private "teacher.agentic"
// prompt/template: this package renders no internal/prompts template of
// its own (Rule 3 keeps that machinery inside internal/agent/*), it
// just hands the runner a System string directly, same shape, own
// identity in the trace.
type skillDef struct {
	Name string
	// Description/DescriptionWithoutTools are the two card descriptions
	// descriptionFor chooses between at serve time, exactly as
	// SystemWithTools/SystemWithoutTools work for the prompt.
	//
	// Whole-branch review I-3: Description alone used to be a static
	// string promising a result produced "after investigating the
	// learner's history through the … agent's permitted tools" — for
	// analyse_learner and plan_lesson, whose agents ("summary"/"lesson")
	// cmd/jlp/main.go has Allow()ed nothing, that promise cannot be
	// kept. The MECHANISM already degraded honestly (empty allowlist →
	// no `tool:` tags → SystemWithoutTools); only the advertisement
	// didn't follow, so a remote client read "Lesson Planner", expected
	// a history-informed plan, and got prose derived from its own
	// message. An agent card is a contract with strangers.
	//
	// Chosen at serve time off the LIVE registry rather than corrected
	// as two literals, so granting those allowlists later fixes the card
	// in the same commit as the grant instead of needing a second one —
	// and so the card can never again claim a capability the process
	// does not have.
	Description, DescriptionWithoutTools string
	// Tags/Examples are the A2A card's own discovery fields (spec
	// AgentSkill): keywords a remote agent can match on, and example
	// prompts showing what this skill expects. card.go appends the live
	// `tool:` allowlist tags to Tags at serve time.
	Tags, Examples []string
	// Agent is the internal/tools.Registry agent name this skill runs
	// as — MUST match one already Allow()ed in cmd/jlp/main.go (today:
	// only "teacher"). A skill mapped onto an agent with no allowlist
	// entry still runs — it just can't call any tool, which is itself a
	// correct, unwidened permission boundary, not a bug in this file.
	// See docs/api/a2a.md's "known consequence" note.
	Agent                     string
	PromptName, PromptVersion string
	// SystemWithTools/SystemWithoutTools are the two system prompts
	// systemFor chooses between, at request time, based on whether
	// Agent CURRENTLY has any tool Allow()ed (reg.DefsFor(Agent) — the
	// exact same live signal card.go's toolTags reads). Kept as two
	// separate, fully-written prompts rather than one templated string:
	// SystemWithTools' "investigate using your available tools"
	// instruction is unfollowable for analyse_learner/plan_lesson,
	// whose agents ("summary"/"lesson") cmd/jlp/main.go hasn't
	// Allow()ed any tool for — the system prompt must agree with what
	// the card honestly discloses, not just the card.
	SystemWithTools, SystemWithoutTools string
}

// defaultSkill is where a message with no explicit skill goes.
//
// A2A v1.0 has no "invoke skill X" field: a client sends a Message, and
// what the agent does with it is the agent's business. Skills are a
// DISCOVERY aid on the card, not a dispatch key on the wire. So a plain
// chat client — which is exactly what the official SDK's `sendMessage`
// produces — must land somewhere sensible rather than being rejected
// for omitting something the protocol never gave it a way to send.
// "chat" is that landing place: a conversational tutor turn, advertised
// on the card like any other skill so it isn't a hidden behaviour. A
// caller that DOES want a specific skill selects it through metadata —
// see skillFrom.
const defaultSkill = "chat"

// skillOrder is the card's/lookup's deterministic ordering — a plain
// map has none, and the agent card should list skills the same way on
// every call. defaultSkill leads, because it is what an unqualified
// message gets.
var skillOrder = []string{"chat", "review_writing", "analyse_learner", "plan_lesson"}

// skillDefs maps every skill ID this adapter accepts onto its
// definition: the three from PRD §30's example (Writing Coach/Learner
// Analyst/Lesson Planner), mapped onto the three local agents that
// already exist for them (internal/agent/teacher, internal/agent/
// summary, internal/agent/lesson), plus the conversational default.
var skillDefs = map[string]skillDef{
	"chat": {
		Name:                    "Japanese Tutor Chat",
		Description:             "Answers a free-text question about Japanese — grammar, vocabulary, usage, or the learner's own progress — as a conversational tutor, consulting the learner's own history through this agent's permitted tools where that makes the answer more useful. This is what a message with no explicitly selected skill is handled as.",
		DescriptionWithoutTools: "Answers a free-text question about Japanese — grammar, vocabulary, usage, or study advice — as a conversational tutor, from the message alone. This skill currently has NO tool access, so it cannot consult the learner's own history. This is what a message with no explicitly selected skill is handled as.",
		Tags:                    []string{"japanese", "chat", "tutor", "conversation", "default"},
		Examples: []string{
			"「は」と「が」の違いを教えてください。",
			"How do I use the て-form to link two actions?",
			"What should I study next?",
		},
		Agent:              "teacher",
		PromptName:         "a2a.chat",
		PromptVersion:      "v1",
		SystemWithTools:    "You are JLP's Japanese tutor, talking to your learner. Answer their message directly and conversationally. Where their own history would make the answer more useful, investigate it first using your available tools.",
		SystemWithoutTools: "You are JLP's Japanese tutor, talking to your learner. You have no tools available for this task right now, so answer their message directly and conversationally from the message alone.",
	},
	"review_writing": {
		Name:                    "Writing Reviewer",
		Description:             "Reviews a piece of Japanese writing and reports corrections and feedback, after investigating the learner's own history through the Teacher agent's permitted tools.",
		DescriptionWithoutTools: "Reviews a piece of Japanese writing and reports corrections and feedback, from the submitted text alone. This skill currently has NO tool access, so the review is not informed by the learner's own history.",
		Tags:                    []string{"japanese", "writing", "review", "correction", "feedback"},
		Examples: []string{
			"友達と映画を見ました。とても面白いでした。",
			"Review this paragraph and tell me what a native speaker would fix.",
		},
		Agent:              "teacher",
		PromptName:         "a2a.review_writing",
		PromptVersion:      "v1",
		SystemWithTools:    "You are the Writing Reviewer agent (JLP's Teacher). First investigate the learner's own history using your available tools, then review the Japanese writing given as input: report the corrections and feedback you would give, in prose.",
		SystemWithoutTools: "You are the Writing Reviewer agent (JLP's Teacher). You have no tools available for this task right now, so review the Japanese writing given as input directly, from the input alone: report the corrections and feedback you would give, in prose.",
	},
	"analyse_learner": {
		Name:                    "Learner Analyst",
		Description:             "Analyses the learner's recent progress, strengths, and weaknesses, after investigating their history through the Learner Analyst agent's permitted tools.",
		DescriptionWithoutTools: "Analyses Japanese progress, strengths, and weaknesses from the input you provide. This skill currently has NO tool access, so it cannot read the learner's stored history — send the material you want analysed in the message itself.",
		Tags:                    []string{"japanese", "analysis", "progress", "strengths", "weaknesses"},
		Examples: []string{
			"How am I doing this month?",
			"What are my recurring weaknesses?",
		},
		Agent:              "summary",
		PromptName:         "a2a.analyse_learner",
		PromptVersion:      "v1",
		SystemWithTools:    "You are the Learner Analyst agent. First investigate the learner's own history using your available tools, then analyse their recent progress, strengths, and weaknesses in light of the input given.",
		SystemWithoutTools: "You are the Learner Analyst agent. You have no tools available for this task right now, so analyse the learner's recent progress, strengths, and weaknesses using only the input given, without further investigation.",
	},
	"plan_lesson": {
		Name:                    "Lesson Planner",
		Description:             "Proposes what a human tutor's next lesson should cover, after investigating the learner's history through the Lesson Planner agent's permitted tools.",
		DescriptionWithoutTools: "Proposes what a human tutor's next lesson should cover, using only the context you provide. This skill currently has NO tool access, so the plan is not informed by the learner's stored history — include what they have been working on in the message itself.",
		Tags:                    []string{"japanese", "lesson", "planning", "curriculum", "teaching"},
		Examples: []string{
			"Plan my next lesson.",
			"We have 45 minutes on Thursday — what should we cover?",
		},
		Agent:              "lesson",
		PromptName:         "a2a.plan_lesson",
		PromptVersion:      "v1",
		SystemWithTools:    "You are the Lesson Planner agent. First investigate the learner's own history using your available tools, then propose what a human tutor's next lesson should cover, given the input as context.",
		SystemWithoutTools: "You are the Lesson Planner agent. You have no tools available for this task right now, so propose what a human tutor's next lesson should cover using only the input given as context, without further investigation.",
	},
}

// PromptNames returns every ai.ToolRequest PromptName this adapter can
// send, in skillOrder, DERIVED from skillDefs rather than restated.
//
// It exists for exactly one caller — cmd/jlp/ai.go's knownPromptNames/
// knownToolPromptNames, the boot-time APP_AI_ROUTES validators —
// because those lists are hand-maintained and this adapter landed
// concurrently with the fix that introduced them. The result was
// whole-branch review I-4: an operator setting
// APP_AI_ROUTES=a2a.chat=anthropic got a boot WARNING calling their
// correct prompt name a typo, and the route was then silently skipped,
// so every A2A request went to APP_AI_PROVIDER anyway. That is the same
// class of failure Task 2's bb3878d fix closed, re-created from the
// other side.
//
// Deriving rather than duplicating is the point: adding a skill to
// skillDefs now registers its prompt name with both validators, so the
// two can never drift again.
func PromptNames() []string {
	names := make([]string, 0, len(skillOrder))
	for _, id := range skillOrder {
		names = append(names, skillDefs[id].PromptName)
	}
	return names
}

// systemFor returns def's system prompt, chosen by whether reg
// currently Allow()s def.Agent any tool at all — see skillDef's
// SystemWithTools/SystemWithoutTools doc comment. Reads reg live, on
// every call, the same as card.go's toolTags: if cmd/jlp/main.go's
// allowlist for this agent ever changes, the very next task picks up
// the matching prompt automatically.
// descriptionFor returns def's agent-card description, chosen by the
// same live registry signal systemFor uses — see skillDef.Description
// for what a static description cost (whole-branch review I-3).
func (s *Server) descriptionFor(def skillDef) string {
	if len(s.reg.DefsFor(def.Agent)) > 0 {
		return def.Description
	}
	return def.DescriptionWithoutTools
}

func (s *Server) systemFor(def skillDef) string {
	if len(s.reg.DefsFor(def.Agent)) > 0 {
		return def.SystemWithTools
	}
	return def.SystemWithoutTools
}

// taskRecord is what Server.tasks caches per task id — see that field's
// own doc comment on why this is a response cache, not a second source
// of truth. Identity is the identity that CREATED the task (never one
// from the request payload — see sendMessageParams' doc comment) and is
// what scopes handleGetTask/handleCancelTask: the Task's artifact is
// model text derived from that identity's own learner history
// (corrections, priorities, session context), so a different
// authenticated identity reading it back would be a cross-identity
// leak — the one read path in this codebase that would otherwise not be
// identity-scoped, unlike every storage.*Repository method and the
// agent_runs table itself.
type taskRecord struct {
	Identity learner.IdentityID
	Task     Task
}

// stringFromMetadata reads key out of a metadata map as a non-empty
// string, if it's there and is one.
func stringFromMetadata(md map[string]any, key string) string {
	if md == nil {
		return ""
	}
	v, _ := md[key].(string)
	return strings.TrimSpace(v)
}

// skillFrom picks which skill a message invokes.
//
// A2A v1.0's wire format has no skill selector (see defaultSkill), so
// this adapter reads an OPTIONAL one out of metadata — the protocol's
// designated "flexible key-value map for passing additional context or
// parameters" — checking the message's own metadata first (where a chat
// UI would naturally attach a per-message choice) and then the
// request's. Absent or unrecognised, the conversational default is used
// rather than an error: an A2A client is entitled to know nothing about
// JLP's skill ids, and refusing its message would be refusing the
// protocol's own normal case.
func skillFrom(p sendMessageParams) (string, skillDef) {
	for _, md := range []map[string]any{p.Message.Metadata, p.Metadata} {
		if id := stringFromMetadata(md, "skill"); id != "" {
			if def, ok := skillDefs[id]; ok {
				return id, def
			}
		}
	}
	return defaultSkill, skillDefs[defaultSkill]
}

// sessionFrom reads an OPTIONAL existing JLP session id out of
// metadata, to scope the underlying agent-run's trace to it — the one
// capability the retired bespoke shape's `session_id` field had that
// A2A has no field for.
//
// Deliberately NOT taken from the A2A contextId, tempting as the
// analogy is. agent_runs.session_id is `uuid REFERENCES sessions(id)`,
// a real foreign key into JLP's own learning sessions; a contextId is a
// grouping identifier the CLIENT invents and hands us for its own
// conversation threading. Feeding one to the other would turn every
// ordinary chat into a failed insert, and would be wrong even if it
// didn't: the two identifiers mean different things.
func sessionFrom(p sendMessageParams) *session.ID {
	for _, md := range []map[string]any{p.Message.Metadata, p.Metadata} {
		if raw := stringFromMetadata(md, "session_id"); raw != "" {
			sid := session.ID(raw)
			return &sid
		}
	}
	return nil
}

// handleSendMessage serves the SendMessage method: it runs the selected
// skill's underlying agent through runner.Run, SYNCHRONOUSLY, and
// returns the finished Task. By the time this responds the run has
// already completed (or failed) and the Task's state is already
// TASK_STATE_COMPLETED/TASK_STATE_FAILED — never SUBMITTED or WORKING.
//
// That is allowed, and is in fact the protocol's default: v1.0's
// SendMessageConfiguration.returnImmediately defaults to false, meaning
// "the operation MUST wait until the task reaches a terminal or
// interrupted state before returning". It is also the honest shape for
// what's underneath — JLP's agent-run loop is already a single bounded
// (MaxTurns-capped) synchronous call, so there is no long-running work
// to hand a task id back early for. GetTask still exists, for clients
// that always poll after sending; it just always finds the task done.
func (s *Server) handleSendMessage(ctx context.Context, identity learner.IdentityID, params json.RawMessage) (any, *rpcError) {
	var p sendMessageParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, rpcErrf(errInvalidParams, "invalid SendMessage params: "+err.Error())
	}

	input := strings.TrimSpace(p.Message.text())
	if input == "" {
		return nil, rpcErrf(errInvalidParams, "message must contain at least one non-empty text part")
	}

	skillID, def := skillFrom(p)

	// contextId groups a conversation. Echo the client's if it supplied
	// one (that's it threading its own conversation), otherwise mint
	// one so the Task always carries a usable, non-empty contextId back
	// — the spec has the server generate it for a new context.
	contextID := strings.TrimSpace(p.Message.ContextID)
	if contextID == "" {
		contextID = uuid.NewString()
	}

	out, runErr := s.runner.Run(ctx, agentrun.RunInput{
		Agent:         def.Agent,
		PromptName:    def.PromptName,
		PromptVersion: def.PromptVersion,
		System:        s.systemFor(def),
		Messages:      []ai.ToolMessage{{Role: "user", Text: input}},
		Identity:      identity,
		SessionID:     sessionFrom(p),
	})

	taskID := out.RunID
	if taskID == "" {
		// Only reachable when Run failed before even generating/
		// persisting a run id (e.g. the agent_runs Start write itself
		// failed) — see agentrun.Runner.Run's doc comment. Mint one so
		// the caller still gets a Task resource back; it's cached below
		// like any other, so GetTask on it still works — there's just no
		// agent_runs row behind it, unlike every other task id this
		// method hands out.
		taskID = uuid.NewString()
	}

	// The client's own message, threaded onto the task, is the first
	// entry of history — echoing its messageId back if it sent one so
	// the client can correlate.
	userMsg := p.Message
	if strings.TrimSpace(userMsg.MessageID) == "" {
		userMsg.MessageID = uuid.NewString()
	}
	userMsg.Role = roleUser
	userMsg.TaskID = taskID
	userMsg.ContextID = contextID

	now := time.Now().UTC().Format(time.RFC3339)
	task := Task{
		ID:        taskID,
		ContextID: contextID,
		Status:    TaskStatus{Timestamp: now},
		History:   []Message{userMsg},
		// Metadata is not part of the protocol's semantics — it's the
		// spec's "custom metadata about a task" — but recording which
		// skill actually ran makes an otherwise invisible routing
		// decision (see skillFrom) legible to the caller.
		Metadata: map[string]any{"skill": skillID},
	}

	agentMsg := Message{
		MessageID: uuid.NewString(),
		ContextID: contextID,
		TaskID:    taskID,
		Role:      roleAgent,
	}
	if runErr != nil {
		task.Status.State = taskStateFailed
		// A Task has no error field; the spec's place for "why" is the
		// status message, so that is where a failed run's error goes
		// rather than being dropped or smuggled into an artifact that
		// would read as a successful answer.
		agentMsg.Parts = []Part{textPart(runErr.Error())}
		task.Status.Message = &agentMsg
	} else {
		task.Status.State = taskStateCompleted
		agentMsg.Parts = []Part{textPart(out.Text)}
		// The prose part comes FIRST and always. Widgets are enrichment
		// on top of a complete answer: any A2A client may ignore parts it
		// does not understand, and other clients will — so nothing a
		// reader needs may exist only inside a data part. See widgets.go.
		//
		// The status message deliberately keeps prose alone: it is the
		// short "here is what happened" summary, while the artifact is
		// the result being handed over.
		artifactParts := append([]Part{textPart(out.Text)}, widgetParts(out.ToolCalls)...)
		task.Artifacts = []Artifact{{
			ArtifactID:  uuid.NewString(),
			Name:        skillID,
			Description: def.Name + " result",
			Parts:       artifactParts,
		}}
	}
	task.History = append(task.History, agentMsg)

	s.remember(taskID, taskRecord{Identity: identity, Task: task})

	// Enveloped, unlike GetTask/CancelTask below: SendMessage's result
	// is a `task`|`message` oneof and the official client parses it as
	// such. See sendMessageResult's doc comment.
	return sendMessageResult{Task: &task}, nil
}

// lookup returns the task id's record if — and only if — identity owns
// it. The two failure modes are deliberately indistinguishable to the
// caller: "no such task" and "that task is someone else's" both come
// back as errTaskNotFound with the same message, so the error can never
// be used as an oracle to confirm a task id belongs to another
// identity. Every read path below goes through here.
func (s *Server) lookup(identity learner.IdentityID, id string) (taskRecord, *rpcError) {
	s.mu.RLock()
	rec, found := s.tasks[id]
	s.mu.RUnlock()
	if !found || rec.Identity != identity {
		return taskRecord{}, rpcErrf(errTaskNotFound, "task not found")
	}
	return rec, nil
}

// handleGetTask serves the GetTask method: reads back a task this
// Server's own process already computed (see Server.tasks' doc
// comment) — protocol compatibility for a client that always polls
// after sending a message, per handleSendMessage's doc comment.
//
// Returns a BARE Task, not an envelope: the official client parses
// GetTask's result with `Task.fromJSON` directly, unlike SendMessage's.
func (s *Server) handleGetTask(identity learner.IdentityID, params json.RawMessage) (any, *rpcError) {
	var p taskIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, rpcErrf(errInvalidParams, "invalid GetTask params: "+err.Error())
	}
	rec, rpcErr := s.lookup(identity, p.ID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return rec.Task, nil
}

// handleCancelTask serves the CancelTask method.
//
// Every task this adapter has ever created is already in a terminal
// state before the caller could learn its id (handleSendMessage runs
// the whole agent-run loop before responding), so there is nothing here
// that can be cancelled and -32002 TASK_NOT_CANCELABLE is the correct,
// spec-named answer — not a stub, and not a silently successful no-op
// that would tell a client it had stopped work that in fact already
// finished.
//
// The identity check runs FIRST and answers -32001 for a task belonging
// to someone else, exactly as GetTask does. Answering "not cancelable"
// there would leak the task's existence to a caller who cannot read it.
func (s *Server) handleCancelTask(identity learner.IdentityID, params json.RawMessage) (any, *rpcError) {
	var p taskIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, rpcErrf(errInvalidParams, "invalid CancelTask params: "+err.Error())
	}
	if _, rpcErr := s.lookup(identity, p.ID); rpcErr != nil {
		return nil, rpcErr
	}
	return nil, rpcErrf(errTaskNotCancelable, "task has already reached a terminal state and cannot be canceled")
}
