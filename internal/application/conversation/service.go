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
}

// NewService wires the conversation tutor pipeline.
func NewService(repo storage.ConversationRepository, sessions storage.SessionRepository, agent *agentconversation.Agent, vocab *appvocabulary.Service, rec *learning.Recorder) *Service {
	return &Service{repo: repo, sessions: sessions, agent: agent, vocab: vocab, rec: rec}
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
func (s *Service) Say(ctx context.Context, identity learner.IdentityID, sid session.ID, text string) (Turn, error) {
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

	out, resp, err := s.agent.Turn(ctx, agentconversation.TurnInput{
		Identity: identity,
		Session:  sess,
		History:  agentHistory,
		Message:  text,
	})
	if err != nil {
		return Turn{}, err
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

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		SessionID:  &sid,
		Type:       event.TypeConversationTurn,
		Subject:    turnID,
		Evidence: map[string]any{
			"position":    position,
			"corrections": len(out.Corrections),
		},
	}); err != nil {
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
