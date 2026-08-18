package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/agents"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// consult_specialist: how a coordinating agent asks another agent a
// question. See docs/superpowers/specs/2026-08-18-a2a-delegation-design.md.
//
// The tool cannot choose what the delegate is permitted to do. It names
// a SKILL, and the implementation behind ports/agents.Consultant
// resolves that to an agent and therefore to that agent's own tool
// allowlist — the same resolution a remote A2A caller gets. A tool that
// could name an agent could name a more privileged one; this one cannot
// express the idea.

type consultArgs struct {
	Skill    string `json:"skill"`
	Question string `json:"question"`
}

// ConsultTools returns the delegation tool. Registered like any other,
// and — critically — Allow()ed to the coordinator agent alone, which is
// what caps delegation depth at one without a counter.
func ConsultTools(c agents.Consultant) []Tool {
	return []Tool{consultSpecialistTool(c)}
}

func consultSpecialistTool(c agents.Consultant) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "consult_specialist",
			Description: `Asks another JLP skill a question and returns its answer. Use for a question that genuinely spans specialities; each call is a full second agent run, so do not use it for anything you can answer yourself. Consult a given specialist at most once. Skills: "review_writing" (corrections on a piece of writing), "analyse_learner" (what the learner's history shows), "plan_lesson" (what to study next).`,
			Schema:      json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","enum":["review_writing","analyse_learner","plan_lesson"]},"question":{"type":"string"}},"required":["skill","question"],"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[consultArgs](args)
			if err != nil {
				return "", fmt.Errorf("consult_specialist: %w", err)
			}
			// identity comes from the Registry's Invoke, which took it
			// from the run — never from the arguments. A model naming a
			// learner would be a model choosing whose history to read.
			answer, err := c.Consult(ctx, identity, a.Skill, a.Question)
			if err != nil {
				// Returned as a RESULT, not an error: "the analyst could
				// not answer" is something the coordinator can work with,
				// and failing the tool would throw away everything it
				// gathered before asking.
				return fmt.Sprintf("consulting %s failed: %v", a.Skill, err), nil
			}
			return answer, nil
		},
	}
}
