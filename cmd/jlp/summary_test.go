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
// the scheduler is real, not just present. "@every 1s" is the fastest
// tick robfig/cron actually produces regardless of what's requested —
// see TestMaybeStartSummarySchedulerSurvivesSendWeeklyPanic's comment
// below for why.
func TestMaybeStartSummarySchedulerEnabledRunsOnSchedule(t *testing.T) {
	svc := &fakeSummarySender{}
	cfg := config.Config{
		Auth:    config.Auth{Static: config.StaticIdentity{ID: "dev"}},
		Summary: config.Summary{Enabled: true, Cron: "@every 1s", To: "learner@jlp.local"},
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

// panicSummarySender is a weeklySummarySender double whose SendWeekly
// panics on every call, recording the call FIRST so a test can observe
// how many times cron actually invoked it despite every single call
// blowing up.
type panicSummarySender struct {
	mu    sync.Mutex
	calls int
}

func (f *panicSummarySender) SendWeekly(context.Context, learner.IdentityID, string) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	panic("boom: simulated SendWeekly panic")
}

func (f *panicSummarySender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestMaybeStartSummarySchedulerSurvivesSendWeeklyPanic is the
// regression test for the code-review finding that an unrecovered
// panic in the scheduled job would crash the ENTIRE process — live
// HTTP server included, not just the summary feature — since
// robfig/cron v3's default chain is empty and every job runs in its
// own bare goroutine with nothing above it to recover. Wiring a
// service whose SendWeekly always panics through the EXACT production
// constructor (maybeStartSummaryScheduler, which uses newSummaryCron's
// cron.WithChain(cron.Recover(...)) plus summaryJob's own
// defer/recover) must not kill this test process, and the panicking
// job must keep firing on every subsequent tick — proving the
// scheduler actually recovers and stays live, not just that one panic
// happened to be swallowed once.
//
// If this test reaches its final assertion at all, that IS part of the
// proof: an unrecovered panic escaping cron's bare `go func(){
// j.Run() }()` would crash this entire go test binary outright, not
// produce an ordinary failing assertion.
func TestMaybeStartSummarySchedulerSurvivesSendWeeklyPanic(t *testing.T) {
	svc := &panicSummarySender{}
	cfg := config.Config{
		Auth: config.Auth{Static: config.StaticIdentity{ID: "dev"}},
		// robfig/cron's Every() silently rounds any sub-second duration
		// UP to a 1-second minimum (only a log.Print warning, no error —
		// see cron.go's Every) — "@every 1s" here is the fastest tick
		// this scheduler can actually produce, not an arbitrary choice.
		Summary: config.Summary{Enabled: true, Cron: "@every 1s", To: "learner@jlp.local"},
	}
	c, err := maybeStartSummaryScheduler(cfg, svc)
	if err != nil {
		t.Fatalf("maybeStartSummaryScheduler returned error: %v", err)
	}
	t.Cleanup(func() { <-c.Stop().Done() })

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && svc.callCount() < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	if svc.callCount() < 3 {
		t.Fatalf("panicking SendWeekly was called %d times in 6s, want >= 3 — the scheduler must survive each panic and keep re-firing this job rather than dying or giving up after the first one", svc.callCount())
	}
}

// TestNewSummaryCronRecoversPanicsAndKeepsRunningOtherJobs exercises
// newSummaryCron directly (the exact constructor
// maybeStartSummaryScheduler itself uses) with two independent jobs —
// one that panics every tick, one that doesn't — proving cron.Recover's
// chain wrapper isolates a panicking entry from the rest of the
// scheduler: an unrelated job keeps running on its own schedule the
// whole time, and the panicking one keeps being re-invoked rather than
// being silently dropped after its first panic.
func TestNewSummaryCronRecoversPanicsAndKeepsRunningOtherJobs(t *testing.T) {
	c := newSummaryCron()

	var mu sync.Mutex
	panics, oks := 0, 0
	// "@every 1s": see TestMaybeStartSummarySchedulerSurvivesSendWeeklyPanic's
	// comment above — robfig/cron rounds any sub-second spec up to 1s anyway.
	if _, err := c.AddFunc("@every 1s", func() {
		mu.Lock()
		panics++
		mu.Unlock()
		panic("boom")
	}); err != nil {
		t.Fatalf("AddFunc(panicking job): %v", err)
	}
	if _, err := c.AddFunc("@every 1s", func() {
		mu.Lock()
		oks++
		mu.Unlock()
	}); err != nil {
		t.Fatalf("AddFunc(ok job): %v", err)
	}
	c.Start()
	t.Cleanup(func() { <-c.Stop().Done() })

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		p, o := panics, oks
		mu.Unlock()
		if p >= 3 && o >= 3 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("panics=%d oks=%d after 6s, want both >= 3 (the panicking job must keep re-firing, and the unrelated ok job must be unaffected by it)", panics, oks)
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
