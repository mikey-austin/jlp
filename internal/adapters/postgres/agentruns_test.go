//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// agentRunTestSetup migrates and returns a repo plus two freshly
// upserted identities — the second one exists purely to exercise
// cross-identity isolation, same "throwaway identity per test" pattern
// lessonTestSetup/ankiTestSetup use.
func agentRunTestSetup(t *testing.T) (*AgentRunRepository, learner.IdentityID, learner.IdentityID) {
	t.Helper()
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	identities := NewIdentityRepository(pool)
	a := learner.Identity{ID: learner.IdentityID("test-agentrun-a-" + uuid.NewString()), DisplayName: "A"}
	b := learner.Identity{ID: learner.IdentityID("test-agentrun-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, b); err != nil {
		t.Fatal(err)
	}
	return NewAgentRunRepository(pool), a.ID, b.ID
}

func testAgentRun(identity learner.IdentityID, now time.Time) storage.AgentRun {
	return storage.AgentRun{
		ID:            uuid.NewString(),
		IdentityID:    identity,
		Agent:         "teacher",
		PromptName:    "teacher.agentic",
		PromptVersion: "v1",
		Status:        "running",
		StartedAt:     now,
		System:        "You are an agentic Japanese writing teacher.",
		Input:         "What should I focus on next?",
	}
}

// TestAgentRunStartFinishListGetRoundTrips exercises the full happy
// path the task brief calls for: start, finish, record a tool call,
// list, and get — one round trip through every AgentRunRepository
// method.
func TestAgentRunStartFinishListGetRoundTrips(t *testing.T) {
	repo, identity, _ := agentRunTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	run := testAgentRun(identity, now)

	if err := repo.Start(ctx, run); err != nil {
		t.Fatalf("Start: %v", err)
	}

	call := storage.ToolCall{
		ID:         uuid.NewString(),
		AgentRunID: run.ID,
		ToolName:   "get_learning_priorities",
		Arguments:  `{"limit":5}`,
		Result:     `[{"subject":"i-adjective-past"}]`,
		IsError:    false,
		DurationMS: 42,
		CreatedAt:  now,
	}
	if err := repo.RecordToolCall(ctx, identity, call); err != nil {
		t.Fatalf("RecordToolCall: %v", err)
	}

	turn1 := storage.AgentTurn{ID: uuid.NewString(), AgentRunID: run.ID, TurnNumber: 1, Text: "", CreatedAt: now}
	if err := repo.RecordTurn(ctx, identity, turn1); err != nil {
		t.Fatalf("RecordTurn(1): %v", err)
	}
	turn2 := storage.AgentTurn{ID: uuid.NewString(), AgentRunID: run.ID, TurnNumber: 2, Text: "Focus on i-adjective-past.", CreatedAt: now.Add(time.Second)}
	if err := repo.RecordTurn(ctx, identity, turn2); err != nil {
		t.Fatalf("RecordTurn(2): %v", err)
	}

	endedAt := now.Add(2 * time.Second)
	if err := repo.Finish(ctx, identity, run.ID, "completed", "", "Focus on i-adjective-past.", 2, endedAt); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	list, err := repo.List(ctx, identity, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(List) = %d, want 1", len(list))
	}
	if list[0].Status != "completed" || list[0].Turns != 2 {
		t.Fatalf("List()[0] = %+v, want Status=completed Turns=2", list[0])
	}

	gotRun, gotCalls, gotTurns, err := repo.Get(ctx, identity, run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotRun.Status != "completed" || gotRun.Agent != "teacher" || gotRun.PromptName != "teacher.agentic" {
		t.Fatalf("Get() run = %+v, want Status=completed Agent=teacher PromptName=teacher.agentic", gotRun)
	}
	if !gotRun.EndedAt.Equal(endedAt) {
		t.Fatalf("Get() run.EndedAt = %v, want %v", gotRun.EndedAt, endedAt)
	}
	if gotRun.System != run.System || gotRun.Input != run.Input {
		t.Fatalf("Get() run System/Input = %q/%q, want %q/%q", gotRun.System, gotRun.Input, run.System, run.Input)
	}
	if gotRun.Output != "Focus on i-adjective-past." {
		t.Fatalf("Get() run.Output = %q, want the text Finish was called with", gotRun.Output)
	}
	if len(gotCalls) != 1 {
		t.Fatalf("len(Get() calls) = %d, want 1", len(gotCalls))
	}
	if gotCalls[0].ToolName != "get_learning_priorities" || gotCalls[0].Result != call.Result || gotCalls[0].DurationMS != 42 {
		t.Fatalf("Get() calls[0] = %+v, want ToolName=get_learning_priorities Result=%q DurationMS=42", gotCalls[0], call.Result)
	}
	if len(gotTurns) != 2 {
		t.Fatalf("len(Get() turns) = %d, want 2", len(gotTurns))
	}
	if gotTurns[0].TurnNumber != 1 || gotTurns[0].Text != "" {
		t.Fatalf("Get() turns[0] = %+v, want TurnNumber=1 Text=\"\"", gotTurns[0])
	}
	if gotTurns[1].TurnNumber != 2 || gotTurns[1].Text != "Focus on i-adjective-past." {
		t.Fatalf("Get() turns[1] = %+v, want TurnNumber=2 Text=%q", gotTurns[1], "Focus on i-adjective-past.")
	}
}

// TestAgentRunGetCrossIdentityMisses pins the identity-scoped access
// control every repository in this package uses: identity B must not
// be able to read identity A's run.
func TestAgentRunGetCrossIdentityMisses(t *testing.T) {
	repo, identityA, identityB := agentRunTestSetup(t)
	ctx := context.Background()
	run := testAgentRun(identityA, time.Now().UTC().Truncate(time.Microsecond))
	if err := repo.Start(ctx, run); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, _, _, err := repo.Get(ctx, identityB, run.ID)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get(wrong identity) err = %v, want storage.ErrNotFound", err)
	}
}

// TestAgentRunFinishCrossIdentityMisses mirrors the Get case for
// Finish: identity B must not be able to finish identity A's run.
func TestAgentRunFinishCrossIdentityMisses(t *testing.T) {
	repo, identityA, identityB := agentRunTestSetup(t)
	ctx := context.Background()
	run := testAgentRun(identityA, time.Now().UTC().Truncate(time.Microsecond))
	if err := repo.Start(ctx, run); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err := repo.Finish(ctx, identityB, run.ID, "completed", "", "", 1, time.Now().UTC())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Finish(wrong identity) err = %v, want storage.ErrNotFound", err)
	}

	// Confirm the real owner's run is untouched — still "running".
	gotRun, _, _, err := repo.Get(ctx, identityA, run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotRun.Status != "running" {
		t.Fatalf("Status = %q, want running (identity B's Finish attempt must not have taken effect)", gotRun.Status)
	}
}

// TestAgentRunRecordToolCallRefusesRunBelongingToAnotherIdentity pins
// the task brief's explicit requirement: RecordToolCall must refuse a
// run belonging to another identity, writing nothing.
func TestAgentRunRecordToolCallRefusesRunBelongingToAnotherIdentity(t *testing.T) {
	repo, identityA, identityB := agentRunTestSetup(t)
	ctx := context.Background()
	run := testAgentRun(identityA, time.Now().UTC().Truncate(time.Microsecond))
	if err := repo.Start(ctx, run); err != nil {
		t.Fatalf("Start: %v", err)
	}

	call := storage.ToolCall{ID: uuid.NewString(), AgentRunID: run.ID, ToolName: "get_active_session", CreatedAt: time.Now().UTC()}
	err := repo.RecordToolCall(ctx, identityB, call)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("RecordToolCall(wrong identity) err = %v, want storage.ErrNotFound", err)
	}

	// Confirm nothing was written: the real owner's Get shows zero calls.
	_, calls, _, err := repo.Get(ctx, identityA, run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("len(calls) = %d, want 0 — a cross-identity RecordToolCall must write nothing", len(calls))
	}
}

