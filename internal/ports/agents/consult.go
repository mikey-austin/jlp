// Package agents is the port through which one agent consults another.
//
// It exists so internal/tools can delegate without importing an adapter
// — PRD §75 Rule 3 — and so the thing doing the delegating cannot pick
// which permissions the delegate runs under. The implementation
// (internal/adapters/a2a) resolves a skill to an agent and therefore to
// that agent's tool allowlist, exactly as it does for a remote caller.
//
// That is the whole point of routing delegation through this port rather
// than reaching for agentrun.Runner directly: a caller that could name
// the agent could also name a more privileged one, and the boundary
// would then depend on every future maintainer passing the right string.
// Here it depends on nothing.
package agents

import (
	"context"
	"errors"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// ErrUnknownSkill is returned when no skill has the requested id. The
// consulting model chose it, so this is ordinary wrong input rather than
// a failure — the tool reports it back and the model can try another.
var ErrUnknownSkill = errors.New("agents: unknown skill")

// ErrDelegationDepth is returned when a delegated run tries to delegate
// again. Depth is normally bounded by the allowlist (only the
// coordinator is granted the consult tool), so reaching this means the
// grants have been widened; failing here keeps that from becoming a
// recursive fan-out of model calls.
var ErrDelegationDepth = errors.New("agents: a consulted agent may not consult further")

// Consultant runs one skill on behalf of another agent.
//
// identity is the LEARNER the work is for, passed explicitly and never
// inferred: a consulted specialist reads that learner's history, and
// taking the identity from anywhere else — a token, a default, the
// process — is how one learner's data reaches another's answer.
type Consultant interface {
	Consult(ctx context.Context, identity learner.IdentityID, skill, question string) (string, error)
}
