# Japanese Output Learning Platform
## Product Requirements Document

**Status:** Draft PRD  
**Version:** 1.0  
**Date:** 14 August 2026  
**Primary user:** Single learner initially, with multi-identity support from day one  
**Primary language:** Japanese learning  
**Primary objective:** Increase the learner's ability and confidence to formulate thoughts naturally in Japanese, especially for written and eventually spoken output.

---

# 1. Executive Summary

This product is a **personal Japanese learning platform centered on active language production**.

The core idea is simple:

> **The learner should spend more time actually using Japanese, while the system quietly turns that usage into personalized learning.**

Rather than separating "studying Japanese" from "using Japanese," the platform connects:

- writing real Japanese,
- receiving intelligent corrections,
- understanding why corrections were necessary,
- reading Japanese,
- looking up vocabulary in authentic contexts,
- reviewing weak areas,
- practicing retrieval,
- interacting with an AI teacher,
- preparing for conversations with human tutors,
- generating Anki material,
- and eventually practicing spoken Japanese.

All of these activities contribute data to a persistent **Learner Model**.

The AI teacher uses that model implicitly to adapt its behavior. The learner should not need to decide:

> "What grammar should I study today?"

Instead, the system should determine that from accumulated evidence and naturally incorporate it into writing feedback, drills, quizzes, explanations, conversations, and review activities.

The product therefore has two major modes:

### 1.1 Writing Aid

The learner writes something useful in Japanese:

- a blog post,
- message to a friend,
- journal entry,
- response,
- essay,
- work-related text,
- or anything else.

They select a passage and request feedback.

The system:

1. identifies problems,
2. produces structured corrections,
3. explains them,
4. teaches the relevant concepts,
5. preserves the original writing,
6. shows a visual diff,
7. records learning events,
8. updates the learner model,
9. and uses those observations to improve future teaching.

### 1.2 Learning Platform

When the learner wants more deliberate practice, the platform can generate:

- grammar exercises,
- vocabulary drills,
- retrieval exercises,
- mini quizzes,
- reading comprehension activities,
- "explain this in Japanese" exercises,
- sentence formulation exercises,
- conversation scenarios,
- review snippets,
- personalized challenges,
- and Anki cards.

These activities are driven by the same Learner Model.

The result is a continuous loop:

**Read → Encounter → Look up → Understand → Write → Get feedback → Correct → Practice → Reuse → Improve**

---

# 2. Product Vision

## 2.1 Vision

Build a personal Japanese teacher that gets progressively better at understanding:

- what the learner knows,
- what the learner thinks they know,
- what they repeatedly get wrong,
- what they are currently learning,
- what vocabulary they encounter,
- what they can produce independently,
- and what areas are becoming automatic.

The system should make the learner **more independent over time**, not more dependent on AI.

## 2.2 North Star

> **Make authentic Japanese use the primary learning activity, and make every interaction with Japanese an opportunity for improvement.**

## 2.3 Learning philosophy

The product should optimize for:

1. **Production over passive recognition**
2. **Retrieval over rereading**
3. **Explanation over simple correction**
4. **Authentic context over artificial vocabulary lists**
5. **Gradual reduction of assistance**
6. **Repeated exposure to persistent weaknesses**
7. **Useful real-world writing**
8. **Low friction**
9. **Long-term adaptation**
10. **Learner independence**

---

# 3. Goals

## 3.1 Primary goals

The system should help the learner:

- formulate thoughts in Japanese more naturally;
- improve Japanese word order and sentence structure;
- reduce recurring grammatical mistakes;
- improve naturalness and idiomatic expression;
- actively use vocabulary encountered through reading;
- increase confidence in producing Japanese;
- transfer writing ability into spontaneous spoken formulation;
- understand recurring weaknesses without manually tracking them;
- maintain motivation through interesting, useful activities;
- build a long-term picture of Japanese development.

## 3.2 Secondary goals

The platform should:

- integrate with external reading/vocabulary applications;
- support Anki;
- support human language tutors;
- provide personalized lesson preparation;
- expose useful statistics;
- support multiple AI providers;
- allow local AI execution;
- support multiple communication channels;
- provide high-quality structured AI output;
- make AI provider quality measurable;
- be extensible without modifying the core domain.

## 3.3 Non-goals

Initially, the system is **not** intended to be:

- a generic Japanese dictionary;
- a replacement for human tutors;
- a conventional textbook;
- a social network;
- a gamified streak-based language application;
- an AI chatbot with no persistent learning model;
- an Anki clone;
- a general-purpose writing editor.

---

# 4. Product Principles

## 4.1 Authenticity

The learner should be encouraged to write things they genuinely want to communicate.

A real blog post is preferable to:

> "Write five sentences using ～わけではない."

The latter can still be generated when appropriate, but should arise from identified learning needs.

## 4.2 Frictionless feedback

Getting feedback should be extremely cheap.

The ideal interaction is:

1. write;
2. highlight;
3. click feedback;
4. learn.

No copying text into another application.

## 4.3 Teach, don't merely correct

A correction should answer:

- What was wrong?
- What would a native speaker naturally say?
- Why?
- What concept does this illustrate?
- Can I use it myself?

## 4.4 Preserve learner agency

The learner remains the author.

The system should distinguish:

- **incorrect**
- **unnatural**
- **possible but stylistically unusual**
- **acceptable**
- **excellent**

It should not imply that every difference from the model's preferred wording is an error.

## 4.5 Assistance should fade

If the learner repeatedly demonstrates mastery, the system should reduce attention to that area.

If the learner repeatedly struggles, assistance should increase.

## 4.6 The AI does not own the learner model

The backend owns:

- learner state,
- events,
- statistics,
- sessions,
- corrections,
- vocabulary,
- observations,
- identity,
- permissions.

AI agents reason over that data through explicit tools.

---

# 5. Target User Experience

## 5.1 Primary writing interface

The primary PWA should have three major areas.

### Left: Writing area

