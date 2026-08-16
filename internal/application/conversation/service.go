// Package conversation is the application-layer conversation tutor
// pipeline (Phase 4 Task 6, PRD §17.4): a free-form back-and-forth
// dialogue in Japanese, as an alternative to the writing/feedback pane.
// It authorizes each turn against the caller's session, asks the
// conversation agent for a reply plus whatever corrections it found,
// persists the turn, records the same learning events writing
// corrections do, and — governed by the session's
// session.Profile.FeedbackTiming — decides whether this turn's
// corrections are shown immediately, held for a later batch, or
// withheld entirely until Summarise.
//
// Unlike application/feedback's corrections, a conversation turn's
// corrections have no independent accept/reject/retry/reveal lifecycle
// of their own: they're a byproduct of the dialogue, not something the
// learner works through one at a time, so they're persisted whole
// (jsonb) on the turn itself — see storage.ConversationTurn's doc
// comment.
package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	agentconversation "github.com/mikeyaustin/jlp/internal/agent/conversation"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// delayedBatchSize is how many turns "delayed" feedback timing
// withholds corrections for before releasing them as one batch (PRD
// §17.4's "withholds them for N turns then shows a batch") — turns
// 1..N-1 show nothing, turn N's response carries every correction found
// across turns 1..N, then the window resets for turns N+1..2N, and so
// on. Not user-configurable (yet): a fixed compromise between
// "immediate" (every turn) and "end" (never until Summarise).
const delayedBatchSize = 3

// Service wires the conversation tutor pipeline.
type Service struct {
	repo     storage.ConversationRepository
	sessions storage.SessionRepository
	agent    *agentconversation.Agent
	vocab    *appvocabulary.Service
	rec      *learning.Recorder
	// grammar backs Finding I-1's fix: Say offers the agent the full
	// catalog of taggable grammar concepts (mirroring
	// application/feedback.Service.RequestFeedback's conceptCandidates/
	// knownSlugs) and resolves whatever slugs come back against it —
	// without this, the conversation.turn prompt's "choose ONLY from the
	// provided candidate list" instruction has no list to choose from,
	// and no correction can ever tag a concept in production.
	grammar storage.GrammarRepository
	// events backs Say's sourceEventID validation (code review
	// Important, post-I1): a client-supplied "speech_event_id" is
	// otherwise a free-form string ANY caller could set on a typed
	// message, forging speech provenance and corrupting exactly the
	// spoken-vs-typed distinction I1 exists to support. See
	// validSourceEvent's own doc comment for what this repository is
	// used to check.
	events storage.LearningEventRepository
}

// NewService wires the conversation tutor pipeline.
func NewService(repo storage.ConversationRepository, sessions storage.SessionRepository, agent *agentconversation.Agent, vocab *appvocabulary.Service, rec *learning.Recorder, grammar storage.GrammarRepository, events storage.LearningEventRepository) *Service {
	return &Service{repo: repo, sessions: sessions, agent: agent, vocab: vocab, rec: rec, grammar: grammar, events: events}
}

// Turn is one exchange in the conversation, as Say returns it: Reply is
// always populated, but Corrections reflects the session's
// FeedbackTiming policy — it's the set of corrections THIS response
// makes visible (empty for a "delayed" turn still inside its batch
// window, or for any "end"-timing turn — the correction data itself is
// durably persisted either way, just not returned here), never the raw
// set the agent found this turn. Timing is copied from the session so
// a caller (the HTTP handler, its template) can caption why nothing
// showed up without a second lookup.
type Turn struct {
	ID          string
	LearnerText string
	Reply       string
	ReplyEN     string
	Corrections []correction.Correction
	Followup    string
	Timing      string
	// Pending is how many corrections THIS turn's own message found but
	// Corrections above does not show (0 whenever Corrections is
	// non-empty, or whenever this turn's message was genuinely clean) —
	// a caller (the HTTP view) uses it to distinguish "nothing was found"
	// from "something was found and is being withheld" without either
	// guessing from Timing alone or being handed the withheld content
	// itself. See visibleCorrections' doc comment for why Corrections
	// alone can't answer this.
	Pending int
}

// Summary is the end-of-conversation digest Summarise returns: every
// correction found across the whole conversation, in the order their
// turns occurred — this is where "end" (and, incidentally, "delayed"
// and "immediate" too) timing's full picture lives, regardless of what
// any individual Turn response showed along the way.
type Summary struct {
	ConversationID string
	Turns          int
	Corrections    []correction.Correction
}

