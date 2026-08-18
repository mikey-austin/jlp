package main

import (
	"testing"

	"github.com/mikeyaustin/jlp/internal/tools"
)

// Every A2A skill maps onto one of these agents (internal/adapters/a2a's
// skillDefs). An agent with an empty allowlist still answers — it just
// answers from the caller's message alone, which is how analyse_learner
// and plan_lesson spent their existence looking like they worked while
// consulting nothing.
var a2aAgents = []string{"teacher", "summary", "lesson"}

// TestEveryA2AAgentCanConsultSomething pins the fix. It deliberately
// asserts "more than zero tools" rather than an exact list: which tools
// each specialist needs is a judgement that should be free to change,
// while "a specialist that can consult nothing" is always a bug.
func TestEveryA2AAgentCanConsultSomething(t *testing.T) {
	reg := buildRegistryForTest(t)
	for _, agent := range a2aAgents {
		if got := len(reg.DefsFor(agent)); got == 0 {
			t.Errorf("agent %q is permitted no tools, so the skill behind it answers from the message alone", agent)
		}
	}
}

// The read-only boundary is the one thing here that must NOT drift. The
// mutating tools are reserved for an agent that acts on the learner's
// behalf; a remote A2A caller is not that, and a model deciding to
// persist a lesson or record a learning event on its own authority is
// exactly what that boundary exists to prevent.
func TestNoA2AAgentMayMutate(t *testing.T) {
	mutating := map[string]bool{
		"record_learning_event": true,
		"create_exercise":       true,
		"create_lesson_plan":    true,
		"create_anki_card":      true,
	}

	reg := buildRegistryForTest(t)
	for _, agent := range a2aAgents {
		for _, def := range reg.DefsFor(agent) {
			if mutating[def.Name] {
				t.Errorf("agent %q may call %q, which mutates the learner's data on the model's own authority", agent, def.Name)
			}
		}
	}
}

// buildRegistryForTest mirrors main's registration and allowlists.
//
// The tools are registered with nil services: DefsFor only reads the
// catalog and the allowlist, and no handler is ever invoked here. What
// this test is about is WHO MAY CALL WHAT, which is exactly the part
// that has no dependencies.
func buildRegistryForTest(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, set := range [][]tools.Tool{
		tools.SessionTools(nil),
		tools.LearnerTools(nil, nil, nil),
		tools.WritingTools(nil),
		tools.VocabularyTools(nil),
		tools.AnalyticsTools(nil, nil),
		tools.LearningTools(nil, nil, nil, nil),
	} {
		for _, tool := range set {
			reg.Register(tool)
		}
	}
	allowA2AAgents(reg)
	return reg
}
