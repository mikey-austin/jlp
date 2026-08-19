# POST /api/v1/words — bulk vocabulary ingestion

This endpoint lets an external reader app sync its own vocabulary deck
into a learner's JLP vocabulary catalog. It implements Nihongo Daily's
`/api/v1/words` contract verbatim (see that project's
`doc/openapi.yaml` — the `IngestWordsRequest`, `WordInput`,
`IngestWordsResponse`, `ErrorResponse`, and `JLPTLevel` schemas): field
names, the `200` (not `201`) status, and the response/error shapes are
pinned by that contract and will not change without breaking every
caller.

## Request

```
POST /api/v1/words
Content-Type: application/json
```

```json
{
  "words": [
    {
      "kanji": "勉強",
      "reading": "べんきょう",
      "meaning": "学ぶこと、学習すること",
      "meaning_en": "studying",
      "jlpt_level": 3,
      "tags": ["education", "noun"],
      "source": "Anki Deck",
      "example": "毎日日本語の勉強をしています。"
    },
    {
      "kanji": "猫",
      "reading": "ねこ",
      "meaning": "cat animal",
      "source": "Anki Deck"
    },
    {
      "kanji": "犬",
      "reading": "いぬ",
      "meaning": "dog animal"
    }
  ]
}
```

Per word:

| Field        | Required | Notes                                                                 |
|--------------|----------|------------------------------------------------------------------------|
| `kanji`      | yes      | The expression itself; may be kana-only for a kana-only word.          |
| `reading`    | yes      | Furigana / pronunciation.                                              |
| `meaning`    | yes      | Japanese definition.                                                   |
| `meaning_en` | no       | English gloss.                                                         |
| `jlpt_level` | no       | Integer `0`–`5`. `0` = unknown (the default when omitted), `5` = N5 (easiest), `1` = N1 (hardest) — the same convention JLP already uses for grammar concepts. |
| `tags`       | no       | Free-form strings (e.g. `["education", "noun"]`).                      |
| `source`     | no       | Free text describing where the word came from (e.g. `"Anki Deck"`).    |
| `example`    | no       | A sentence the word was met in. See below — this is the one optional field that changes what the learner is shown. |

### `example` is worth sending

JLP's 練習 page drills a word **in a sentence** — the sentence with the
word blanked out, choose which expression fills the gap — and shows the
intact sentence, with the word emphasised, once the answer is in.

A word with no sentence gets one written for it by a model. That works,
and it is strictly worse than the real thing: the sentence you send is
the one the learner actually read, in the context that made them save
the word. A generated sentence has to invent a context, and inventing
one is how a word ends up remembered attached to something that never
happened.

Sent examples win over generated ones, and a later send replaces an
earlier sentence for the same word. Words already imported without one
are unaffected until they are sent again.

`kanji`/`reading`/`meaning` must be non-empty after trimming
whitespace for every word in the batch, and `jlpt_level` (when given)
must be `0`–`5`. A batch is validated **as a whole** before anything is
written: if any single word is invalid, the entire request is rejected
and nothing in the batch is imported.

**Batch limit:** at most **1000 words** per request. The request body
itself is capped at **4 MiB**.

## Response

```
200 OK
```

```json
{ "imported": 3 }
```

`imported` is how many rows were created-or-updated by this batch —
not necessarily distinct expressions if the batch itself repeats one.

## Errors

```
400 Bad Request
```

```json
{ "error": "words[1]: kanji is required" }
```

Possible `error` messages:

- A missing/blank required field on word at index *i*: e.g.
  `"words[1]: kanji is required"`, `"words[0]: reading is required"`,
  `"words[2]: meaning is required"`.
- An out-of-range `jlpt_level`: `"words[0]: jlpt_level must be between 0 and 5"`.
- An empty `"words"` array: `"words is required and must not be empty"`.
- More than 1000 words: `"too many words in one request (max 1000)"`.
- Malformed JSON: `"malformed request body"`.
- A request body over 4 MiB: `"request body too large"`.

## Upsert and counter semantics

Words are upserted by **(identity, kanji)** — re-importing the same
expression updates the existing row rather than creating a duplicate.
This all happens in **one database transaction** per request: either
the whole batch lands, or none of it does.

**Counters are never touched by this endpoint.** JLP tracks
`lookups`/`productions`/`successful_productions` per vocabulary item as
signals of the learner's own engagement (looking a word up again,
using it in their own writing). Importing a deck is not a lookup or a
production, so those three counters start at `0` on first import and
are left completely alone on every re-sync — even if the item already
has real usage history from the learner using JLP directly, this
endpoint will never reset or increment it.

**A sparse re-sync will not erase richer data.** If you sync a word
once with `meaning_en`, `jlpt_level`, and `tags` set, then later
re-sync the same word with only `kanji`/`reading`/`meaning` (e.g.
because your deck export doesn't always carry every field), the
missing fields are **left as they were**, not blanked out:

- An empty incoming string field (`reading`, `meaning`, `meaning_en`,
  `source`) never overwrites an existing non-empty value. A non-empty
  incoming value **does** overwrite.
- `jlpt_level: 0` (or omitted) never overwrites a previously known
  level.
- `tags` is the one exception: a **non-empty** incoming `tags` array
  replaces the stored list wholesale (it is not merged/unioned with
  what's already there). An empty/omitted `tags` leaves the stored
  list untouched.

## Example

```sh
curl -X POST http://localhost:28080/api/v1/words \
  -H "Content-Type: application/json" \
  -d '{
    "words": [
      {"kanji":"勉強","reading":"べんきょう","meaning":"学ぶこと、学習すること",
       "meaning_en":"studying","jlpt_level":3,"tags":["education","noun"],
       "source":"Anki Deck"},
      {"kanji":"猫","reading":"ねこ","meaning":"cat animal","source":"Anki Deck"},
      {"kanji":"犬","reading":"いぬ","meaning":"dog animal"}
    ]
  }'
# => {"imported":3}
```

A follow-up sync with only the required fields for 勉強 preserves the
`meaning_en`/`jlpt_level`/`tags` set above:

```sh
curl -X POST http://localhost:28080/api/v1/words \
  -H "Content-Type: application/json" \
  -d '{"words":[{"kanji":"勉強","reading":"べんきょう","meaning":"学ぶこと、学習すること"}]}'
# => {"imported":1}
```

Imported words appear on the `/vocabulary` page (English gloss and
tags show alongside the Japanese meaning when present) and the sync
records exactly **one** `vocabulary.imported` learning event per batch
(never one per word), carrying `{"count": N, "sample": [...up to 5
imported expressions]}` as evidence.

## Authentication

This endpoint sits behind the same identity middleware as every other
route in JLP — there is **no dedicated service-token scheme** for
external callers, and building one is explicitly out of scope for this
endpoint.

- **`APP_AUTH_MODE=static`** (the default for local/LAN dev): every
  request is treated as the single configured identity with no
  credentials required, so this endpoint works unauthenticated out of
  the box. This is the mode a self-hosted, LAN-only Nihongo Daily
  instance should expect to run against.
- **Authelia mode**: requests must either carry a valid Authelia
  session, or the deployment must add an explicit Authelia bypass rule
  for `POST /api/v1/words` for the calling app's source. Without one
  of those two, calls from an external app will be rejected before
  ever reaching this handler.

If you need per-app credentials distinct from the platform's own
identity/session model, that is a separate piece of work — track it
before relying on this endpoint from a multi-tenant or public-facing
deployment.
