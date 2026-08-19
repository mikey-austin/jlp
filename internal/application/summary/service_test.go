package summary_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double; PRD §75 forbids agents/application importing real adapters, not fakes constructed in tests
	agentsummary "github.com/mikeyaustin/jlp/internal/agent/summary"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	appsummary "github.com/mikeyaustin/jlp/internal/application/summary"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/notifications"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const testIdentity = learner.IdentityID("learner-a")

// --- fakes: same "in-memory, panic on unused methods" shape as
// application/lessons/service_test.go. ---

type fakeAnalyticsRepo struct {
	stats storage.Statistics
}

func (f *fakeAnalyticsRepo) Statistics(context.Context, learner.IdentityID) (storage.Statistics, error) {
	return f.stats, nil
}

func (f *fakeAnalyticsRepo) VocabFunnel(context.Context, learner.IdentityID) (storage.VocabFunnel, error) {
	panic("not used by summary tests")
}

func (f *fakeAnalyticsRepo) WeaknessTrends(context.Context, learner.IdentityID) ([]storage.SubjectTrend, error) {
	panic("not used by summary tests")
}

func (f *fakeAnalyticsRepo) ConfidenceCalibration(context.Context, learner.IdentityID) ([]storage.ConfidenceCalibration, error) {
	panic("not used by summary tests")
}

func (f *fakeAnalyticsRepo) AgentUsage(context.Context, learner.IdentityID) ([]storage.AgentUsage, error) {
	panic("not used by summary tests")
}

func (f *fakeAnalyticsRepo) SystemStats(context.Context, learner.IdentityID) (storage.SystemStats, error) {
	panic("not used by summary tests")
}

// fakePriorityRepo is a minimal storage.PriorityRepository double:
// SendWeekly only ever calls Top.
type fakePriorityRepo struct {
	top      []storage.Priority
	topCalls []int
}

func (f *fakePriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by summary service tests")
}

func (f *fakePriorityRepo) Top(_ context.Context, _ learner.IdentityID, limit int) ([]storage.Priority, error) {
	f.topCalls = append(f.topCalls, limit)
	return f.top, nil
}

// fakeVocabRepo is a minimal storage.VocabularyRepository double:
// SendWeekly only ever calls List.
type fakeVocabRepo struct {
	items      []vocabulary.Item
	listFilter []string
}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used by summary service tests")
}
func (f *fakeVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	panic("not used by summary service tests")
}
func (f *fakeVocabRepo) List(_ context.Context, _ learner.IdentityID, filter string) ([]vocabulary.Item, error) {
	f.listFilter = append(f.listFilter, filter)
	return f.items, nil
}
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	panic("not used by summary service tests")
}
func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	panic("not used by summary service tests")
}
func (f *fakeVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by summary service tests")
}

func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used by summary service tests")
}
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used by summary service tests")
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	return nil
}

// fakeNotifier captures every Notification handed to Send.
type fakeNotifier struct {
	sent    []notifications.Notification
	sendErr error
}

func (f *fakeNotifier) Send(_ context.Context, n notifications.Notification) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, n)
	return nil
}

// spyGenerator wraps a real ai.StructuredGenerator (fakeai, for its
// canned weekly_summary.v1 response) while recording the last request
// it saw, so tests can assert on exactly what SendWeekly rendered into
// the prompt (e.g. how many vocabulary lines) without the agent package
// exposing that intermediate state itself.
type spyGenerator struct {
	inner   ai.StructuredGenerator
	lastReq ai.StructuredRequest
}

func (s *spyGenerator) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.lastReq = req
	return s.inner.GenerateStructured(ctx, req)
}

type testHarness struct {
	svc      *appsummary.Service
	analytic *fakeAnalyticsRepo
	prios    *fakePriorityRepo
	vocab    *fakeVocabRepo
	notifier *fakeNotifier
	spy      *spyGenerator
}

func newTestHarness() *testHarness {
	analyticRepo := &fakeAnalyticsRepo{}
	prios := &fakePriorityRepo{}
	vocab := &fakeVocabRepo{}
	notifier := &fakeNotifier{}
	spy := &spyGenerator{inner: fakeai.New()}
	agent := agentsummary.New(spy)
	svc := appsummary.NewService(analytics.NewService(analyticRepo), prios, vocab, agent, notifier)
	return &testHarness{svc: svc, analytic: analyticRepo, prios: prios, vocab: vocab, notifier: notifier, spy: spy}
}

// --- SendWeekly ---

