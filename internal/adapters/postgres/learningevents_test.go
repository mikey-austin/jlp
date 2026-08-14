//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

func TestLearningEventAppendAndListRecentScopedByIdentity(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// FK chain: identities -> sessions -> learning_events.
	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-event-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-event-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identityA.ID,
		Title:      "旅行について書く",
		Purpose:    "Diary",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	events := NewLearningEventRepository(pool)

	// A session-scoped event and a session-less event for identity A.
	scoped := event.LearningEvent{
		ID:         uuid.NewString(),
		IdentityID: identityA.ID,
		SessionID:  &sess.ID,
		Type:       event.TypeWritingUpdated,
		Subject:    "doc-1",
		Evidence:   map[string]any{"rune_count": float64(12), "version": float64(2)},
		OccurredAt: now,
	}
	if err := events.Append(ctx, scoped); err != nil {
		t.Fatal(err)
	}
	sessionless := event.LearningEvent{
		ID:         uuid.NewString(),
		IdentityID: identityA.ID,
		Type:       event.TypeFeedbackRequested,
		Subject:    "doc-1",
		OccurredAt: now.Add(time.Second),
	}
	if err := events.Append(ctx, sessionless); err != nil {
		t.Fatal(err)
	}

	// An event for identity B must never show up in identity A's list.
	other := event.LearningEvent{
		ID:         uuid.NewString(),
		IdentityID: identityB.ID,
		Type:       event.TypeWritingUpdated,
		OccurredAt: now,
	}
	if err := events.Append(ctx, other); err != nil {
		t.Fatal(err)
	}

	// ListRecent with no session filter: both of identity A's events,
	// newest first.
	list, err := events.ListRecent(ctx, identityA.ID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListRecent (no session filter) returned %d events, want 2", len(list))
	}
	if list[0].ID != sessionless.ID || list[1].ID != scoped.ID {
		t.Fatalf("ListRecent order = [%s,%s], want newest-first [%s,%s]", list[0].ID, list[1].ID, sessionless.ID, scoped.ID)
	}
	if list[1].SessionID == nil || *list[1].SessionID != sess.ID {
		t.Fatalf("scoped event SessionID = %v, want %s", list[1].SessionID, sess.ID)
	}
	if list[0].SessionID != nil {
		t.Fatalf("session-less event SessionID = %v, want nil", list[0].SessionID)
	}
	if got := list[1].Evidence["rune_count"]; got != float64(12) {
		t.Fatalf("scoped event Evidence[rune_count] = %v, want 12", got)
	}

	// ListRecent scoped to the session: only the scoped event.
	bySession, err := events.ListRecent(ctx, identityA.ID, &sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bySession) != 1 || bySession[0].ID != scoped.ID {
		t.Fatalf("ListRecent (session filter) = %+v, want just %s", bySession, scoped.ID)
	}

	// limit is honored.
	limited, err := events.ListRecent(ctx, identityA.ID, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("ListRecent limit=1 returned %d rows, want 1", len(limited))
	}
}
