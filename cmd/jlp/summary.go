package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	smtpadapter "github.com/mikeyaustin/jlp/internal/adapters/smtp"
	agentsummary "github.com/mikeyaustin/jlp/internal/agent/summary"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	appsummary "github.com/mikeyaustin/jlp/internal/application/summary"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// weeklySummarySender is the minimal capability
// maybeStartSummaryScheduler needs — satisfied by *appsummary.Service
// in production and a call-capturing fake in summary_test.go, so the
// scheduler-gating test below needs neither a live database nor a real
// SMTP server: it only has to prove a cron.Cron is (or isn't)
// constructed, never that a send actually happens end to end (that's
// application/summary's own service_test.go's job).
type weeklySummarySender interface {
	SendWeekly(ctx context.Context, identity learner.IdentityID, to string) error
}

// slogCronLogger adapts slog's package-level logger to robfig/cron's
// own Logger interface, so cron.Recover's panic log (see newSummaryCron
// below) lands in the same structured JSON stream every other JLP log
// line uses instead of cron.DefaultLogger's separate, unstructured
// "cron: ..." line to stdout.
type slogCronLogger struct{}

func (slogCronLogger) Info(msg string, keysAndValues ...any) {
	slog.Info("cron: "+msg, keysAndValues...)
}

func (slogCronLogger) Error(err error, msg string, keysAndValues ...any) {
	slog.Error("cron: "+msg, append([]any{"err", err}, keysAndValues...)...)
}

// newSummaryCron returns a *cron.Cron wired with cron.WithChain(cron.
// Recover(...)) — WITHOUT this, robfig/cron v3's default chain is
// empty and every job runs via Cron.startJob's bare `go func(){
// j.Run() }()`, with nothing above it to recover a panic: an
// unrecovered panic anywhere in a scheduled SendWeekly call —
// agent.Generate unmarshaling an unexpected AI response shape, a
// template render, an SMTP write, any of it, unattended, once a week —
// would crash a bare goroutine with no recover in its call stack, which
// takes the ENTIRE process down, live HTTP server included, not just
// the summary feature. cron.Recover wraps every job so a panic is
// caught, logged (with a stack trace) via slogCronLogger above, and the
// scheduler keeps running everything else exactly as before — see
// summary_test.go's TestMaybeStartSummarySchedulerSurvivesSendWeeklyPanic
// and TestNewSummaryCronRecoversPanicsAndKeepsRunningOtherJobs for the
// proof (both go through this exact constructor, not just a bare
// recover() call in isolation).
func newSummaryCron() *robfigcron.Cron {
	return robfigcron.New(robfigcron.WithChain(robfigcron.Recover(slogCronLogger{})))
}

// summaryJob returns the cron job function for identity/to: it calls
// svc.SendWeekly and logs a failure. This closure ALSO carries its own
// defer/recover — belt-and-suspenders on top of newSummaryCron's
// cron.Recover chain wrapper above — so a panic is logged here with
// this job's own identity/to context (which the generic chain-level
// logger has no way to know about) before cron.Recover's own catch (if
// this recover somehow didn't run first) would log it more generically.
func summaryJob(svc weeklySummarySender, identity learner.IdentityID, to string) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("summary: scheduled send panicked", "identity", identity, "to", to, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		if err := svc.SendWeekly(context.Background(), identity, to); err != nil {
			slog.Error("summary: scheduled send failed", "identity", identity, "to", to, "err", err)
		}
	}
}

// maybeStartSummaryScheduler wires the weekly-summary cron job (PRD
// §21, §65) — but ONLY when cfg.Summary.Enabled is true. This is the
// explicit opt-in boundary for JLP's one piece of automatic outbound
// external communication: an operator who never sets
// APP_SUMMARY_ENABLED gets a nil *cron.Cron back here, no scheduler is
// ever constructed, and svc.SendWeekly can never fire on a schedule —
// see summary_test.go's TestMaybeStartSummarySchedulerDisabledByDefault
// for the proof. (The `jlp send-summary` CLI command, runSendSummary
// below, is a separate, always-available manual trigger — Enabled
// gates the SCHEDULER, not a human explicitly asking for one send.)
//
// The job always targets cfg.Auth.Static.ID — multi-identity scheduling
// (a distinct weekly summary per real learner account) is a Phase 4
// concern; today's static-identity auth mode is JLP's only supported
// multi-user story in the first place.
//
// cfg.Summary.Cron is already validated by config.validate() when
// Enabled is true (invalid cron syntax is a boot error there, before
// this function is ever reached in production) — AddFunc's error return
// is still checked here as defense in depth for any other caller.
func maybeStartSummaryScheduler(cfg config.Config, svc weeklySummarySender) (*robfigcron.Cron, error) {
	if !cfg.Summary.Enabled {
		return nil, nil
	}

	identity := learner.IdentityID(cfg.Auth.Static.ID)
	c := newSummaryCron()
	if _, err := c.AddFunc(cfg.Summary.Cron, summaryJob(svc, identity, cfg.Summary.To)); err != nil {
		return nil, fmt.Errorf("summary: invalid APP_SUMMARY_CRON %q: %w", cfg.Summary.Cron, err)
	}
	c.Start()
	// "from" is deliberately NOT logged here: config.Summary.From is not
	// currently wired into the SMTP transport at all (see that field's
	// doc comment) — logging it alongside cron/to would misleadingly
	// imply it takes effect.
	slog.Info("summary: weekly scheduler started", "cron", cfg.Summary.Cron, "to", cfg.Summary.To)
	return c, nil
}

// runSendSummary is `jlp send-summary` (wrapped by `make send-summary`):
// a manual, explicit trigger that sends ONE weekly summary immediately,
// bypassing cfg.Summary.Enabled entirely — Enabled only gates the
// AUTOMATIC cron scheduler (see maybeStartSummaryScheduler's doc
// comment); a human explicitly running this command is itself the
// opt-in act PRD §65 asks for. Fails fast if APP_SUMMARY_TO is empty —
// there is no recipient to send to.
func runSendSummary(ctx context.Context, cfg config.Config) error {
	if cfg.Summary.To == "" {
		return fmt.Errorf("send-summary: APP_SUMMARY_TO is required")
	}

	pool, err := postgres.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("send-summary: connect: %w", err)
	}
	defer pool.Close()

	aiRequestRepo := postgres.NewAIRequestRepository(pool)
	aiGen, err := buildAIGenerator(cfg, aiRequestRepo)
	if err != nil {
		return fmt.Errorf("send-summary: ai: %w", err)
	}

	svc := appsummary.NewService(
		analytics.NewService(postgres.NewAnalyticsRepository(pool)),
		postgres.NewPriorityRepository(pool),
		postgres.NewVocabularyRepository(pool),
		agentsummary.New(aiGen),
		smtpadapter.New(cfg.SMTP),
	)

	identity := learner.IdentityID(cfg.Auth.Static.ID)
	if err := svc.SendWeekly(ctx, identity, cfg.Summary.To); err != nil {
		return fmt.Errorf("send-summary: %w", err)
	}
	// "from" is deliberately NOT logged here — see
	// maybeStartSummaryScheduler's matching comment above.
	slog.Info("send-summary: sent", "identity", identity, "to", cfg.Summary.To)
	return nil
}
