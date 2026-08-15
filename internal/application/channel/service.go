// Package channel is the application-layer channel pipeline (Phase 4
// Task 4, PRD §20/§20.1): the ONE place that turns a
// ports/channels.Inbound message from any transport adapter into calls
// against the existing sessions/feedback/practice application services,
// and their result back into a channel-appropriate, compact
// ports/channels.Outbound reply. No adapter (internal/adapters/slack
// today) contains any of this routing logic itself — it only translates
// its own wire format into Inbound/Outbound and calls Handle.
package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/channels"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// refusalText is what an unmapped sender (see Service.allowFrom) gets
// back — PRD §20.1's untrusted-edge default-deny. Deliberately generic:
// it names neither the channel nor the external ID back to an
// unauthenticated sender, and gives an operator, not the sender
// themself, the next step.
const refusalText = "Sorry, I don't recognize this account. Ask the JLP operator to add it to APP_CHANNELS_ALLOWFROM."

// helpText is the "help"/ヘルプ command's reply — PRD §20.1's short
// command list.
const helpText = "JLP commands:\n" +
	"• Send Japanese text to get corrections\n" +
	"• \"practice\" or 練習 — start a quick drill (your next message is the answer)\n" +
	"• \"help\" or ヘルプ — show this message"

// Service wires the channel pipeline over the same application services
// every other JLP surface (the HTTP UI, the agentic teacher's tools)
// already uses — sessions/feedback/practice — plus docs/identities,
// needed only here: docs backs the per-channel session's single
// document (see getOrCreateChannelDocument), identities auto-provisions
// a learner.Identity on a mapped sender's first contact (see
// ensureIdentity) so the sessions/feedback foreign keys it writes
// through never fail on an identity nothing else has created yet.
type Service struct {
	sessions   *sessions.Service
	feedback   *feedback.Service
	practice   *practice.Service
	docs       storage.DocumentRepository
	identities storage.IdentityRepository

	// allowFrom is config.ParseAllowFrom(cfg.AllowFrom), computed once at
	// construction — see that function's doc comment for the exact
	// "<channel>:<external id>" -> identity shape.
	allowFrom map[string]string

	// mu guards pending, in-memory, per-process conversation state:
	// "start an exercise, treat the NEXT message as the answer" (PRD
	// §20.1's practice routing) is a live-conversation concern, not
	// learner-model history — it belongs here, not in a repository, and
	// does not survive a process restart (an in-flight practice prompt
	// left unanswered across a restart just falls through to the
	// correction path on the learner's next message, same as it would
	// after Handle's own pending entry is cleared by TakePending).
	mu      sync.Mutex
	pending map[string]string // "channel:externalID" -> exercise ID awaiting an answer
}

// NewService wires the channel pipeline. cfg.AllowFrom is parsed once
// here via config.ParseAllowFrom — config.Load's own validate() already
// rejects a malformed APP_CHANNELS_ALLOWFROM at boot (fail-fast, same as
// every other config value), so a parse error here only means a caller
// constructed Service directly with an unvalidated cfg (e.g. a future
// test); NewService fails safe in that case — deny every sender — rather
// than panicking or silently guessing at a partial mapping.
func NewService(sessionsSvc *sessions.Service, feedbackSvc *feedback.Service, practiceSvc *practice.Service, docs storage.DocumentRepository, identities storage.IdentityRepository, cfg config.Channels) *Service {
	allowFrom, err := config.ParseAllowFrom(cfg.AllowFrom)
	if err != nil {
		slog.Error("channel: invalid APP_CHANNELS_ALLOWFROM, denying every sender", "err", err)
		allowFrom = map[string]string{}
	}
	return &Service{
		sessions:   sessionsSvc,
		feedback:   feedbackSvc,
		practice:   practiceSvc,
		docs:       docs,
		identities: identities,
		allowFrom:  allowFrom,
		pending:    map[string]string{},
	}
}

// Handle is the channel port's single entry point (ports/channels.
// Channel.Start's handle callback): resolve in's sender against
// AllowFrom, refuse (recording nothing) if unmapped, then route by the
// message text — PRD §20.1:
//
//	Japanese text     → a correction of that text in the learner's
//	                    channel session, rendered COMPACT (pairs + one-
//	                    line reasons), gated corrections showing only
//	                    the hint, never the answer.
//	"practice" / 練習  → start an exercise, render it as text; the
//	                    NEXT message from this sender is treated as the
//	                    answer.
//	"help" / ヘルプ    → helpText.
//
// An explicit "help"/"practice" command always takes priority over a
// pending practice answer: a learner who abandons a drill mid-way and
// types "help" or starts a fresh "practice" round is not forced to
// finish (or nonsense-answer) the old one first.
func (s *Service) Handle(ctx context.Context, in channels.Inbound) (channels.Outbound, error) {
	key := allowKey(in.Channel, in.ExternalID)
	identityStr, ok := s.allowFrom[key]
	if !ok {
		// PRD §20.1: nothing is recorded for an unmapped sender — return
		// BEFORE touching sessions/feedback/practice/docs/identities at
		// all, not merely before persisting a "final" result.
		return channels.Outbound{ThreadID: in.ThreadID, Text: refusalText}, nil
	}
	identity := learner.IdentityID(identityStr)

	trimmed := strings.TrimSpace(in.Text)
	if trimmed == "" {
		return channels.Outbound{ThreadID: in.ThreadID, Text: helpText}, nil
	}

	if err := s.ensureIdentity(ctx, identity); err != nil {
		return channels.Outbound{}, fmt.Errorf("channel: ensure identity: %w", err)
	}

	switch {
	case isHelpCommand(trimmed):
		return channels.Outbound{ThreadID: in.ThreadID, Text: helpText}, nil
	case isPracticeCommand(trimmed):
		return s.handlePractice(ctx, in, identity, key)
	default:
		if exerciseID, ok := s.takePending(key); ok {
			return s.handleAnswer(ctx, in, identity, exerciseID)
		}
		return s.handleCorrection(ctx, in, identity, trimmed)
	}
}