The learner writes Japanese.

Capabilities:

- rich text or plain text editing;
- autosave;
- Japanese IME support;
- selection;
- keyboard shortcuts;
- version/history;
- feedback actions;
- session context.

### Right: Session/context panel

Shows:

- active session;
- previous sessions;
- session purpose;
- session context;
- conversation with teacher;
- current learning focus;
- feedback history.

A session is explicitly created by the learner.

Examples:

- "Japanese blog post"
- "Reply to Ken"
- "旅行について書く"
- "Practice casual Japanese"
- "Work diary"

### Feedback panel

When text is selected, the system can show:

- original;
- corrected version;
- inline diff;
- individual corrections;
- grammar explanations;
- naturalness explanation;
- examples;
- Japanese explanation;
- English explanation;
- optional hints;
- follow-up exercise.

---

# 6. Sessions

A **Session** is the central contextual unit for writing.

The learner explicitly creates it.

A session contains:

- identity;
- title;
- purpose;
- writing content;
- conversation history;
- selections;
- feedback;
- corrections;
- contextual instructions;
- teaching preferences;
- learning events;
- session-level observations.

## 6.1 Session purpose

Examples:

```text
Blog post
Message to friend
Diary
Formal email
Casual conversation practice
Business Japanese
Creative writing
JLPT practice
Tutor preparation
```

## 6.2 Session profile

A session may define:

```text
Purpose
Audience
Relationship
Tone
Register
Topic
Desired style
Teacher mode
Explanation language
Correction strictness
Learning objectives
```

The learner should be able to override defaults.

---

# 7. Teacher Profiles

The platform should support configurable teacher personas.

The default is:

> **Japanese Teacher**

The teacher should prioritize learning over merely producing polished Japanese.

Potential modes:

### Teacher

Explains mistakes and guides improvement.

### Strict Corrector

Focuses primarily on correctness.

### Naturalness Coach

Focuses on native-like expression and collocation.

### Conversation Tutor

Uses interactive dialogue.

### Socratic Teacher

Provides hints before answers.

### Exam Coach

Optimizes for JLPT-style performance.

### Writing Editor

Provides polished prose while clearly separating editorial changes from actual language errors.

Teacher profiles are configuration/data, not hardcoded application behavior.

---

# 8. Correction Workflow

## 8.1 User selects text

Example:

```text
昨日友達と映画を見に行って、とても面白いでした。
```

## 8.2 User requests feedback

The backend creates a feedback request.

The teacher receives:

- selected text;
- surrounding context;
- session purpose;
- previous conversation;
- relevant learner context;
- current teaching priorities.

## 8.3 AI returns semantic correction data

The AI should **not** be responsible for calculating the final visual diff.

It returns structured correction proposals.

Example conceptual schema:

```json
{
  "original": "とても面白いでした",
  "corrections": [
    {
      "original": "面白いでした",
      "replacement": "面白かったです",
      "type": "grammar",
      "severity": "incorrect",
      "explanation": {
        "ja": "...",
        "en": "..."
      }
    }
  ]
}
```

The Go backend then computes the deterministic diff.

## 8.4 Feedback types

Corrections should support:

- grammar;
- particle;
- conjugation;
- word order;
- vocabulary;
- collocation;
- register;
- politeness;
- naturalness;
- ambiguity;
- punctuation;
- spelling;
- kanji;
- stylistic choice.

## 8.5 Correction states

A correction can be:

```text
incorrect
unnatural
less-natural
style
optional
excellent-alternative
```

This distinction is important for preventing overcorrection.

---

# 9. Active Recall Mode

The teacher should sometimes avoid immediately revealing the answer.

Example:

> Your sentence has a problem with the adjective ending. Can you try correcting it?

Possible interaction:

1. learner writes;
2. teacher gives hint;
3. learner retries;
4. teacher evaluates;
5. teacher reveals answer if necessary.

The system records:

- whether learner solved independently;
- number of hints;
- number of attempts;
- eventual correctness.

This becomes valuable learner-model evidence.

---

# 10. Grammar Knowledge

Grammar should be represented as structured learning concepts.

Example:

```text
Grammar Concept
  ID
  Name
  JLPT level
  Description
  Examples
  Related concepts
  Prerequisites
```

A correction can reference one or more concepts.

For example:

```text
食べる → 食べた
```

might map to:

```text
past-tense conjugation
```

The learner model can then aggregate performance against concepts rather than simply counting raw strings.

---

# 11. Vocabulary System

Vocabulary is broader than individual words.

The system should support:

- words;
- expressions;
- collocations;
- grammatical constructions;
- fixed phrases;
- sentence patterns.

Each vocabulary item may contain:

```text
Expression
Reading
Meaning
Part of speech
JLPT level
Example sentences
Source
Context
First encountered
Lookup count
Production count
Successful production count
Last reviewed
Confidence
```

## 11.1 Reading ingestion

External applications may send:

- looked-up word;
- definition;
- example sentence;
- surrounding text;
- source/book;
- timestamp.

This information becomes part of the learner's reading history.

## 11.2 Active vocabulary

The platform should distinguish:

**Seen**

The learner encountered it.

**Looked up**

The learner explicitly needed help.

**Reviewed**

The learner deliberately practiced it.

**Produced**

The learner used it.

**Produced correctly**

The learner used it successfully.

This allows the teacher to identify:

> "You recognize this expression but don't seem to produce it."

---

# 12. Reading Integration

The system should expose a clean ingestion port for external reading applications.

Example event:

```json
{
  "type": "vocabulary.lookup",
  "expression": "取り組む",
  "definition": "...",
  "example": "...",
  "source": {
    "type": "novel",
    "title": "..."
  }
}
```

The source application should not need to know anything about the learner model.

It simply emits information through an adapter/API.

---

# 13. Learner Model

The Learner Model is the central intelligence substrate.

It should represent:

### Strengths

Things the learner consistently demonstrates.

### Weaknesses

