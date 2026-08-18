# A2A rich content — design

**Goal:** the A2A chat renders JLP's structured content — correction
diffs, vocabulary, priorities, lesson plans — instead of flattening
everything to prose.

**Status:** approved 2026-08-18. First of two projects; the second is the
Chrome extension overhaul, which will reuse this renderer.

## The problem

Every A2A skill (`a2a.chat`, `a2a.review_writing`, `a2a.analyse_learner`,
`a2a.plan_lesson`) runs the same tool-calling agent and returns one
thing: prose. The structured data the agent read along the way — the
corrections, the word list, the ranked priorities — is discarded at the
adapter, and what survives is a paragraph describing it.

The chat client makes it worse at the other end: `server.js` flattens a
data part to `JSON.stringify(...)`, so even if the adapter emitted one,
the browser would receive a blob of text.

## Principle

**Prose is always the complete answer; widgets are enrichment.**

The A2A spec lets any client ignore parts it does not understand, and
other clients will. The text part must therefore stand alone — a widget
never carries the only copy of something the reader needs.

## Data flow

```
agent run (unchanged)            A2A adapter                chat client
─────────────────────            ───────────                ───────────
calls get_correction_history
  → structured JSON      ─────┐
calls get_vocabulary_history   │
  → structured JSON      ─────┤
writes prose ─────────────────┼──► artifact.parts:
                              │      {text: "…"}       ──►  bubble (as today)
                              └──►  {data, mediaType}  ──►  widget
```

Three changes, each in one place:

1. **`agentrun.RunOutput` returns the tool results it already
   collected.** The runner holds them in memory (it builds
   `ai.ToolMessage{Role: "tool", Results: results}` every turn) and
   already persists a `tool_calls` row per invoke. Returning them costs
   nothing and adds no query.

2. **The A2A adapter maps renderable tool results to data parts** — one
   per media type, deduplicated to the **last** call of each tool. An
   agent that calls `get_vocabulary_history` three times while narrowing
   down should produce the final view, not three stacked widgets.

3. **The chat client stops discarding them.** `server.js` passes the
   structured value through instead of stringifying it; `app.js` picks a
   renderer by media type and falls back to today's text for anything
   unknown.

## Why "attach what the agent read" and not the alternatives

Considered and rejected:

- **Agent chooses via a display tool** (`show_corrections([...])`).
  Precise, but depends on the model remembering to call it, which varies
  by provider — the exact class of unreliability that produced two
  Gemini outages this week.
- **Model emits structured output alongside prose.** Costs a round trip
  and asks the model to do two jobs at once, adding a per-provider
  failure mode.

Attaching tool results is deterministic, costs no extra tokens, works on
every provider, and needs no cooperation from the model.

**Accepted cost:** the widget can show more than the prose discusses —
the agent may pull 50 words and mention 3. Display-only rendering makes
that cheap rather than wrong.

## The widgets

Each payload is the tool's existing result, copied verbatim.

| Media type | Source tools | Payload |
|---|---|---|
| `application/vnd.jlp.correction+json` | `get_recent_errors`, `get_correction_history` | `[{id, original, replacement?, type, severity, explanation_en?, status?, attempts?, confidence?}]` plus `spans` (see below) |
| `application/vnd.jlp.vocabulary+json` | `get_vocabulary_history`, `search_vocabulary` | `[{expression, reading?, meaning?, kind, lookups, productions}]` |
| `application/vnd.jlp.priorities+json` | `get_learning_priorities` | `[{subject_type, subject, score, reason}]` |
| `application/vnd.jlp.lesson+json` | `create_lesson_plan` | `{id, status, plan}` — `plan` is already `lesson_plan.v1` |

Media types are versioned by their `vnd.jlp.<thing>` name and carry
`+json` so a generic client can still parse the value as data. An old
extension build will meet a newer server; an unknown type degrades to
prose rather than breaking.

## The Socratic gate — load-bearing

`internal/tools.redactIfGated` blanks `replacement` and `explanation_en`
for any correction still under the gate (PRD §9/§53), and it runs in the
**tool layer**, before the model sees anything.

Because widgets are built from tool results, **a widget cannot leak an
answer the learner has not earned** — it copies an already-redacted
payload.

That property is fragile in one specific way. The obvious future
"improvement" is to enrich a widget by reading the repository directly,
to recover the explanation the payload is missing. That bypasses the
gate and makes this the ninth surface `IsGated` has leaked through.

**Rule: widgets copy tool results and never query storage.** Pinned by a
test that fails if a gated correction's `replacement` or `spans` reach a
data part.

### Two visual states for a diff

```
ungated                          gated (answer withheld)
┌───────────────────────────┐    ┌───────────────────────────┐
│ 昨日、映画を -[見] +[見に] │    │ 昨日、映画を [見] 行った   │
│              行った        │    │              ~~~~~        │
│ 助詞 · minor              │    │ 助詞 · minor              │
│ "…so 見 must be 見に"      │    │ まだ答えは出ていません     │
└───────────────────────────┘    │ 2回挑戦中                 │
                                 └───────────────────────────┘
```

The gated state marks *where* the problem is and says the answer is
still being worked out. It must not render as an empty diff, which would
read as "no correction needed" and invert the meaning.

## The one exception to "verbatim"

`correctionView` carries `original` and `replacement` as plain strings.
Something must compute which characters changed:

- **In JS, client-side:** a second diff implementation, in another
  language, that must agree with `internal/domain/diff` on Japanese
  text. Diff algorithms disagree at exactly the boundaries that matter
  (okurigana, particles), and two sources of truth for "what changed"
  is how one correction renders two different ways.
- **In Go, server-side:** the correction data part carries an extra
  `spans` field produced by the *same* `domain/diff` the HTML UI uses.

**Decision: server-side.** One diff implementation, already tested
against Japanese.

It composes with the gate: a gated correction has no `replacement`, so
it gets **no spans at all**. Diffing against an empty string would render
the whole sentence as deleted — both wrong, and a tell that something
was withheld.

## Rendering

A standalone ES module, `clients/a2a-chat/public/widgets.js`, holding a
registry keyed by media type: `render(mediaType, payload) → HTMLElement |
null`. The chat client imports it; the extension project will import the
same file.

This preserves a claim the client's README makes — *"nothing about it is
JLP-specific"*. A media-type registry keeps that true: the client renders
what it recognises and falls back otherwise, and the JLP widgets are four
registered entries. Pointed at another agent, it behaves exactly as
today.

### Failure behaviour

A widget must never cost the reader the answer.

| Situation | Behaviour |
|---|---|
| Unknown media type | Today's `[structured data]` text fallback |
| Payload does not match the expected shape | Skip that widget, keep the prose |
| Empty array | Render nothing (not an empty box) |
| Renderer throws | Catch, log to console, keep the prose |

The prose part renders first and unconditionally, in every case.

## Testing

Go, where the risk is:

- Data parts are emitted for renderable tools, deduplicated to the last
  call of each.
- **A gated correction never reaches a data part carrying `replacement`,
  `explanation_en`, or `spans`.** The ninth-surface guard.
- The prose part is always present, whatever else is attached.
- An unrenderable tool result produces no data part rather than an empty
  one.

JS: the chat client has no test harness, so widgets get browser
verification against the running stack — including one deliberately
malformed payload, to prove the prose survives it.

## Out of scope

- Interactivity. Accepting or rejecting a correction stays in JLP, where
  the gate and the learning events live. Widgets are display-only.
- Rendering these widgets inside JLP's own UI, which already renders all
  four natively.
- The Chrome extension, which is the next project and will reuse
  `widgets.js`.