// Say authorizes text against the caller's session, asks the
// conversation agent for a reply (with the conversation's history so
// far for continuity), persists the resulting turn, records
// conversation.turn plus one correction.presented (and, for a socratic
// hint, hint.shown) per correction found — the SAME two event types
// application/feedback.Service.RequestFeedback records for writing
// corrections — then runs vocabulary production detection exactly like
// a writing review does (non-fatal — see that call's own comment
// below). sid must belong to identity: s.sessions.Get is the
// authorization check every other identity-scoped service in this
// codebase leads with, so a wrong identity fails here with
// storage.ErrNotFound before anything else runs.
//
// sourceEventID is empty for ordinary typed input; when non-empty (set
// by internal/adapters/http/conversation.go's conversationSay from the
// conversation form's hidden "speech_event_id" field, which record.js
// populates from POST /speech/transcribe's own response) it is
// CLIENT-SUPPLIED and UNTRUSTED — Say validates it (validSourceEvent)
// before recording anything, and only a value that resolves to a
// genuine speech.transcribed event owned by identity, scoped to sid,
// and not already attached to an earlier turn is recorded verbatim
// into the resulting conversation.turn event's Evidence as
// "speech_event_id" (code review Important I1: without this key, a
// speech.transcribed event and the conversation.turn it became have no
// way to be joined, and Task 9's learning-outcome analytics can't tell
// spoken production from typed — the entire reason PRD §66 asks for
// ONE shared pipeline in the first place; a follow-up review then
// found the naive version of this trusted the client outright, which
// would have let any caller forge a typed message as speech-sourced,
// including with another identity's real event id). A sourceEventID
// that fails validation silently DOWNGRADES the turn to typed rather
// than rejecting the request — a bad provenance hint must never lose
// the learner's message. Either way this is still the SAME pipeline —
// no branch on validity changes what Say does beyond this one Evidence
// field.
func (s *Service) Say(ctx context.Context, identity learner.IdentityID, sid session.ID, text string, sourceEventID string) (Turn, error) {
	sess, err := s.sessions.Get(ctx, identity, sid)
	if err != nil {
		return Turn{}, err
	}

	convID, _, err := s.repo.GetOrCreateForSession(ctx, identity, sid)
	if err != nil {
		return Turn{}, fmt.Errorf("conversation: get or create conversation: %w", err)
	}

	history, err := s.repo.ListTurns(ctx, identity, convID)
	if err != nil {
		return Turn{}, fmt.Errorf("conversation: list turns: %w", err)
	}

	agentHistory := make([]agentconversation.HistoryTurn, 0, len(history))
	for _, t := range history {
		agentHistory = append(agentHistory, agentconversation.HistoryTurn{LearnerText: t.LearnerText, Reply: t.Reply})
	}

	// conceptCandidates/knownSlugs mirror
	// application/feedback.Service.RequestFeedback's identically-named
	// locals exactly (Finding I-1): the full catalog of taggable grammar
	// concepts, offered to the agent as candidates and used afterward to
	// resolve whatever slugs the response actually tagged.
	concepts, err := s.grammar.ListConcepts(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("conversation: list grammar concepts: %w", err)
	}
	conceptCandidates := make([]string, 0, len(concepts))
	knownSlugs := make(map[string]bool, len(concepts))
	for _, c := range concepts {
		conceptCandidates = append(conceptCandidates, fmt.Sprintf("%s — %s", c.Slug, c.Name))
		knownSlugs[c.Slug] = true
	}

	out, resp, err := s.agent.Turn(ctx, agentconversation.TurnInput{
		Identity:          identity,
		Session:           sess,
		History:           agentHistory,
		Message:           text,
		ConceptCandidates: conceptCandidates,
	})
	if err != nil {
		return Turn{}, err
	}

	// Narrow each correction's Concepts to the resolved (catalog-known)
	// subset, deduped — the same "never let an unresolved or duplicate
	// slug reach a view" guarantee resolveConceptTags gives the writing
	// path (feedback/service.go), applied here since a conversation
	// correction's Concepts field IS what's persisted and later rendered
	// (no separate correction_concepts table to keep unresolved tags
	// out of a view — see storage.ConversationTurn's doc comment).
	for i, c := range out.Corrections {
		out.Corrections[i].Concepts = resolveConceptSlugs(c.Concepts, knownSlugs)
	}

	position := len(history) + 1
	turnID := uuid.New().String()
	rec := storage.ConversationTurn{
		ID:             turnID,
		ConversationID: convID,
		SessionID:      sid,
		Position:       position,
		LearnerText:    text,
		Reply:          out.Reply,
		ReplyEN:        out.ReplyEN,
		Followup:       out.Followup,
		Corrections:    out.Corrections,
		AIRequestID:    resp.RequestID,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.repo.InsertTurn(ctx, identity, convID, rec); err != nil {
		return Turn{}, fmt.Errorf("conversation: persist turn: %w", err)
	}

	turnEvidence := map[string]any{
		"position":    position,
		"corrections": len(out.Corrections),
	}
	if sourceEventID != "" && s.validSourceEvent(ctx, identity, sid, sourceEventID) {
		// The join key I1 asked for: Task 9 (or any future consumer) can
		// match this conversation.turn back to the exact speech.
		// transcribed event (internal/application/speech.Service.
		// Transcribe's own Evidence carries duration_ms/mime/chars) that
		// produced text, distinguishing spoken from typed production.
		// Only ever set once validSourceEvent has confirmed the id is
		// genuine, owned, scoped, and unused — see that method's doc
		// comment. An invalid id silently downgrades to typed (this map
		// simply never gets the key), never an error.
		turnEvidence["speech_event_id"] = sourceEventID
	}
	err = s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		SessionID:  &sid,
		Type:       event.TypeConversationTurn,
		Subject:    turnID,
		Evidence:   turnEvidence,
	})
	if errors.Is(err, storage.ErrDuplicate) {
		// validSourceEvent's check-then-write left a narrow window: another
		// concurrent Say call for the SAME sourceEventID could Record its
		// own turn between our check and this write, and the database's
		// own migrationsfs/00025_speech_event_id_unique.sql partial index —
		// not just our application-level check — is what actually caught
		// it. Losing this race must behave EXACTLY like an invalid id
		// (Say's own doc comment): downgrade to typed and retry once,
		// never surface a database error to the learner.
		delete(turnEvidence, "speech_event_id")
		err = s.rec.Record(ctx, event.LearningEvent{
			IdentityID: identity,
			SessionID:  &sid,
			Type:       event.TypeConversationTurn,
			Subject:    turnID,
			Evidence:   turnEvidence,
		})
	}
	if err != nil {
		return Turn{}, fmt.Errorf("conversation: record %s: %w", event.TypeConversationTurn, err)
	}

	for _, c := range out.Corrections {
		if err := s.rec.Record(ctx, event.LearningEvent{
			IdentityID: identity,
			SessionID:  &sid,
			Type:       event.TypeCorrectionPresented,
			Subject:    c.ID,
			Evidence: map[string]any{
				"type":     string(c.Type),
				"severity": string(c.Severity),
			},
		}); err != nil {
			return Turn{}, fmt.Errorf("conversation: record %s: %w", event.TypeCorrectionPresented, err)
		}
		if c.HasHint() {
			if err := s.rec.Record(ctx, event.LearningEvent{
				IdentityID: identity,
				SessionID:  &sid,
				Type:       event.TypeHintShown,
				Subject:    c.ID,
				Evidence: map[string]any{
					"type":     string(c.Type),
					"severity": string(c.Severity),
				},
			}); err != nil {
				return Turn{}, fmt.Errorf("conversation: record %s: %w", event.TypeHintShown, err)
			}
		}

		// One grammar.concept.encountered event per RESOLVED slug this
		// correction was tagged with (c.Concepts was already narrowed to
		// resolved-only above) — the SAME event
		// application/feedback.Service.RequestFeedback records per
		// resolved slug, so a conversation turn feeds the learner model's
		// concept-weakness tracking exactly like a writing review does
		// (Finding I-1, the brief's third learner-model bullet).
		for _, slug := range c.Concepts {
			if err := s.rec.Record(ctx, event.LearningEvent{
				IdentityID: identity,
				SessionID:  &sid,
				Type:       event.TypeGrammarConceptEncountered,
				Subject:    slug,
				Evidence: map[string]any{
					"correction_id": c.ID,
					"type":          string(c.Type),
					"severity":      string(c.Severity),
				},
			}); err != nil {
				return Turn{}, fmt.Errorf("conversation: record %s: %w", event.TypeGrammarConceptEncountered, err)
			}
		}
	}

	// Production detection runs LAST and its error is deliberately
	// swallowed (logged, not returned) — same tradeoff, for the same
	// reason, as application/feedback.Service.RequestFeedback's own
	// closing DetectProduction call: by this point the turn and its
	// events are already durably recorded, and a vocabulary-production
	// hiccup is enrichment layered on top of that history, not part of
	// it.
	if s.vocab != nil {
		correctedSpans := make([]string, 0, len(out.Corrections))
		for _, c := range out.Corrections {
			correctedSpans = append(correctedSpans, c.Original)
		}
		if err := s.vocab.DetectProduction(ctx, identity, sid, text, correctedSpans); err != nil {
			slog.Error("conversation: vocabulary detect production", "err", err)
		}
	}

	shown := visibleCorrections(sess.Profile.FeedbackTiming, position, history, out.Corrections)
	return Turn{
		ID:          turnID,
		LearnerText: text,
		Reply:       out.Reply,
		ReplyEN:     out.ReplyEN,
		Corrections: shown,
		Followup:    out.Followup,
		Timing:      sess.Profile.FeedbackTiming,
		Pending:     pendingCount(shown, out.Corrections),
	}, nil
}