// handlePractice starts one exercise (application/practice.Service.
// Start already records quiz.started) and remembers it as key's pending
// answer.
func (s *Service) handlePractice(ctx context.Context, in channels.Inbound, identity learner.IdentityID, key string) (channels.Outbound, error) {
	ex, err := s.practice.Start(ctx, identity)
	if err != nil {
		return channels.Outbound{}, fmt.Errorf("channel: start practice: %w", err)
	}
	s.setPending(key, ex.ID)
	return channels.Outbound{ThreadID: in.ThreadID, Text: renderExercise(ex)}, nil
}

// handleAnswer scores in's text against the pending exerciseID
// (application/practice.Service.Answer already records quiz.answered +
// quiz.completed). No confidence rating is available over a channel
// reply, so it's passed as 0 ("not given") — practice.Service.Answer's
// own documented contract for that value.
func (s *Service) handleAnswer(ctx context.Context, in channels.Inbound, identity learner.IdentityID, exerciseID string) (channels.Outbound, error) {
	eval, err := s.practice.Answer(ctx, identity, exerciseID, strings.TrimSpace(in.Text), 0)
	if err != nil {
		return channels.Outbound{}, fmt.Errorf("channel: answer practice: %w", err)
	}
	return channels.Outbound{ThreadID: in.ThreadID, Text: renderEvaluation(eval)}, nil
}

// handleCorrection is the default route: get-or-create this sender's
// channel session and its single document (see
// getOrCreateChannelSession/getOrCreateChannelDocument), save text as
// the document's latest version, then ask
// application/feedback.Service.RequestFeedback to review the WHOLE
// document (Start=End=0 — RequestFeedback's own documented "empty
// selection means review the whole document" rule) — which is exactly
// text, since it was just saved. RequestFeedback already records
// feedback.requested/correction.presented/hint.shown/grammar.concept.
// encountered.
func (s *Service) handleCorrection(ctx context.Context, in channels.Inbound, identity learner.IdentityID, text string) (channels.Outbound, error) {
	sess, err := s.getOrCreateChannelSession(ctx, identity, in.Channel, in.ExternalID)
	if err != nil {
		return channels.Outbound{}, fmt.Errorf("channel: get or create session: %w", err)
	}
	doc, err := s.getOrCreateChannelDocument(ctx, identity, sess.ID, text)
	if err != nil {
		return channels.Outbound{}, fmt.Errorf("channel: get or create document: %w", err)
	}

	fb, err := s.feedback.RequestFeedback(ctx, feedback.Request{
		Identity:   identity,
		SessionID:  sess.ID,
		DocumentID: doc.ID,
		Text:       text,
	})
	if err != nil {
		return channels.Outbound{}, fmt.Errorf("channel: request feedback: %w", err)
	}
	return channels.Outbound{ThreadID: in.ThreadID, Text: renderCorrectionsCompact(fb.Corrections)}, nil
}

// ensureIdentity auto-provisions a bare learner.Identity the first time
// a mapped sender is ever seen — AllowFrom names an identity string, but
// nothing else in the channel path creates the identities row every
// sessions/feedback/practice write foreign-keys against (see
// db/migrations' identities(id) references). A DisplayName equal to the
// identity string itself is a deliberately minimal placeholder; an
// operator who wants a nicer one can still edit it through the existing
// /learner UI — this never overwrites an identity that already exists.
func (s *Service) ensureIdentity(ctx context.Context, identity learner.IdentityID) error {
	if _, err := s.identities.Get(ctx, identity); err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err := s.identities.Upsert(ctx, learner.Identity{ID: identity, DisplayName: string(identity)}); err != nil {
			return err
		}
	}
	return nil
}

// getOrCreateChannelSession finds identity's existing channel session
// for (channel, externalID) by title (see sessionTitle) or creates one —
// PRD §20's "a channel session is a real session.Session ... created on
// first contact so all the usual events/learner-model updates flow."
// Listing every one of identity's sessions and matching by title (rather
// than, say, a dedicated repository lookup) reuses
// storage.SessionRepository's existing List/Create exactly as they are;
// a channel sender's session count is small enough that this is not a
// scaling concern.
func (s *Service) getOrCreateChannelSession(ctx context.Context, identity learner.IdentityID, channel, externalID string) (session.Session, error) {
	title := sessionTitle(channel, externalID)
	existing, err := s.sessions.List(ctx, identity)
	if err != nil {
		return session.Session{}, err
	}
	for _, sess := range existing {
		if sess.Title == title {
			return sess, nil
		}
	}
	return s.sessions.Create(ctx, identity, title, "channel", session.Profile{})
}