Persistent problems.

### Emerging skills

Recently improving areas.

### Vocabulary exposure

What the learner encounters.

### Production ability

What the learner actually uses.

### Confidence

How confident the learner reports being.

### Error patterns

Recurring mistakes.

### Learning history

What interventions have occurred.

### Preferences

Preferred teaching style, explanation language, etc.

---

# 14. Learning Events

Everything important should generate an immutable learning event.

Examples:

```text
WritingCreated
WritingUpdated
FeedbackRequested
CorrectionPresented
CorrectionAccepted
CorrectionRejected
CorrectionRetried
GrammarConceptEncountered
GrammarConceptMastered
VocabularyLookedUp
VocabularyReviewed
VocabularyProduced
VocabularyProducedCorrectly
QuizStarted
QuizAnswered
QuizCompleted
HintRequested
HintUsed
ConfidenceRecorded
TutorLessonStarted
TutorLessonCompleted
AnkiCardCreated
AnkiCardReviewed
```

Events are the raw learning history.

Derived learner state should be rebuildable from events where practical.

---

# 15. Learner Analytics

The dashboard should show useful information without turning the application into a spreadsheet.

Potential metrics:

### Writing

- words written;
- sessions;
- feedback requests;
- corrections;
- corrections per 1,000 characters;
- independent corrections;
- recurring errors.

### Grammar

- strongest concepts;
- weakest concepts;
- improving concepts;
- persistent concepts;
- recently encountered concepts.

### Vocabulary

- words encountered;
- words looked up;
- words reviewed;
- words produced;
- words successfully produced;
- expressions becoming active.

### Learning behavior

- writing frequency;
- reading activity;
- retrieval activity;
- review completion;
- confidence trends.

### AI feedback

- response ratings;
- provider;
- model;
- capability;
- latency;
- cost;
- acceptance rate.

---

# 16. Adaptive Teaching Engine

The platform should have a dedicated teaching-planning component.

It should answer:

> **What would be most useful for this learner right now?**

Inputs:

- learner model;
- recent events;
- current session;
- current text;
- historical weaknesses;
- recent improvements;
- vocabulary exposure;
- confidence;
- activity history.

Outputs:

```text
Current priorities
Recommended interventions
Concepts to reinforce
Concepts to stop emphasizing
Vocabulary worth activating
Suggested exercise types
```

The first implementation should be deterministic/heuristic where possible.

For example:

```text
persistent_error × recent_frequency × learning_value
```

can produce a priority score.

AI can subsequently refine the recommendation.

---

# 17. Learning Modules

## 17.1 Writing Coach

Primary module.

Capabilities:

- correction;
- naturalness feedback;
- grammar explanation;
- vocabulary suggestions;
- active recall;
- rewrite comparison;
- follow-up exercises.

## 17.2 Drill Engine

Generates targeted exercises from learner weaknesses.

Exercise types:

- fill-in-the-blank;
- sentence transformation;
- multiple choice;
- free production;
- translation;
- correction;
- explanation;
- ordering;
- contextual choice.

## 17.3 Explain-in-Japanese

The learner reads a sentence/concept and explains it in Japanese.

The teacher evaluates:

- understanding;
- vocabulary;
- grammar;
- naturalness;
- conceptual accuracy.

This deliberately combines comprehension with production.

## 17.4 Conversation Tutor

Interactive conversation.

The system should prioritize natural dialogue rather than constant interruption.

Feedback can be:

- immediate;
- delayed;
- end-of-conversation.

## 17.5 Vocabulary Activator

Finds words the learner has encountered but rarely produced.

It creates opportunities to use them naturally.

## 17.6 Lesson Generator

Creates lesson plans for:

- self-study;
- AI conversation;
- human tutor sessions.

---

# 18. Human Tutor Integration

The platform should generate a shareable **Lesson Guide**.

Example contents:

```text
Learner
Current level
Recent strengths
Current weaknesses
Recommended focus
Vocabulary to practice
Grammar concepts
Conversation prompts
Suggested exercises
Recent examples
Questions for tutor
```

After the lesson, the tutor or learner can record observations.

These become learning events.

This creates a loop:

**AI analysis → tutor lesson → tutor observations → learner model → future AI teaching**

---

# 19. Anki Integration

Anki is an optional review channel.

The system should support an **Anki Card Generator Agent**.

Card sources may include:

- writing corrections;
- recurring grammar problems;
- useful expressions;
- vocabulary from novels;
- tutor lessons;
- successful examples.

Cards should favor contextual material over isolated translations.

Example:

```text
Front:
昨日の旅行について書いた文章で、
「楽しいでした」と書いた。

何が不自然？

Back:
「楽しかったです」

理由:
い-adjective past tense uses ～かった.
```

The learner should be able to:

- approve;
- edit;
- reject;
- batch approve;
- export.

Possible adapters:

- AnkiConnect;
- file export;
- future Anki API mechanisms.

---

# 20. Communication Channels

All channels should ultimately invoke the same application capabilities.

Supported/desired channels:

- PWA;
- Chrome extension;
- REST API;
- Slack;
- Signal;
- WhatsApp;
- email;
- future mobile application.

The channel must be an adapter.

The domain must not know whether an interaction originated in Slack or the PWA.

## 20.1 Channel-aware interaction

The same teacher can behave differently depending on channel.

### PWA

Long-form writing and detailed explanations.

### Chrome extension

Short contextual feedback.

### Messaging

Small exercises and conversational interaction.

### Email

Periodic summaries.

---

# 21. Weekly Email Summary

The system can generate a concise weekly digest.

Contents:

- what you accomplished;
- biggest improvements;
- persistent weaknesses;
- useful new expressions;
- words successfully activated;
- recommended focus;
- optional next week's challenge.

The summary should encourage rather than judge.

---

# 22. AI Architecture

The AI subsystem must be provider-independent.

The core should depend on **capabilities**, not vendors.

Example conceptual port:

```go
type LanguageModel interface {
    Generate(ctx context.Context, request Request) (Response, error)
}
```

However, the architecture should evolve toward capability-specific interfaces rather than one giant abstraction.

Examples:

```text
TextGeneration
StructuredGeneration
ToolCalling
Embedding
Vision
SpeechRecognition
SpeechSynthesis
```

---

# 23. AI Adapters

Potential adapters:

```text
OpenAI
Anthropic
Gemini
Ollama Local
Ollama Cloud
Claude CLI
Codex CLI
Antigravity CLI
```

CLI adapters are legitimate first-class adapters.

They allow existing locally authenticated tools to participate without the application having to implement every provider protocol itself.

---

# 24. AI Routing

A routing layer selects an adapter based on policy.

Example:

```text
Writing correction:
  preferred = Gemini
  fallback = Claude CLI

Deep teaching analysis:
  preferred = Claude
  fallback = OpenAI

Local/private:
  preferred = Ollama

Cheap classification:
  preferred = local model
```

Policies should be configuration-driven.

The application should be able to change providers without changing domain code.

---

# 25. AI Observability Decorator

Every AI capability invocation should pass through an observability decorator.

Record:

```text
request ID
identity
session
capability
provider
model
timestamp
latency
input token estimate
output token estimate
cost estimate
success/failure
tool calls
response ID
user rating
```

Potential user feedback:

```text
1–5 stars
```

or:

```text
Useful
Not useful
```

The richer rating should be optional.

---

# 26. AI Quality Dashboard

Dashboard views should include:

### Provider comparison

```text
Provider
Model
Capability
Requests
Average rating
Success rate
Latency
Estimated cost
```

### Personal preference

Show which models the learner tends to prefer.

### Capability performance

For example:

```text
Writing correction:
Claude 4.1 — 4.7
Gemini — 4.4
Ollama — 3.8
```

The exact values are illustrative only.

### Quality over time

Track whether model performance changes after prompt/version changes.

---

# 27. Agents

Agents should be narrow and composable.

Potential agents:

### Teacher Agent

Primary orchestrator.

### Writing Reviewer Agent

Analyzes written Japanese.

### Learner Analyst Agent

Interprets learner data.

### Teaching Planner Agent

Determines useful interventions.

### Vocabulary Agent

Works with reading vocabulary.

### Drill Agent

Creates exercises.

### Conversation Agent

Conducts dialogue.

### Lesson Planner Agent

Creates tutor lesson guides.

### Anki Agent

Creates review cards.

### Progress Analyst Agent

Generates longer-term analysis.

The agent does not own persistent state.

---

# 28. Tool Architecture

Agents interact with application capabilities through typed tools.

Examples:

```text
get_active_session
get_session_context
get_learner_profile
get_learning_priorities
get_recent_errors
get_grammar_history
get_vocabulary_history
search_vocabulary
get_recent_writing
get_correction_history
record_learning_event
create_exercise
create_lesson_plan
create_anki_card
```

Tools should have explicit schemas and authorization.

Agents should not directly access the database.

---

# 29. Tool Registry

Tools should be registered during application bootstrap.

Conceptually:

```text
Tool Registry
 ├── Learner tools
 ├── Session tools
 ├── Writing tools
 ├── Vocabulary tools
 ├── Learning tools
 ├── Analytics tools
 └── Integration tools
```

This makes modules independently replaceable.

---

# 30. A2A Support

The architecture should support **Agent2Agent (A2A)** communication as an integration boundary.

A2A should be treated as an adapter/protocol layer rather than embedded into the domain.

Example:

```text
Teacher Agent
     |
     +---- Learner Analyst Agent
     |
     +---- Writing Coach Agent
     |
     +---- Vocabulary Agent
     |
     +---- Lesson Planner Agent
```

Agents communicate using structured contracts.

Important rule:

> **Agents own reasoning; application services own state.**

A remote agent should not become a second source of truth for learner state.

---

# 31. Message Bus

The message bus is a port.

Example conceptual interface:

```go
type EventBus interface {
    Publish(ctx context.Context, event Event) error
    Subscribe(ctx context.Context, topic string, handler Handler) error
}
```

The domain knows nothing about MQTT.

---

# 32. MQTT Adapter

MQTT is one transport adapter.

Possible topic hierarchy:

```text
learner/{identity}/learning/...
learner/{identity}/writing/...
learner/{identity}/vocabulary/...
learner/{identity}/ai/...
```

MQTT should be useful for:

- external integrations;
- home/local-network tooling;
- event observation;
- future automation;
- external reading application ingestion.

The internal event model remains transport-independent.

---

# 33. Event Architecture

Example:

```text
Writing Feedback
      |
      v
Correction Recorded
      |
      +----> Learner Model
      |
      +----> Analytics
      |
      +----> Teaching Planner
      |
      +----> Anki Agent
      |
      +----> Notification system
```

No consumer should need to be directly called by the writing subsystem.

---

# 34. Ports and Adapters Architecture

The application should follow a strong hexagonal architecture.

Conceptually:

```text
                    ┌────────────────────────────┐
                    │        Adapters            │
                    │                            │
                    │ PWA / REST / Chrome        │
                    │ Slack / Signal / WhatsApp  │
                    │ Email / MQTT / A2A          │
                    │ PostgreSQL / SQLite         │
                    │ AI Providers / CLI          │
                    └─────────────┬──────────────┘
                                  │
                             Ports / DTOs
                                  │
                    ┌─────────────▼──────────────┐
                    │    Application Layer        │
                    │                             │
                    │ Use Cases / Orchestration   │
                    │ Commands / Queries          │
                    └─────────────┬──────────────┘
                                  │
                    ┌─────────────▼──────────────┐
                    │       Domain Layer          │
                    │                             │
                    │ Learner                     │
                    │ Session                     │
                    │ Learning Events             │
                    │ Corrections                 │
                    │ Vocabulary                  │
                    │ Grammar                     │
                    │ Teaching priorities         │
                    └─────────────────────────────┘
```

