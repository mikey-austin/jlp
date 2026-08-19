package a2a

import (
	"context"
	"fmt"
	"strings"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/agents"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// Delegation: one agent consulting another through this adapter's own
// skill table. See docs/superpowers/specs/2026-08-18-a2a-delegation-design.md.
//
// This is the ONLY door. A delegated run resolves its skill to an agent
// here, exactly as a remote caller's would, so it executes under that
// agent's tool allowlist by construction rather than because a caller
// passed the right agent name.

// consultMaxTurns bounds a delegated run.
//
// Lower than a top-level run's: a consulted specialist answers one
// specific question rather than an open brief, and a delegation is
// already a second full agent run whose cost the learner did not
// directly ask for.
const consultMaxTurns = 6

// depthKey marks a context as belonging to an already-delegated run.
type depthKey struct{}

// withDelegationDepth marks ctx as running inside a delegation.
func withDelegationDepth(ctx context.Context) context.Context {
	return context.WithValue(ctx, depthKey{}, true)
}

func delegated(ctx context.Context) bool {
	v, _ := ctx.Value(depthKey{}).(bool)
	return v
}

// Consult runs skill's agent against question, for identity.
//
// Depth is normally bounded by the allowlist — only the coordinator
// agent is granted the consult tool, so a specialist cannot delegate.
// The check below is defence in depth for the day those grants are
// widened without anyone thinking about recursion: the failure should be
// a refusal, not a fan-out of model calls.
func (s *Server) Consult(ctx context.Context, identity learner.IdentityID, skill, question string) (string, error) {
	if delegated(ctx) {
		return "", agents.ErrDelegationDepth
	}

	def, ok := skillDefs[skill]
	if !ok {
		return "", fmt.Errorf("%w: %q", agents.ErrUnknownSkill, skill)
	}
	// A coordinator consulting itself would recurse through a door the
	// depth guard only closes one level down; refuse it by name too.
	if def.Agent == coordinatorAgent {
		return "", fmt.Errorf("%w: %q coordinates rather than answers", agents.ErrUnknownSkill, skill)
	}

	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("agents: no question for skill %q", skill)
	}

	// One consultation per specialist per run. A model that asks the
	// same specialist twice is looping, not investigating — observed
	// doing exactly that, spawning a fresh agent run each time while its
	// own run never finished.
	//
	// Reported back as an ordinary error the model can read, so it
	// learns it already has that answer rather than being cut off with
	// nothing.
	if parent := agentrun.RunIDFrom(ctx); parent != "" && !s.markConsulted(parent, skill) {
		return "", fmt.Errorf("already consulted %s in this run; use the answer you were given", skill)
	}

	out, err := s.runner.Run(withDelegationDepth(ctx), agentrun.RunInput{
		Agent:         def.Agent,
		PromptName:    def.PromptName,
		PromptVersion: def.PromptVersion,
		// No rendering note: a delegate's answer is read by a MODEL, not
		// by a browser, so telling it that cards will be displayed would
		// have it write "as shown above" into text nobody renders.
		System:   s.systemFor(def),
		Messages: []ai.ToolMessage{{Role: "user", Text: question}},
		Identity: identity,
		MaxTurns: consultMaxTurns,
		// The consulting run, so the trace can explain this one.
		ParentRun: agentrun.RunIDFrom(ctx),
	})
	if err != nil {
		// Returned to the consulting model as a tool result, not raised:
		// a specialist that failed is information the coordinator can act
		// on ("the analyst could not answer, so I will say what I know"),
		// and failing the whole run would waste everything gathered so
		// far.
		return "", fmt.Errorf("consulting %s: %w", skill, err)
	}

	// Keep what the specialist READ, not just what it said. The
	// coordinator's own tool calls are all consult_specialist, which
	// renders as nothing, so without this a coordinated answer is the
	// one kind of answer that can never carry a card — the corrections
	// were fetched one level down and thrown away with the child run.
	//
	// The raw calls are kept, not parts: the client's
	// acceptedOutputModes are not visible here, and widgetParts already
	// knows how to filter and dedupe. Doing it there means a delegated
	// widget obeys exactly the same rules as a direct one, including
	// the socratic redaction internal/tools already applied.
	//
	// Only the model-facing prose is returned. The coordinator reads
	// text; feeding it the raw JSON as well would spend its context on
	// data it cannot do anything with and invite it to transcribe the
	// card into words.
	s.recordDelegated(agentrun.RunIDFrom(ctx), out.ToolCalls)
	return out.Text, nil
}

// recordDelegated files a specialist's tool calls under the run that
// consulted it. A run id of "" means nothing is coordinating this call
// (a direct Consult in a test, say), so there is no task to attach them
// to and holding them would leak.
func (s *Server) recordDelegated(parent string, calls []agentrun.ToolCall) {
	if parent == "" || len(calls) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delegated[parent] = append(s.delegated[parent], calls...)
}

// CoordinatorAgent exposes the agent name behind the "coordinate"
// skill, so cmd/jlp can assert its allowlist is attached to the same
// agent the skill table resolves to. Two constants naming one agent is
// how the skill silently ends up with no grants at all.
func CoordinatorAgent() string { return coordinatorAgent }

// markConsulted records that run has consulted skill, reporting false if
// it already had.
func (s *Server) markConsulted(run, skill string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.consulted[run]
	if seen == nil {
		seen = map[string]bool{}
		s.consulted[run] = seen
	}
	if seen[skill] {
		return false
	}
	seen[skill] = true
	return true
}

// endRun drops a finished run's delegation bookkeeping and hands back
// the tool calls its specialists made, for the task to render.
//
// Both maps are cleared here, in one place, because they have the same
// lifetime: they describe work in flight, and a process that kept them
// would grow one entry per question ever asked.
func (s *Server) endRun(run string) []agentrun.ToolCall {
	if run == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.consulted, run)
	calls := s.delegated[run]
	delete(s.delegated, run)
	return calls
}