// sourceEventLookupWindow bounds validSourceEvent's own event lookup —
// generous enough that a genuine speech.transcribed event from earlier
// in a long conversation session is still found (a session's total
// event count is bounded by how many turns/corrections/speech clips
// one sitting realistically produces), while still being a fixed,
// non-unbounded query.
const sourceEventLookupWindow = 500

// validSourceEvent reports whether sourceEventID is safe to record as
// a conversation.turn's "speech_event_id" evidence — code review's
// post-I1 finding that the naive version of this (recording whatever
// string the client sent) let ANY caller forge speech provenance onto
// a typed message, including by replaying another identity's real
// event id. All four conditions must hold:
//
//  1. sourceEventID resolves to an event that actually exists.
//  2. That event's Type is event.TypeSpeechTranscribed (not some other
//     event type reused as a forgery vector).
//  3. It's owned by identity and scoped to sid — enforced by asking
//     s.events.ListRecent for exactly this identity+session window,
//     the same repository-level scoping every other identity-scoped
//     lookup in this codebase relies on (a postgres implementation
//     filters by identity_id/session_id in SQL; the id simply won't
//     appear in the results otherwise — see internal/adapters/
//     postgres/learningevents.go's ListRecent).
//  4. It hasn't already been attached to an earlier conversation.turn
//     in this same window — otherwise one real event id could be
//     replayed to tag an unlimited number of typed messages as
//     speech-sourced.
//
// Any of these failing returns false, never an error: Say's own
// caller (this method's) treats a bad sourceEventID as "no provenance
// hint" and proceeds with an ordinary typed turn — see Say's doc
// comment on why a forged/invalid id downgrades rather than rejects.
func (s *Service) validSourceEvent(ctx context.Context, identity learner.IdentityID, sid session.ID, sourceEventID string) bool {
	events, err := s.events.ListRecent(ctx, identity, &sid, sourceEventLookupWindow)
	if err != nil {
		slog.Error("conversation: validate speech_event_id: list recent events", "err", err)
		return false
	}

	found := false
	for _, ev := range events {
		if ev.Type == event.TypeConversationTurn {
			if id, ok := ev.Evidence["speech_event_id"]; ok && id == sourceEventID {
				return false // condition 4: already consumed by an earlier turn
			}
			continue
		}
		if ev.Type == event.TypeSpeechTranscribed && ev.ID == sourceEventID {
			found = true // conditions 1-3: exists, right type, and — since
			// ListRecent already scoped this whole query to identity+sid —
			// owned and scoped correctly by construction.
		}
	}
	return found
}