The dependency direction is always inward.

---

# 35. Go Package Structure

A possible structure:

```text
/internal
    /domain
        /learner
        /session
        /writing
        /correction
        /vocabulary
        /grammar
        /learning
        /lesson
        /anki
        /event

    /application
        /writing
        /feedback
        /learning
        /lesson
        /analytics
        /anki

    /ports
        /ai
        /auth
        /events
        /storage
        /messaging
        /notifications

    /adapters
        /http
        /pwa
        /chrome
        /mqtt
        /postgres
        /sqlite
        /authelia
        /openai
        /anthropic
        /gemini
        /ollama
        /claudecli
        /codexcli
        /antigravity
        /slack
        /signal
        /whatsapp
        /email
        /a2a

    /agent
        /teacher
        /reviewer
        /planner
        /vocabulary
        /drill
        /lesson
        /anki

    /config
    /observability
```

The exact package structure can change; the architectural boundaries should not.

---

# 36. Configuration

Use a Viper-style configuration approach with strong typed configuration.

Configuration precedence:

```text
Defaults
    ↓
Configuration file
    ↓
Environment variables
    ↓
Explicit runtime overrides
```

Environment variables should use a predictable naming scheme:

```text
APP_SERVER_PORT
APP_DATABASE_URL
APP_AUTH_AUTHELIA_URL
APP_AI_DEFAULT_PROVIDER
APP_AI_GEMINI_API_KEY
APP_AI_OLLAMA_URL
APP_MQTT_URL
```

Configuration should be:

1. loaded once;
2. parsed into typed structs;
3. validated;
4. injected into components.

Fail fast on invalid configuration.

---

# 37. HTTP/API Layer

Use **Chi** for HTTP routing.

The API should expose resource-oriented endpoints.

Examples:

```text
GET    /api/sessions
POST   /api/sessions
GET    /api/sessions/{id}
PATCH  /api/sessions/{id}

GET    /api/sessions/{id}/messages
POST   /api/sessions/{id}/feedback

GET    /api/learner/profile
GET    /api/learner/priorities
GET    /api/learner/statistics

GET    /api/vocabulary
POST   /api/vocabulary/events

POST   /api/exercises
POST   /api/exercises/{id}/answers

POST   /api/anki/cards
POST   /api/ai/feedback
POST   /api/ratings
```

The browser and Chrome extension should consume the same backend contracts.

---

# 38. HTMX

The primary web UI should use:

- Go templates;
- HTMX;
- minimal JavaScript;
- Alpine.js where useful;
- standard browser APIs.

HTMX should not become the domain boundary.

The application API should remain independently usable.

This is important because the Chrome extension and future clients should not depend on HTMX.

---

# 39. PWA

The web application should be installable as a Progressive Web App.

Requirements:

- web app manifest;
- service worker;
- installability;
- responsive UI;
- offline shell;
- local draft persistence;
- background synchronization where practical.

Offline operation should initially focus on:

- opening the app;
- viewing existing sessions;
- writing drafts.

AI operations naturally require connectivity unless a local model is available.

---

# 40. Chrome Extension

The Chrome extension is a separate adapter/client.

Primary use case:

1. select Japanese text anywhere;
2. invoke the platform;
3. send selected text and relevant page context;
4. receive structured feedback;
5. render correction UI in extension-specific form.

The extension should never contain learning-domain logic.

It should consume the same APIs used by the PWA.

Potential features:

- contextual lookup;
- send selection to active session;
- request correction;
- save vocabulary;
- create learning event;
- open related session.

---

# 41. Authentication and Identity

The platform must support identity compartmentalization from the beginning.

Authentication should be externalized.

Primary provider:

**Authelia**

The application receives an authenticated identity through an authentication adapter.

The domain should only see:

```text
Identity
  ID
  DisplayName
  Attributes
```

Every user-owned resource must be scoped to an identity.

---

# 42. Authorization

Authorization must be enforced server-side.

Every request must resolve:

```text
Authenticated Identity
        ↓
Authorization Context
        ↓
Domain/Application Operation
```

A learner must never be able to access another learner's:

- sessions;
- writing;
- vocabulary;
- learning events;
- statistics;
- AI conversations;
- ratings;
- lesson plans.

Agents must also execute under an identity context.

---

# 43. Data Storage

PostgreSQL is the preferred primary persistent store.

Core tables/entities:

```text
identities
sessions
documents
document_versions
feedback_requests
corrections
grammar_concepts
vocabulary_items
vocabulary_events
learning_events
learner_observations
learner_priorities
exercises
exercise_attempts
lessons
lesson_observations
anki_cards
ai_requests
ai_feedback
ai_ratings
agent_runs
tool_calls
```

Use relational data for authoritative state.

Event data can be append-oriented.

---

# 44. Event vs Derived Data

A useful distinction:

### Events

"What happened?"

Examples:

```text
user wrote X
user looked up Y
user failed concept Z
```

### Observations

"What does the system currently believe?"

Examples:

```text
learner struggles with particle X
expression Y is becoming active
concept Z is probably mastered
```

### Recommendations

"What should happen next?"

Examples:

```text
practice X
introduce Y
deprioritize Z
```

This separation prevents the learner model from becoming an opaque blob.

---

# 45. Privacy

The system will contain highly personal writing and learning history.

Requirements:

- strict per-identity isolation;
- encrypted transport;
- secure secrets;
- minimal external transmission;
- explicit provider configuration;
- clear provider selection;
- ability to use local models;
- auditability of AI requests;
- configurable retention.

The system should make it possible to identify:

> Which provider received this text?

This is especially important for private writing.

---

# 46. Local-First Deployment

Primary deployment target:

```text
Local network
```

Recommended topology:

```text
                 LAN
                  |
              Authelia
                  |
                Caddy
                  |
           Japanese Learning App
             /       |       \
        PostgreSQL  MQTT    Ollama
                              |
                         Local Models
```