// getOrCreateChannelDocument fetches (creating if needed) sid's single
// document and overwrites its content with text — a channel session has
// exactly one document, and each inbound message becomes that
// document's newest version (storage.DocumentRepository.Save already
// increments Version and appends a document_versions row), rather than
// accumulating every message into one ever-growing document.
func (s *Service) getOrCreateChannelDocument(ctx context.Context, identity learner.IdentityID, sid session.ID, text string) (writing.Document, error) {
	doc, _, err := s.docs.GetOrCreateForSession(ctx, identity, sid)
	if err != nil {
		return writing.Document{}, err
	}
	return s.docs.Save(ctx, identity, doc.ID, text)
}

// setPending/takePending guard the in-memory "awaiting a practice
// answer" map described on Service.pending's own doc comment.
func (s *Service) setPending(key, exerciseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[key] = exerciseID
}

func (s *Service) takePending(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.pending[key]
	if ok {
		delete(s.pending, key)
	}
	return id, ok
}

// allowKey is the exact "<channel>:<external id>" shape
// config.ParseAllowFrom keys its map with.
func allowKey(channel, externalID string) string {
	return channel + ":" + externalID
}

// sessionTitle is PRD §20's exact example shape, "Slack — <external
// id>", generalized over channelDisplayName so a future Signal/WhatsApp
// adapter (Task 5) gets the same convention for free.
func sessionTitle(channel, externalID string) string {
	return fmt.Sprintf("%s — %s", channelDisplayName(channel), externalID)
}

func channelDisplayName(channel string) string {
	switch channel {
	case "slack":
		return "Slack"
	case "signal":
		return "Signal"
	case "whatsapp":
		return "WhatsApp"
	case "":
		return "Channel"
	default:
		return strings.ToUpper(channel[:1]) + channel[1:]
	}
}

func isHelpCommand(trimmed string) bool {
	return strings.EqualFold(trimmed, "help") || trimmed == "ヘルプ"
}

func isPracticeCommand(trimmed string) bool {
	return strings.EqualFold(trimmed, "practice") || trimmed == "練習"
}

// renderExercise formats a started exercise.Exercise as plain text: its
// EN instructions (falling back to JA), the prompt, and — for
// multiple-choice — a numbered list of choices.
func renderExercise(ex exercise.Exercise) string {
	var b strings.Builder
	switch {
	case ex.InstructionsEN != "":
		b.WriteString(ex.InstructionsEN)
	case ex.InstructionsJA != "":
		b.WriteString(ex.InstructionsJA)
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(ex.Prompt)
	for i, choice := range ex.Choices {
		fmt.Fprintf(&b, "\n%d. %s", i+1, choice)
	}
	return b.String()
}

// renderEvaluation formats an exercise.Evaluation as a one-line
// EN-preferred (falling back to JA) verdict.
func renderEvaluation(eval exercise.Evaluation) string {
	feedbackText := eval.FeedbackEN
	if feedbackText == "" {
		feedbackText = eval.FeedbackJA
	}
	if eval.Correct {
		return "✓ " + feedbackText
	}
	return "✗ " + feedbackText
}

// renderCorrectionsCompact is the channel port's compact correction
// rendering (PRD §20.1: "pairs + one-line reasons, not the full card").
// Every correction's gate state is decided by calling correction.
// IsGated — the single socratic pre-reveal predicate (see that
// function's own doc comment) — NEVER by re-deriving hasHint/status/
// revealed logic here: an answer leak has already been found on five
// separate surfaces of this project, every time because a new consumer
// re-implemented this check instead of calling the shared one. A gated
// correction's line carries ONLY cv.Original and its hint text — never
// cv.Replacement, which is the whole point of the gate.
func renderCorrectionsCompact(views []feedback.CorrectionView) string {
	if len(views) == 0 {
		return "No corrections — well done!"
	}
	lines := make([]string, 0, len(views))
	for _, cv := range views {
		if correction.IsGated(cv.HasHint(), cv.Status, cv.Revealed) {
			lines = append(lines, fmt.Sprintf("• %s → 🔒 %s", cv.Original, hintText(cv.Hint)))
			continue
		}
		line := fmt.Sprintf("• %s → %s", cv.Original, cv.Replacement)
		if reason := explanationText(cv.Explanation); reason != "" {
			line += " (" + reason + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// hintText/explanationText both prefer EN, falling back to JA — the
// same "EN preferred, JA fallback" convention renderExercise/
// renderEvaluation already use for channel-appropriate brevity.
func hintText(hint correction.Explanation) string {
	if hint.EN != "" {
		return hint.EN
	}
	return hint.JA
}

func explanationText(exp correction.Explanation) string {
	if exp.EN != "" {
		return exp.EN
	}
	return exp.JA
}
