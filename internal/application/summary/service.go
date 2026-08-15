// Package summary implements the weekly email summary pipeline (Phase
// 3 Task 5, PRD §21, §65): it gathers a learner's aggregate statistics,
// scored priorities, and recently-active vocabulary, asks the summary
// agent to write a weekly_summary.v1-derived subject/body, and hands
// the result to a notifications.Notifier to actually deliver.
package summary

import (
	"context"
	"errors"
	"fmt"

	agentsummary "github.com/mikeyaustin/jlp/internal/agent/summary"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/notifications"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// prioritiesLimit/newExpressionsLimit cap how much context SendWeekly
// hands the summary agent — prioritiesLimit matches
// application/feedback.Service's recentErrorsLimit (Top(5) is the
// established "how many priorities is enough" convention across this
// codebase); newExpressionsLimit matches application/lessons.Service's
// activationLimit (8) for the analogous "how much vocabulary is enough
// for one AI call" cap.
const (
	prioritiesLimit     = 5
	newExpressionsLimit = 8
)

// ErrRecipientRequired is returned by SendWeekly when to is empty —
// there is no address to deliver to, so nothing downstream (agent call,
// SMTP dial) is worth attempting.
var ErrRecipientRequired = errors.New("summary: recipient address is required")

// Service wires the weekly email summary pipeline.
type Service struct {
	analytics *analytics.Service
	prios     storage.PriorityRepository
	vocab     storage.VocabularyRepository
	agent     *agentsummary.Agent
	notifier  notifications.Notifier
	// rec is accepted for constructor-signature symmetry with every
	// other application service in this codebase (anki, lessons,
	// vocabulary, feedback, practice all take a *learning.Recorder), but
	// SendWeekly does not currently record a learning event for the
	// summary send itself: the task brief is explicit that this task
	// adds nothing new to internal/domain/event ("event.go (nothing
	// new)"), and unlike a persisted anki card or lesson guide, a sent
	// email leaves no in-app artifact for a later event to reference by
	// ID — it's an outbound side effect, not something the learner did
	// within the app. Reserved here rather than dropped from the
	// signature entirely, should a future task want an audit trail of
	// summary sends.
	rec *learning.Recorder
}

// NewService wires the weekly email summary pipeline.
func NewService(analyticsSvc *analytics.Service, prios storage.PriorityRepository, vocab storage.VocabularyRepository, agent *agentsummary.Agent, notifier notifications.Notifier, rec *learning.Recorder) *Service {
	return &Service{analytics: analyticsSvc, prios: prios, vocab: vocab, agent: agent, notifier: notifier, rec: rec}
}

// SendWeekly gathers identity's current context — aggregate Statistics
// (application/analytics.Service, the same computed AcceptanceRate/
// CorrectionsPer1000 the dashboard reads), Top(5) priorities (the
// heuristic teaching planner's ranked weaknesses), and identity's most
// recently active vocabulary, capped at 8 — asks the summary agent to
// write a subject/body pair from it, and delivers the result to to via
// the wired Notifier.
//
// Unlike application/lessons.Service.Generate or
// application/anki.Service.GenerateFromCorrection, a Notifier failure
// is NOT log-and-continue: there is no already-persisted artifact to
// protect the caller's success from — the entire point of this call is
// the delivery, so a failed Send is returned as a real error.
//
// Sending is gated entirely by the CALLER: SendWeekly itself performs
// no APP_SUMMARY_ENABLED check (see cmd/jlp/summary.go's
// maybeStartSummaryScheduler, the only place that flag is read) — it is
// impossible for this method to fire on a schedule unless main.go's
// wiring already decided cfg.Summary.Enabled was true, and a direct
// call (the `jlp send-summary` CLI command) is itself an explicit,
// one-off human trigger, PRD §65's opt-in act in person.
func (s *Service) SendWeekly(ctx context.Context, identity learner.IdentityID, to string) error {
	if to == "" {
		return ErrRecipientRequired
	}

	stats, err := s.analytics.Statistics(ctx, identity)
	if err != nil {
		return fmt.Errorf("summary: statistics: %w", err)
	}

	priorities, err := s.prios.Top(ctx, identity, prioritiesLimit)
	if err != nil {
		return fmt.Errorf("summary: top priorities: %w", err)
	}

	recent, err := s.vocab.List(ctx, identity, "")
	if err != nil {
		return fmt.Errorf("summary: list vocabulary: %w", err)
	}
	if len(recent) > newExpressionsLimit {
		recent = recent[:newExpressionsLimit]
	}

	subject, body, _, err := s.agent.Generate(ctx, agentsummary.SummaryInput{
		Identity:       identity,
		StatsSummary:   formatStats(stats),
		Priorities:     formatPriorities(priorities),
		NewExpressions: formatExpressions(recent),
	})
	if err != nil {
		return fmt.Errorf("summary: generate: %w", err)
	}

	if err := s.notifier.Send(ctx, notifications.Notification{To: to, Subject: subject, TextBody: body}); err != nil {
		return fmt.Errorf("summary: send: %w", err)
	}
	return nil
}
