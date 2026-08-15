package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// fakeSummarySender is a weeklySummarySender test double that records
// every SendWeekly call — cheap enough to prove the scheduler-gating
// contract without a database or a real notifications.Notifier (the
// brief's "disabled-notifier-never-called-when-config-off ... boot
// test if cheap" — this is that test).
type fakeSummarySender struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeSummarySender) SendWeekly(context.Context, learner.IdentityID, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil
}

func (f *fakeSummarySender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestMaybeStartSummarySchedulerDisabledByDefault pins PRD §65's
// opt-in boundary: with cfg.Summary.Enabled left false (the zero value
// — what an operator gets from a plain `make up`, no APP_SUMMARY_*
// set), maybeStartSummaryScheduler returns a nil *cron.Cron and
// constructs nothing at all — no cron job is ever registered, so
// svc.SendWeekly can never fire on a schedule. This is the proof that
// sending is impossible when APP_SUMMARY_ENABLED is unset/false.
func TestMaybeStartSummarySchedulerDisabledByDefault(t *testing.T) {
	svc := &fakeSummarySender{}
	c, err := maybeStartSummaryScheduler(config.Config{}, svc)
	if err != nil {
		t.Fatalf("maybeStartSummaryScheduler returned error: %v", err)
	}
	if c != nil {
		t.Fatal("expected a nil *cron.Cron when Summary.Enabled is false")
	}
	if svc.callCount() != 0 {
		t.Fatalf("SendWeekly was called %d times, want 0 (scheduler must not exist at all)", svc.callCount())
	}
}

// TestMaybeStartSummarySchedulerEnabledRunsOnSchedule pins the positive
// case: Enabled=true with a cron spec that fires every second actually
// invokes svc.SendWeekly, at least once, within a short wait — proving
// the scheduler is real, not just present.
func TestMaybeStartSummarySchedulerEnabledRunsOnSchedule(t *testing.T) {
	svc := &fakeSummarySender{}
	cfg := config.Config{
		Auth:    config.Auth{Static: config.StaticIdentity{ID: "dev"}},
		Summary: config.Summary{Enabled: true, Cron: "@every 100ms", To: "learner@jlp.local"},
	}
	c, err := maybeStartSummaryScheduler(cfg, svc)
	if err != nil {
		t.Fatalf("maybeStartSummaryScheduler returned error: %v", err)
	}
	if c == nil {
		t.Fatal("expected a non-nil *cron.Cron when Summary.Enabled is true")
	}
	t.Cleanup(func() { <-c.Stop().Done() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if svc.callCount() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("SendWeekly was never called by the running scheduler")
}

// TestMaybeStartSummarySchedulerRejectsInvalidCron pins the defense-in-
// depth check: an unparseable cron spec is a boot error, not a silently
// inert scheduler.
func TestMaybeStartSummarySchedulerRejectsInvalidCron(t *testing.T) {
	svc := &fakeSummarySender{}
	cfg := config.Config{Summary: config.Summary{Enabled: true, Cron: "not a cron spec"}}
	if _, err := maybeStartSummaryScheduler(cfg, svc); err == nil {
		t.Fatal("expected an error for an invalid cron spec")
	}
}

// TestRunSendSummaryRequiresTo pins the brief's "errors if To empty"
// contract for the CLI command — checked before any database
// connection is attempted, so this needs no live postgres.
func TestRunSendSummaryRequiresTo(t *testing.T) {
	err := runSendSummary(context.Background(), config.Config{})
	if err == nil {
		t.Fatal("expected an error when APP_SUMMARY_TO is empty")
	}
}
