# 読解: study articles written in other languages — design

**Goal:** send an article in English (or any language) and get a Kindle
study edition of it in Japanese: the article translated into natural
written Japanese, then the usual lesson built on the translation.

**Status:** decided by the controller on the learner's instruction
("auto accept and implement", 2026-09-30); decisions are listed under
Rulings so the learner can overturn any of them.

## Behaviour

- **Automatic.** Today a non-Japanese submission is refused
  (`reading.ErrNotJapanese`, 「日本語の文章ではないようです」). Instead,
  any submission whose text does not look Japanese is accepted and
  translated. No toggle: the one-shot and draft flows, extension, phone
  and paste form all just work.
- **Where.** Translation is the first step of the worker's analysis
  attempt: if the article is not Japanese and has no stored translation,
  translate it, store the translation, then analyse the Japanese. A
  failure is an ordinary failed attempt (retries, backoff, give-up).
  Regenerate reuses the stored translation — never translates twice.
- **What.** A faithful, natural written-Japanese translation at native
  register (not simplified — the lesson is where the learner's level
  applies): the same paragraph breaks one-for-one, the title translated,
  names in their usual Japanese form. The model also names the source
  language. Prompt `reading.translate` v1, schema `reading_translation.v1`
  (`source_language` as a Japanese name like 英語, `title`, `paragraphs`
  array — **no `maxItems`**, which Gemini rejects), through the routed,
  observed generator with `aiutil.ValidateWithRepairAndRetry`; a semantic
  check requires the same number of non-empty paragraphs as the original
  (a mismatch is a repairable error).
- **Stored on the article.** New columns (migration 00033):
  `original_language text NOT NULL DEFAULT 'ja'`, `original_title text`,
  `original_paragraphs jsonb`. At ingestion a non-Japanese article is
  stored with `original_language = 'und'` and its own text in
  `paragraphs`. When translated, `paragraphs` and `title` become the
  Japanese, the source text moves to `original_title`/
  `original_paragraphs`, and `original_language` gets the model's
  language name. So every renderer, the lesson, furigana, figures
  (anchored by paragraph index, which the one-for-one rule preserves) and
  the Kindle filename work unchanged on Japanese text.
- **Dedupe** stays on the content hash of the submitted text: the same
  English article twice is one article.
- **The original in the book.** The EPUB's byline reads 「{言語}から翻訳」
  and a final chapter 「原文」 holds the original title and paragraphs
  (after the answers, so it never spoils the reading). The web page shows
  the same byline and a collapsed 「原文」 section.
- **Limits.** `MaxArticleRunes` applies to the submitted (original)
  text. The translate call's MaxTokens is 16384.
- **Routing.** `reading.translate` joins `APP_AI_ROUTES` in the deploy
  playbook as `gemini,ollama`, like the other prompts.

## Failure handling

| Situation | Result |
|---|---|
| Translation call fails or keeps returning the wrong paragraph count | attempt fails; retried with backoff; after the attempt budget the edition is failed with the error |
| Model returns Japanese source_language for an article judged non-Japanese | accepted as-is (the text is stored as the translation; harmless) |
| Empty / whitespace submission | still `ErrEmptyContent` |

## Testing

Domain: non-Japanese ingestion accepted and marked `und`; Japanese stays
`ja`; applying a translation swaps paragraphs/title and keeps the
original; paragraph-count mismatch rejected. Agent: fake generator —
valid translation, count mismatch → repair, prompt-injection markers
stripped like `reading.analyse`. Gemini: the new schema translates (no
blockers, no maxItems). Service: an English submission is translated
then analysed; regenerate does not call the translator again; a
Japanese article never calls it. Postgres integration: the new columns
round-trip. Rendering: EPUB 原文 chapter and byline (EPUBCheck clean),
web page byline and 原文. Browser: submit an English article through
the extension shim with the fake provider; production is checked by
the learner with a real article.

## Rulings

1. Automatic for any non-Japanese text; no per-submission switch — cost
   if wrong: a learner wanting to reject non-Japanese text can't.
2. Native-register translation, not simplified — the lesson carries the
   level; cost if wrong: harder reading for a beginner.
3. Original text kept and shown after the answers — cost if wrong: a
   longer book.
4. Translation stored on the article row, replacing `paragraphs` with
   the Japanese — renderers stay untouched; cost if wrong: the original
   must be read from the `original_*` columns everywhere it's wanted.
