# A2A delegation — design

**Goal:** one question can reach more than one specialist, without the
asker having to know which.

**Status:** designed 2026-08-18, not yet built. Follows the specialist
tool grants (`ba008bb`), which made the specialists worth consulting in
the first place.

## The problem

Every A2A request runs exactly one agent, chosen by the caller. A
question that spans two specialities — "review this writing and tell me
what to study next" — has no path: you pick the reviewer or the analyst,
and the other half of your question goes unanswered.

Selecting a specialist by hand also assumes you know which one you need,
which is precisely what someone asking a broad question does not.

## Mechanism: delegate through the A2A boundary

The teacher gets a `consult_specialist(skill, question)` tool. Its
handler calls **JLP's own A2A dispatch, in process**.

That choice is the whole design, and it came from asking why delegation
should be a bespoke internal mechanism at all. The permission chain
already exists end to end:

```
A2A dispatch → skillFrom(metadata) → skillDef.Agent → registry allowlist for THAT agent
```

so a delegated run executes under the specialist's own grants **by
construction**. The alternative — a tool that reaches for
`agentrun.Runner` directly with an agent name passed by hand — would
make that boundary something a maintainer has to remember, which is the
kind that drifts.

### In process, not an HTTP loopback

`handleSendMessage(ctx, identity, params)` already takes identity as a
parameter: authentication and dispatch are separate in this adapter.
Delegation therefore passes the *current learner's* identity straight
through.

Over an HTTP loopback to our own `/a2a`, the request would authenticate
through `APIAuth` and the delegated run would act as **the token's**
identity — whoever minted it — not the learner being served. In a
single-user deployment that is invisible; with two learners it is a
cross-account data leak. In-process is the correct call, and the
signature above is what makes it available.

## Exposure: a skill, not a flag

Delegation is a fifth A2A skill rather than a per-request toggle.

Skills are already the menu. As a skill it:

- appears in the chat's existing picker — no new UI,
- is disclosed on the agent card, so an external client can discover it
  and see which tools it carries,
- names itself in `agent_runs.prompt_name`, so the trace says which mode
  ran,
- and, critically, carries its permission difference through the
  mechanism that already exists: **a skill maps to an agent, and an
  agent has an allowlist.**

The delegating skill maps to a NEW agent (working name: `coordinator`)
whose allowlist is the teacher's read tools plus `consult_specialist`.
The plain `chat` skill keeps the `teacher` agent and cannot delegate.

"Delegation enabled" is therefore not a new concept in the permission
model — it is a different agent with a wider grant.

## Recursion bounds itself

`consult_specialist` is granted to `coordinator` and to nobody else. A
specialist that is not allowed the tool cannot delegate, so depth is
capped at 1 by the same allowlist that governs everything else — no
counter to maintain, no new invariant.

A context-carried depth guard is added anyway, as defence in depth: the
day someone grants the tool more widely without thinking it through, the
failure should be a refusal rather than a fork bomb of model calls.

## Cost

A delegation is a second full agent run: up to 10 turns of model calls,
started by a model's judgement rather than a human's.

Controls:

- **Opt-in.** The plain `chat` skill cannot delegate. Choosing the
  coordinator is choosing to spend more.
- **Delegate turn budget.** Nested runs get a lower `MaxTurns` than a
  top-level run (proposed: 6). A consulted specialist answers a specific
  question, not an open brief.
- **One consultation per specialist per run**, enforced in the tool
  handler. A model that asks the analyst twice is looping, not
  investigating.

The honest position: you can already pick a specialist directly, in one
run. Delegation earns its cost only for questions that genuinely span
two specialities. It is opt-in for that reason.

## Trace

`agent_runs` gains a nullable `parent_run_id`. Without it a delegated
run appears in `/ai/agents` as an unexplained second run with no visible
cause, and "why did this cost twice as much" becomes unanswerable from
the trace — which is the question the trace viewer exists to answer.

The child run's own tool calls are recorded as they already are.

## Structure

```
internal/ports/agents      Consultant: Consult(ctx, identity, skill, question) (string, error)
internal/adapters/a2a      implements it — it owns the skill table
internal/tools             consult_specialist, depending on the PORT only
cmd/jlp                    wires the a2a server in; grants the coordinator its allowlist
```

The port exists for the dependency direction: `internal/tools` importing
an adapter fails `make arch-check`, and that rule is worth keeping.

## Testing

The permission properties are the point, so they are what gets pinned:

- A delegated run executes under the **specialist's** allowlist, not the
  caller's — a coordinator that may not call a mutating tool cannot
  obtain one by consulting an agent that can.
- The delegated run carries the **requesting learner's** identity, not a
  token's and not a default.
- A specialist cannot delegate (it lacks the tool), and the depth guard
  refuses one that somehow tries.
- One consultation per specialist per run.
- The plain `chat` skill still cannot delegate at all.

Each mutation-tested: granting the tool to a specialist, passing the
caller's agent name instead of the specialist's, and dropping the depth
guard must each fail a test.

## Out of scope

- **Delegating to external A2A agents.** The natural extension — the
  same tool pointed at another agent's card — and deliberately deferred:
  it needs outbound credentials, trust decisions about a third party
  reading a learner's history, and a per-agent allowlist of its own.
- **Auto-routing.** Choosing a specialist from the message without being
  asked is a different feature, and the label fix in `ba008bb` was
  specifically about not pretending we do it.
- **Parallel consultation.** Sequential first; fan-out multiplies cost
  in a way worth measuring before offering.
