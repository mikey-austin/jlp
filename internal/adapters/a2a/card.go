package a2a

import (
	"encoding/json"
	"net/http"

	"github.com/mikeyaustin/jlp/internal/tools"
)

// cardVersion is this adapter's own protocol/contract version — bumped
// only when the SHAPE of the agent card or the task contract changes,
// independent of JLP's application release cadence.
const cardVersion = "1.0.0"

// AgentCard is the A2A protocol's self-description document, served at
// GET {path}/.well-known/agent-card.json: enough for a remote agent to
// discover what JLP exposes and how to call it. docs/api/a2a.md
// restates the same contract for a human reader.
type AgentCard struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	// TasksEndpoint is this Server's own mount path's /tasks route
	// (cfg.Path + "/tasks"), spelled out explicitly rather than left
	// for the caller to reconstruct from cfg — a remote agent that
	// only ever fetches the card should never need to already know the
	// mount path some other configuration decided.
	TasksEndpoint string       `json:"tasks_endpoint"`
	Skills        []SkillCard  `json:"skills"`
	Capabilities  Capabilities `json:"capabilities"`
}

// SkillCard describes one A2A skill. Tools is the exact, LIVE
// internal/tools.Registry allowlist (internal/tools.Registry.Allow, as
// configured in cmd/jlp/main.go) the underlying agent may call while
// running this skill — read straight from the Registry every time the
// card is served, never a static claim in this file that could
// silently drift from the real permission the agent's local path has.
// This is Rule 13 (PRD §30) made visible: a remote caller can see, up
// front, that this skill reaches no more of the learner's state than
// exactly these tools allow — and if that list is empty, the skill
// currently runs with no tool access at all (still a valid, if less
// useful, run: see docs/api/a2a.md).
type SkillCard struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Tools        []string        `json:"tools"`
}

// Capabilities advertises this adapter's protocol capabilities. Both
// false: task execution is synchronous (see task.go's
// handleCreateTask doc comment) — there is no streaming transport and
// no separate push-notification callback mechanism, so both are
// honestly reported false rather than merely omitted.
type Capabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"push_notifications"`
}

// handleAgentCard serves the agent card — a pure read, computed fresh
// on every request (no caching) since it's cheap and always reflects
// the Registry's current allowlists.
func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	skills := make([]SkillCard, 0, len(skillOrder))
	for _, id := range skillOrder {
		def := skillDefs[id]
		skills = append(skills, SkillCard{
			ID:           id,
			Name:         def.Name,
			Description:  def.Description,
			InputSchema:  taskInputSchema,
			OutputSchema: taskOutputSchema,
			Tools:        toolNamesFor(s.reg, def.Agent),
		})
	}
	writeJSON(w, http.StatusOK, AgentCard{
		Name:          "JLP",
		Description:   "Japanese Learning Platform — exposes selected agents as A2A skills, each running through the exact same tool-registry permissions its local path uses (PRD §30, Rule 13): a remote caller gets no privilege a local agent lacks.",
		Version:       cardVersion,
		TasksEndpoint: s.cfg.Path + "/tasks",
		Skills:        skills,
		Capabilities:  Capabilities{},
	})
}

// toolNamesFor returns the tool names agent may call, per reg — a thin
// projection of reg.DefsFor(agent) (already sorted by name) onto just
// the names, since SkillCard.Tools is a transparency listing, not a
// full tool catalog a remote agent could invoke directly (it never
// gets a Registry of its own — see the package doc comment).
func toolNamesFor(reg *tools.Registry, agent string) []string {
	defs := reg.DefsFor(agent)
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}