// pendingCount is Turn.Pending's definition: 0 whenever shown is
// non-empty (this turn's corrections — whether just its own, under
// "immediate", or a released batch including it, under "delayed" — are
// already visible, so nothing about THIS turn is pending), otherwise
// the count of raw corrections this turn's own message actually found
// (0 for a genuinely clean message, >0 for one being withheld).
func pendingCount(shown, raw []correction.Correction) int {
	if len(shown) > 0 {
		return 0
	}
	return len(raw)
}

// resolveConceptSlugs narrows slugs to the subset present in known,
// deduped in first-occurrence order — the single place a correction's
// raw AI-tagged Concepts becomes what's actually persisted/recorded,
// mirroring application/feedback's resolveConceptTags (that package's
// second return value): an unresolved (hallucinated/stale) slug is
// dropped here, never persisted, never eventable, and never rendered as
// a /grammar/{slug} chip that would 404.
func resolveConceptSlugs(slugs []string, known map[string]bool) []string {
	if len(slugs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(slugs))
	out := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		if !known[slug] || seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}

// visibleCorrections decides what a single Say call's Turn.Corrections
// carries, per PRD §17.4's three feedback-timing policies:
//
//   - "immediate": exactly this turn's own corrections (current).
//   - "delayed": nothing until position lands on a delayedBatchSize
//     boundary, at which point every correction from the turns in that
//     batch window (history's tail plus current) comes back together.
//   - "end" (and any other/empty value — session.Profile.FeedbackTiming
//     is never empty for a session created through
//     application/sessions.Service.Create, but a zero-value
//     session.Session in a test is not required to set it): nothing,
//     ever — the correction data is still durably persisted on the
//     turn (see Say above), just not surfaced here. Summarise is the
//     only path that ever returns it.
func visibleCorrections(timing string, position int, history []storage.ConversationTurn, current []correction.Correction) []correction.Correction {
	switch timing {
	case "immediate":
		return current
	case "delayed":
		if position%delayedBatchSize != 0 {
			return nil
		}
		batchStart := position - delayedBatchSize + 1
		if batchStart < 1 {
			batchStart = 1
		}
		var out []correction.Correction
		for _, t := range history {
			if t.Position >= batchStart {
				out = append(out, t.Corrections...)
			}
		}
		out = append(out, current...)
		return out
	default:
		return nil
	}
}

