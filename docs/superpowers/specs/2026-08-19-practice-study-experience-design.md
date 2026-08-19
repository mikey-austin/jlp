# 練習: a study experience, not a form

Rebuilds `/practice` into something worth opening: drills that include
the words you actually collected, failures you can see, and enough
feedback to know the thing is working.

`/learner` (学習) is deliberately untouched. It is a read-only overview
and stays one.

## What is wrong today

**The button can do nothing, silently.** htmx does not swap a non-2xx
response. `practiceStart` answers any failure with a bare
`http.Error(w, "could not start practice", 500)` and logs nothing at
all. So a failed generation leaves the page byte-identical and the
server without a trace — indistinguishable from a dead button, and
impossible to diagnose afterwards.

**The progress treatment exists but only one button can use it.**
`app.js` already renders a spinner, the adapter's name and a live
elapsed-seconds timer on `htmx:beforeRequest`, and the error status and
body on `htmx:responseError`. It is bound to `#feedback-btn` by id. A
5–30 second model call behind 練習する gets none of it.

**Provider selection likewise.** `aiProviderOptions` and the
`provider_override` round trip exist only on the workspace page.

**Vocabulary is never drilled.** `Service.Start` goes due-concept →
top-concept → random, and `dueConcept` keeps only
`SubjectType == "concept"`, discarding expressions. Every word the
learner collected is unreachable from 練習.

**It looks like a form because it is one.** No sense of what is being
drilled, how far through you are, or how you are doing.

## What gets drilled, and in what order

```
1. recently added words     newest first
2. SRS due items            most overdue first   (concepts AND words)
3. top priority concept
4. random catalog concept
```

Recency leads because a word added this week still has the context that
produced it attached — the sentence it came from, why it was looked up.
That is the moment it is cheapest to learn. SRS is the right ordering
for everything past that, and stays exactly as it is underneath.

A word is "recent" if it was added inside `recentWordWindow` (14 days)
and has not yet been answered correctly. Both bounds matter: without
the window every drill is a word forever, and without the second the
same word repeats until the SRS interval catches up.

`dueConcept`'s `SubjectType == "concept"` filter is removed rather than
widened — expressions become drillable through the same door.

## Exercise gains a subject

`Exercise.ConceptSlug` cannot name a word. Rather than overload it,
exercises gain:

- `SubjectType` — `"concept"` or `"word"`
- `SubjectRef` — the concept slug or the vocabulary entry id

`ConceptSlug` stays, meaning what it always meant, so nothing that reads
it changes. Migration backfills `subject_type='concept'`,
`subject_ref=concept_slug`.

## Failure becomes visible

Two halves, because the failure has two audiences.

**Server:** `practiceStart` and `practiceAnswer` log the underlying
error — identity, subject, requested provider — before answering. The
handler's HTTP text stays generic; the log carries the cause.

**Client:** the progress/error block in `app.js` stops naming
`#feedback-btn` and reads a data attribute instead:

```html
<button data-progress-into="#exercise-area"
        data-progress-label-from="#provider-override">
```

Any htmx trigger opts in by declaring where its progress goes. The
workspace button keeps its behaviour by declaring the same thing; the
hard-coded id is what let 練習 ship without it, and a second copy of the
same code would be a second thing to forget.

## Provider selection

The workspace's exact pattern, moved to where both can use it: the
`provider_override` select, validated against `s.opts.AIProviders` with
`isKnownAIProvider` (400 on unknown), threaded through
`practice.Service.Start` into `drill.GenerateInput.ProviderOverride` and
on to the router. Per-request only; it never persists and never touches
/settings.

## The four widgets

### 1. Session progress and streak

A run is 10 questions. The exercise partial carries `n` and `streak`
forward through `hx-vals`, so the run needs no server-side state and no
schema: position is a property of the exchange, not of the learner.

Rendered as a progress bar and a streak counter above the question, and
an end-of-run summary partial (`practice_summary`) with what was
drilled and what to do next.

### 2. Instant answer diff

A wrong typed answer is shown against `Exercise.Answer` as a
character-level diff, computed with `internal/domain/diff.Runes` and
rendered with the same `.d-ins`/`.d-del` spans the correction card
already uses. One diff implementation, one rendering, one set of rules
about Japanese.

Only for types with a canonical answer. `free-production` has none and
keeps its model-written feedback.

### 3. Flip-card word drill

Exercise type `word-recall`: the word on the front, reading and meaning
on the back, revealed on click. Self-graded できた / もう一度, which
feeds the retrieval scheduler exactly as a typed answer does.

Self-grading is the honest shape for recall — only the learner knows
whether they actually recalled it — and it is what makes a word drill
cheap enough to be worth doing daily.

### 4. Typing with live kana check

For reading drills, characters turn green as they match the expected
reading, instead of waiting for submit.

**This puts the answer in the DOM.** That is acceptable here and
nowhere else: these are self-study drills whose neighbouring widget is
already self-graded, so honesty is assumed by construction. It must
never be extended to anything under the socratic gate
(`domain/correction.IsGated`), where withholding the answer is the
entire point — see `internal/tools.redactIfGated` and the ninth-surface
guards. A gated correction has no business in a drill payload at all.

## Look

`.practice` becomes a real layout rather than an unstyled wrapper: the
subject as a chip (which word, which concept), the progress bar, the
question at a readable size, and the confidence control sized to its
content instead of spanning the card. Existing tokens and components
only — no new colour, no new spacing scale.

## Testing

- selection order, as a table: recent word beats due concept beats top
  concept beats random, and each falls through when empty
- the recency window and the not-yet-correct bound, both directions
- a failing generation logs the cause and answers 500 (the silent-
  failure regression)
- unknown `provider_override` is a 400, known one reaches the agent
- run position and streak survive the round trip; streak resets on a
  wrong answer
- the diff is not rendered for `free-production`
- a gated correction never reaches a drill payload