Docker Compose should be the primary local-development mechanism.

The application itself can run as a single Go service initially.

Do **not** prematurely split the system into microservices.

The architecture should be modular without requiring distributed deployment.

---

# 47. Docker Compose

Development environment should include:

```text
app
postgres
mqtt
ollama (optional)
```

Potential additional services:

```text
mailhog
```

for email testing.

A development configuration should make external providers optional.

---

# 48. Testing Strategy

## Domain

Pure unit tests.

No database.

No network.

No AI.

## Application

Use-case tests with mocked ports.

## Adapters

Integration tests.

Examples:

- PostgreSQL;
- MQTT;
- HTTP;
- Authelia;
- AI providers.

## AI

AI behavior should be tested through:

- schema validation;
- golden datasets;
- regression tests;
- provider comparison;
- structured-output validation.

AI should not be trusted simply because it returned valid JSON.

---

# 49. AI Evaluation

Create a Japanese correction evaluation corpus.

Examples should include:

- grammar errors;
- unnatural expressions;
- acceptable alternatives;
- casual/formal differences;
- deliberate ambiguity;
- native-like but unconventional expressions.

Evaluate:

```text
Correction precision
Correction recall
False-positive correction rate
Explanation quality
Naturalness
Teaching usefulness
```

User ratings should supplement automated evaluation.

---

# 50. Agent Observability

Every agent execution should produce a trace:

```text
Agent run
  ├── input context
  ├── model invocation
  ├── tool call
  ├── tool result
  ├── model invocation
  ├── output
  └── user rating
```

This makes agent behavior inspectable.

The dashboard can eventually show:

> "Why did the teacher recommend this exercise?"

---

# 51. Monitoring Dashboard

The dashboard should have several sections.

## Learning

- writing activity;
- reading activity;
- vocabulary;
- grammar;
- recurring mistakes;
- progress.

## Weaknesses

- top recurring issues;
- trend;
- confidence;
- frequency;
- last occurrence.

## Vocabulary

- recently encountered;
- active;
- passive;
- neglected;
- frequently looked up.

## AI

- providers;
- models;
- quality ratings;
- latency;
- cost;
- failure rate.

## Agents

- agent usage;
- tool usage;
- execution time;
- errors;
- outcomes.

## System

- requests;
- errors;
- event processing;
- MQTT;
- database;
- external integrations.

---

# 52. Feedback Quality Measurement

The system should distinguish between:

### Model quality

How good the AI response was objectively.

### User satisfaction

How useful the learner found it.

### Learning outcome

Whether the interaction actually helped.

For example:

```text
AI rating: 5/5
Learner independently solved similar problem later: yes
```

is a stronger signal than a 5/5 alone.

Eventually the system can measure:

> **Did the correction lead to improved future production?**

This is the most valuable long-term metric.

---

# 53. Confidence Tracking

After selected exercises or writing corrections, optionally ask:

> How confident were you?

Use a lightweight scale:

```text
1 — guessed
2 — unsure
3 — reasonably confident
4 — confident
5 — certain
```

Compare confidence with actual performance.

This identifies:

- false confidence;
- underconfidence;
- genuinely weak areas.

The teacher can adapt accordingly.

---

# 54. Spaced Retrieval

The system should gradually resurface learning points.

Rather than:

> "Study grammar today."

the system might naturally present:

> "You used this construction correctly last week. Can you use it again in this sentence?"

Review scheduling should consider:

- previous success;
- previous failure;
- time since exposure;
- production frequency;
- confidence;
- importance.

---

# 55. Expression Bank

Create a first-class expression bank.

This is distinct from vocabulary.

Examples:

```text
〜というわけではない
それはそれとして
気がしないでもない
〜に越したことはない
```

The system should encourage active use of expressions that are:

- encountered frequently;
- understood;
- but not produced.

---

# 56. Motivation System

Avoid aggressive gamification.

Instead use meaningful progress indicators:

- "You wrote 4,200 Japanese characters this week."
- "You independently corrected 12 recurring errors."
- "You successfully used 8 expressions you've previously looked up."
- "This grammar pattern has stopped appearing in your recurring errors."

The system should reinforce competence rather than streak anxiety.

---

# 57. Example End-to-End Writing Flow

```text
Learner writes blog post
        |
        v
Selects paragraph
        |
        v
"Get feedback"
        |
        v
Application builds teaching context
        |
        +--> session
        +--> learner priorities
        +--> recent relevant errors
        +--> vocabulary
        +--> teacher profile
        |
        v
Teacher Agent
        |
        +--> get_learner_priorities()
        +--> get_relevant_history()
        |
        v
Writing Reviewer
        |
        v
Structured correction result
        |
        v
Backend validates + computes diff
        |
        v
UI renders corrections
        |
        v
Learner retries
        |
        v
Learning event recorded
        |
        +--> learner model
        +--> analytics
        +--> teaching planner
        +--> possible Anki candidate
```

---

# 58. Example Learning Flow

```text
Learner says:
"I want to practice."

        |
        v

Teacher Agent
        |
        v

get_learning_priorities()
        |
        v

Teaching Planner
        |
        v

Current weak area:
particle usage

        |
        v

Generate contextual exercise
        |
        v

Learner answers
        |
        v

Evaluate
        |
        v

Record learning event
        |
        v

Update learner model
```

The learner does not need to know why particle usage was selected.

---

# 59. Example Reading Flow

```text
Learner reads novel
        |
        v
Looks up expression
        |
        v
Reading app
        |
        v
Integration adapter
        |
        v
VocabularyLookup event
        |
        v
Learner model
        |
        +--> vocabulary history
        +--> reading history
        +--> active vocabulary candidate
        |
        v
Future writing session
        |
        v
Teacher notices expression
        |
        v
Creates opportunity to use it
```

---

# 60. Example Human Tutor Flow