// History returns every turn already said in identity's conversation
// for sid, oldest first, each with its OWN Corrections re-derived
// through visibleCorrections exactly as Say computed them the moment
// they happened — so a page reload (or a fresh workspace load) shows
// precisely what the learner already saw as each turn came in, never
// more, regardless of how many turns have been said since. Not part of
// the brief's original two-method interface, but needed by the
// workspace pane to render the transcript on GET, the same way
// application/writing.Service.Open exists alongside Autosave for the
// editor pane's own initial render.
func (s *Service) History(ctx context.Context, identity learner.IdentityID, sid session.ID) ([]Turn, error) {
	sess, err := s.sessions.Get(ctx, identity, sid)
	if err != nil {
		return nil, err
	}

	convID, _, err := s.repo.GetOrCreateForSession(ctx, identity, sid)
	if err != nil {
		return nil, fmt.Errorf("conversation: get or create conversation: %w", err)
	}

	turns, err := s.repo.ListTurns(ctx, identity, convID)
	if err != nil {
		return nil, fmt.Errorf("conversation: list turns: %w", err)
	}

	out := make([]Turn, 0, len(turns))
	for i, t := range turns {
		shown := visibleCorrections(sess.Profile.FeedbackTiming, t.Position, turns[:i], t.Corrections)
		out = append(out, Turn{
			ID:          t.ID,
			LearnerText: t.LearnerText,
			Reply:       t.Reply,
			ReplyEN:     t.ReplyEN,
			Corrections: shown,
			Followup:    t.Followup,
			Timing:      sess.Profile.FeedbackTiming,
			Pending:     pendingCount(shown, t.Corrections),
		})
	}
	return out, nil
}

// Summarise authorizes sid against identity (same s.sessions.Get check
// Say leads with), then gathers every correction found across the
// whole conversation so far, in turn order — the end-of-conversation
// digest PRD §17.4's "end" feedback timing withholds everything for
// until now (and which "immediate"/"delayed" sessions can call too, for
// the same complete picture). Corrections that are still socratically
// gated (correction.IsGated — see that function's doc comment: this
// package never re-derives the predicate) are included here exactly as
// persisted; it's the CALLER's job — the HTTP view, via the same
// correction_card partial every other gated-correction surface uses —
// to withhold each gated correction's Replacement, not this method's.
func (s *Service) Summarise(ctx context.Context, identity learner.IdentityID, sid session.ID) (Summary, error) {
	if _, err := s.sessions.Get(ctx, identity, sid); err != nil {
		return Summary{}, err
	}

	convID, _, err := s.repo.GetOrCreateForSession(ctx, identity, sid)
	if err != nil {
		return Summary{}, fmt.Errorf("conversation: get or create conversation: %w", err)
	}

	turns, err := s.repo.ListTurns(ctx, identity, convID)
	if err != nil {
		return Summary{}, fmt.Errorf("conversation: list turns: %w", err)
	}

	var corrections []correction.Correction
	for _, t := range turns {
		corrections = append(corrections, t.Corrections...)
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		SessionID:  &sid,
		Type:       event.TypeConversationSummarised,
		Subject:    convID,
		Evidence: map[string]any{
			"turns":       len(turns),
			"corrections": len(corrections),
		},
	}); err != nil {
		return Summary{}, fmt.Errorf("conversation: record %s: %w", event.TypeConversationSummarised, err)
	}

	return Summary{ConversationID: convID, Turns: len(turns), Corrections: corrections}, nil
}