// TestAgentRunRecordTurnRefusesRunBelongingToAnotherIdentity mirrors
// the RecordToolCall case above for RecordTurn.
func TestAgentRunRecordTurnRefusesRunBelongingToAnotherIdentity(t *testing.T) {
	repo, identityA, identityB := agentRunTestSetup(t)
	ctx := context.Background()
	run := testAgentRun(identityA, time.Now().UTC().Truncate(time.Microsecond))
	if err := repo.Start(ctx, run); err != nil {
		t.Fatalf("Start: %v", err)
	}

	turn := storage.AgentTurn{ID: uuid.NewString(), AgentRunID: run.ID, TurnNumber: 1, CreatedAt: time.Now().UTC()}
	err := repo.RecordTurn(ctx, identityB, turn)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("RecordTurn(wrong identity) err = %v, want storage.ErrNotFound", err)
	}

	_, _, turns, err := repo.Get(ctx, identityA, run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("len(turns) = %d, want 0 — a cross-identity RecordTurn must write nothing", len(turns))
	}
}

// TestAgentRunListIsIdentityScoped pins that List never leaks another
// identity's runs.
func TestAgentRunListIsIdentityScoped(t *testing.T) {
	repo, identityA, identityB := agentRunTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.Start(ctx, testAgentRun(identityA, now)); err != nil {
		t.Fatalf("Start(A): %v", err)
	}
	if err := repo.Start(ctx, testAgentRun(identityB, now)); err != nil {
		t.Fatalf("Start(B): %v", err)
	}

	list, err := repo.List(ctx, identityA, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].IdentityID != identityA {
		t.Fatalf("List(A) = %+v, want exactly identityA's one run", list)
	}
}

// TestAgentRunGetUnknownRunMisses pins a plain not-found lookup
// (neither identity has ever started this run ID).
func TestAgentRunGetUnknownRunMisses(t *testing.T) {
	repo, identity, _ := agentRunTestSetup(t)
	_, _, _, err := repo.Get(context.Background(), identity, uuid.NewString())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get(unknown run) err = %v, want storage.ErrNotFound", err)
	}
}