```text
Learner requests lesson guide
        |
        v
Lesson Planner Agent
        |
        +--> learner priorities
        +--> recent vocabulary
        +--> recent writing
        +--> persistent weaknesses
        |
        v
Lesson Guide
        |
        v
Share with tutor
        |
        v
Lesson
        |
        v
Tutor observations
        |
        v
Learning events
        |
        v
Learner model
```

---

# 61. API Design Principles

The API should:

- use explicit resource identifiers;
- be versioned;
- use JSON for machine-readable contracts;
- support structured AI responses;
- separate commands from queries where useful;
- never expose database models directly;
- enforce identity at every request;
- provide stable schemas to clients.

AI-generated output should always pass through backend validation.

---

# 62. Structured AI Contracts

JSON Schema should define all AI outputs.

For example:

```text
CorrectionResult
Explanation
Exercise
LessonPlan
LearnerObservation
AnkiCard
TeachingPlan
```

If a model returns malformed data:

1. validate;
2. attempt constrained repair;
3. retry if appropriate;
4. fail safely.

The UI should never have to parse arbitrary model prose to understand the result.

---

# 63. AI Prompt Architecture

Prompts should be versioned.

Example:

```text
teacher.feedback.v1
teacher.feedback.v2
teacher.exercise.v1
writing.review.v1
anki.generate.v1
lesson.generate.v1
```

Prompt templates should be configuration/data rather than scattered strings throughout Go code.

Each AI response should record:

```text
prompt version
model
provider
capability
tool versions
```

This enables meaningful quality comparisons.

---

# 64. Security Model for Agents

Agents should have explicit permissions.

For example:

### Writing Reviewer

Can:

- read selected writing;
- read relevant learner context.

Cannot:

- send email;
- modify learner profile directly;
- access another identity.

### Notification Agent

Can:

- read approved summaries;
- send configured notifications.

Cannot:

- access arbitrary private documents.

### Anki Agent

Can:

- read learning events;
- create draft cards.

Cannot:

- automatically publish cards without permission unless configured.

This follows least privilege.

---

# 65. Human Approval Boundaries

Actions should be classified:

### Read-only

No approval.

### Learning-state mutation

Usually automatic.

### External communication

Require explicit approval initially.

### External publication

Require explicit approval.

### Financial/irreversible actions

Never autonomous by default.

This is particularly important once messaging channels are added.

---

# 66. Future Speech Module

Although not required for MVP, the architecture should support speech.

Potential capabilities:

```text
SpeechRecognition
SpeechSynthesis
PronunciationEvaluation
ConversationAudio
```

The eventual loop:

```text
Think → formulate → speak → receive feedback
```

should use the same learner model as writing.

The learner's hypothesis is that improved written formulation will transfer to speaking.

The platform should eventually be able to measure this.

---

# 67. MVP

The first release should resist implementing everything.

### MVP components

- Go backend;
- Chi;
- HTMX;
- PWA;
- PostgreSQL;
- Docker Compose;
- Authelia authentication;
- sessions;
- writing editor;
- text selection;
- AI writing feedback;
- structured corrections;
- deterministic diff;
- Japanese/English explanations;
- teacher agent;
- learner events;
- basic learner model;
- basic statistics;
- one AI provider;
- AI provider abstraction;
- user ratings.

This creates the fundamental loop:

> **Write → Feedback → Learn → Record → Adapt**

---

# 68. Phase 2

Add:

- adaptive teaching planner;
- grammar concept tracking;
- vocabulary ingestion API;
- expression bank;
- active recall;
- drills;
- confidence tracking;
- AI quality dashboard;
- second/third AI provider;
- Ollama;
- CLI adapters.

---

# 69. Phase 3

Add:

- Chrome extension;
- Anki integration;
- tutor lesson guides;
- email summaries;
- MQTT;
- external reading integration;
- richer analytics.

---

# 70. Phase 4

Add:

- A2A;
- multi-agent collaboration;
- Signal;
- WhatsApp;
- Slack;
- conversation tutor;
- advanced spaced retrieval;
- speech;
- pronunciation;
- deeper learning-outcome analytics.

---

# 71. Architecture Evolution

The application should begin as a **modular monolith**.

Do not deploy:

```text
writing-service
learner-service
ai-service
vocabulary-service
agent-service
analytics-service
```

on day one.

Instead:

```text
One Go binary
    |
    +-- Domain modules
    +-- Application modules
    +-- Ports
    +-- Adapters
```

The internal boundaries should make future extraction possible if genuinely required.

This provides the benefits of modularity without the operational cost of microservices.

---

# 72. Success Metrics

The ultimate success metric is not:

> Number of AI interactions.

It is:

> **Improvement in independent Japanese production.**

Supporting metrics:

### Production

- Japanese characters written;
- writing sessions;
- independent writing frequency.

### Correction

- recurring errors over time;
- corrections per 1,000 characters;
- independent self-corrections.

### Vocabulary

- looked-up → reviewed;
- reviewed → produced;
- produced → retained.

### Grammar

- weak concept frequency;
- successful production;
- recurrence decay.

### Confidence

- confidence vs correctness;
- confidence trend.

### Learning outcome

Most importantly:

> Can the learner independently produce something later that they previously needed assistance with?

---

# 73. Product Success Criterion

The platform succeeds if, after sustained use:

1. the learner writes more Japanese because feedback is frictionless;
2. the learner increasingly catches their own errors;
3. recurring errors decline;
4. vocabulary moves from recognition into active production;
5. the learner can formulate increasingly complex thoughts without assistance;
6. AI assistance becomes less necessary for previously mastered areas;
7. the learner can transfer improved formulation into conversation.

The ideal outcome is paradoxical:

> **The better the learner becomes, the less they need the teacher for basic things.**

---

# 74. Core Domain Model

At the conceptual level:

```text
Identity
 └── Learner
      ├── Learner Profile
      ├── Learner Observations
      ├── Learning Priorities
      ├── Vocabulary
      ├── Grammar Knowledge
      ├── Learning Events
      └── Statistics

Identity
 └── Sessions
      ├── Session Profile
      ├── Documents
      ├── Selections
      ├── Feedback
      ├── Conversations
      └── Session Events

Learning Event
 ├── Source
 ├── Subject
 ├── Evidence
 ├── Outcome
 └── Timestamp

AI Interaction
 ├── Provider
 ├── Model
 ├── Capability
 ├── Agent
 ├── Tools
 ├── Result
 └── User Feedback
```

---

# 75. Architectural Rules

These should become explicit project rules.

### Rule 1

**Domain code must not import AI providers.**

### Rule 2

**Domain code must not import HTTP, MQTT, PostgreSQL, or Authelia.**

### Rule 3

**Agents must not access repositories directly.**

### Rule 4

**AI output must be schema validated.**

### Rule 5

**The backend computes deterministic diffs.**

### Rule 6

**All learner state is identity-scoped.**

### Rule 7

**Events are transport-independent.**

### Rule 8

**MQTT is an adapter, not the event model.**

### Rule 9

**AI providers are interchangeable.**

### Rule 10

**Prompts are versioned.**

### Rule 11

**AI interactions are observable.**

### Rule 12

**External side effects require explicit capability/permission.**

### Rule 13

**The learner model is authoritative; agents are consumers and interpreters.**

### Rule 14

**Prefer modular monolith over premature microservices.**

### Rule 15

**Every new feature should ideally be implementable as a new module, adapter, agent, tool, or event consumer rather than a modification to unrelated modules.**

---

# 76. The Fundamental Architecture

The final conceptual architecture is:

```text
                           ┌─────────────────────┐
                           │       Clients       │
                           │                     │
                           │ PWA / HTMX          │
                           │ Chrome Extension    │
                           │ Slack               │
                           │ Signal              │
                           │ WhatsApp            │
                           │ Email               │
                           └──────────┬──────────┘
                                      │
                                      ▼
                           ┌─────────────────────┐
                           │     API / Channel   │
                           │      Adapters       │
                           └──────────┬──────────┘
                                      │
                                      ▼
                  ┌────────────────────────────────────┐
                  │          Application Layer          │
                  │                                    │
                  │ Writing / Learning / Sessions      │
                  │ Lessons / Analytics / Vocabulary   │
                  └────────────────┬───────────────────┘
                                   │
             ┌─────────────────────┼─────────────────────┐
             │                     │                     │
             ▼                     ▼                     ▼
      ┌─────────────┐      ┌──────────────┐      ┌──────────────┐
      │   Domain    │      │    Agents    │      │    Tools     │
      │             │      │              │      │              │
      │ Learner     │◄────►│ Teacher      │◄────►│ Learner      │
      │ Session     │      │ Reviewer     │      │ Vocabulary   │
      │ Writing     │      │ Planner      │      │ Sessions     │
      │ Grammar     │      │ Drill        │      │ Analytics    │
      │ Vocabulary  │      │ Lesson       │      │ Writing      │
      │ Events      │      │ Anki         │      │              │
      └──────┬──────┘      └──────┬───────┘      └──────────────┘
             │                    │
             │                    ▼
             │             ┌──────────────┐
             │             │ AI Capability│
             │             │     Port     │
             │             └──────┬───────┘
             │                    │
             │        ┌───────────┼────────────┐
             │        ▼           ▼            ▼
             │      Gemini     Claude       Ollama
             │      OpenAI     Codex CLI    CLI tools
             │
             ▼
      ┌──────────────────┐
      │   Event Bus Port │
      └────────┬─────────┘
               │
          ┌────┴─────┐
          ▼          ▼
        MQTT       Future
                   transports

      ┌────────────────────────────────────────────┐
      │                Adapters                    │
      │                                            │
      │ PostgreSQL │ Authelia │ MQTT │ AI         │
      │ Email      │ Anki     │ A2A  │ Messaging  │
      └────────────────────────────────────────────┘
```

---

# 77. The Most Important Design Decision

The most important architectural choice is **not** Go, HTMX, Chi, MQTT, A2A, or which AI model is used.

It is this separation:

> **The learner model and learning events are the stable core; everything else is replaceable.**

The writing interface can change.

The AI provider can change.

The AI agent architecture can change.

The communication channel can change.

The reading application can change.

The database can eventually change.

The local network deployment can change.

Even the entire teaching strategy can change.

But the accumulated evidence of:

> **what this learner encounters, writes, understands, struggles with, successfully produces, and improves**

remains the durable asset of the platform.

That gives the application a coherent architecture rather than a collection of AI features.

---

# 78. Final Product Definition

This product is best understood as a **personal adaptive language-learning operating system** rather than an AI writing assistant.

Its primary interface happens to be a writing editor, because writing is one of the richest sources of evidence about active language ability.

Its underlying system is:

```text
                    AUTHENTIC USE
                         │
             ┌───────────┼───────────┐
             ▼           ▼           ▼
           READ        WRITE       SPEAK
             │           │           │
             └───────────┼───────────┘
                         ▼
                 LEARNING EVENTS
                         │
                         ▼
                  LEARNER MODEL
                         │
                         ▼
                 TEACHING PLANNER
                         │
                         ▼
                  TEACHER / AGENTS
                         │
             ┌───────────┼───────────┐
             ▼           ▼           ▼
           FEEDBACK    DRILLS      LESSONS
             │           │           │
             └───────────┼───────────┘
                         ▼
                  MORE AUTHENTIC USE
```

The system should ultimately make learning feel almost incidental:

**You read because you enjoy reading.  
You write because you have something to say.  
You talk because you want to communicate.  
The platform quietly turns all three into increasingly personalized opportunities to improve.**

That is the core product. Everything else—agents, A2A, MQTT, PWA, Chrome, Anki, AI adapters, dashboards, and integrations—exists to make that loop more powerful while keeping the underlying system modular and replaceable.