// TestSendWeeklyGathersContextAndDeliversCannedContent pins the brief's
// Step 1 happy path: Top(5) priorities and List("") vocabulary are both
// fetched, the fakeai-generated (canned, encouraging) weekly summary is
// composed into a Notification, and delivered to exactly the requested
// recipient.
func TestSendWeeklyGathersContextAndDeliversCannedContent(t *testing.T) {
	h := newTestHarness()
	h.analytic.stats = storage.Statistics{RunesWritten: 500, SessionCount: 3, FeedbackRequests: 4, CorrectionsPresented: 2, CorrectionsAccepted: 1, CorrectionsRejected: 1}
	h.prios.top = []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 2.5, Reason: "recurring weakness: 5 occurrences in 30d"},
	}
	h.vocab.items = []vocabulary.Item{
		{Expression: "それはそれとして", Reading: "それはそれとして", Meaning: "話題を切り替える際に使う表現", Lookups: 3},
	}

	err := h.svc.SendWeekly(context.Background(), testIdentity, "learner@jlp.local")
	if err != nil {
		t.Fatalf("SendWeekly returned error: %v", err)
	}

	if len(h.prios.topCalls) != 1 || h.prios.topCalls[0] != 5 {
		t.Fatalf("Top calls = %v, want exactly one call with limit 5", h.prios.topCalls)
	}
	if len(h.vocab.listFilter) != 1 || h.vocab.listFilter[0] != "" {
		t.Fatalf("List calls = %v, want exactly one call with filter \"\"", h.vocab.listFilter)
	}

	if len(h.notifier.sent) != 1 {
		t.Fatalf("sent = %d notifications, want 1", len(h.notifier.sent))
	}
	n := h.notifier.sent[0]
	if n.To != "learner@jlp.local" {
		t.Fatalf("To = %q, want learner@jlp.local", n.To)
	}
	if n.Subject == "" {
		t.Fatal("Subject is empty, want the canned fixture's subject")
	}
	if !strings.Contains(n.TextBody, "面白かったです") {
		t.Fatalf("TextBody missing canned content 面白かったです: %s", n.TextBody)
	}
	if !strings.Contains(n.TextBody, "それはそれとして") {
		t.Fatalf("TextBody missing canned content それはそれとして: %s", n.TextBody)
	}
	if !strings.Contains(n.TextBody, "What you accomplished this week:") {
		t.Fatalf("TextBody missing a composed section heading: %s", n.TextBody)
	}
}

// TestSendWeeklyEmptyContextStillSucceeds pins the brand-new-identity
// path: no priorities/vocabulary yet still produces a valid send
// (fakeai's canned summary doesn't depend on the prompt's content).
func TestSendWeeklyEmptyContextStillSucceeds(t *testing.T) {
	h := newTestHarness()
	if err := h.svc.SendWeekly(context.Background(), testIdentity, "learner@jlp.local"); err != nil {
		t.Fatalf("SendWeekly returned error: %v", err)
	}
	if len(h.notifier.sent) != 1 {
		t.Fatalf("sent = %d notifications, want 1", len(h.notifier.sent))
	}
}

// TestSendWeeklyRequiresRecipient pins the brief's "errors if To empty"
// contract at the service layer: no recipient means no context is even
// gathered, and the notifier is never called.
func TestSendWeeklyRequiresRecipient(t *testing.T) {
	h := newTestHarness()
	err := h.svc.SendWeekly(context.Background(), testIdentity, "")
	if !errors.Is(err, appsummary.ErrRecipientRequired) {
		t.Fatalf("err = %v, want ErrRecipientRequired", err)
	}
	if len(h.notifier.sent) != 0 {
		t.Fatal("notifier.Send was called despite an empty recipient")
	}
	if len(h.prios.topCalls) != 0 {
		t.Fatal("context was gathered despite an empty recipient")
	}
}

// TestSendWeeklyPropagatesNotifierFailure pins the "no log-and-continue"
// contract Service's doc comment describes: unlike
// application/lessons.Service.Generate's event-record failure, a failed
// Send is the entire point of the call and must be returned, not
// swallowed.
func TestSendWeeklyPropagatesNotifierFailure(t *testing.T) {
	h := newTestHarness()
	h.notifier.sendErr = errors.New("smtp: connection refused")

	err := h.svc.SendWeekly(context.Background(), testIdentity, "learner@jlp.local")
	if err == nil {
		t.Fatal("SendWeekly returned nil error, want the notifier's failure to propagate")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want it to wrap the notifier's error", err)
	}
}

// TestSendWeeklyCapsNewExpressionsAt8 pins the newExpressionsLimit cap:
// with 10 vocabulary items available, only the first 8 are rendered
// into the agent's prompt — asserted via spyGenerator's captured
// request, since fakeai's canned response doesn't otherwise reveal how
// much of the prompt was actually used.
func TestSendWeeklyCapsNewExpressionsAt8(t *testing.T) {
	h := newTestHarness()
	for i := 1; i <= 10; i++ {
		h.vocab.items = append(h.vocab.items, vocabulary.Item{
			Expression: "expr" + string(rune('0'+i%10)),
			Meaning:    "meaning",
		})
	}

	if err := h.svc.SendWeekly(context.Background(), testIdentity, "learner@jlp.local"); err != nil {
		t.Fatalf("SendWeekly returned error: %v", err)
	}

	count := strings.Count(h.spy.lastReq.User, "meaning")
	if count != 8 {
		t.Fatalf("rendered %d vocabulary lines, want 8 (newExpressionsLimit)", count)
	}
}

// ListPage delegates to List and truncates. This package does not page;
// implemented rather than stubbed so it returns real rows if it ever
// starts. See storage.VocabularyRepository.ListPage.
func (f *fakeVocabRepo) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
	all, err := f.List(ctx, identity, filter)
	if err != nil || len(all) <= limit {
		return all, storage.VocabularyCursor{}, err
	}
	page := all[:limit]
	last := page[len(page)-1]
	return page, storage.VocabularyCursor{LastEvent: last.LastEvent, ID: last.ID}, nil
}

// ListRecentUnpracticed is 練習's word-drill source; no test in this
// package drills words, so reaching it means a wiring mistake.
func (r *fakeVocabRepo) ListRecentUnpracticed(context.Context, learner.IdentityID, time.Time, int) ([]vocabulary.Item, error) {
	panic("not used by these tests")
}
