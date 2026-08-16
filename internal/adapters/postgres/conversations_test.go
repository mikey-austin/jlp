//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// conversationTestSetup migrates and returns a repo plus two freshly
// upserted identities, each with their own session — the FK chain
// identities -> sessions -> conversations -> conversation_turns —
// mirroring documents_test.go's own two-identity setup so cross-
// identity isolation can be exercised directly.
func conversationTestSetup(t *testing.T) (*ConversationRepository, learner.IdentityID, session.ID, learner.IdentityID, session.ID) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-conv-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-conv-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sessA := session.Session{ID: session.ID(uuid.New().String()), IdentityID: identityA.ID, Title: "A's session", Purpose: "Diary", CreatedAt: now, UpdatedAt: now}
	sessB := session.Session{ID: session.ID(uuid.New().String()), IdentityID: identityB.ID, Title: "B's session", Purpose: "Diary", CreatedAt: now, UpdatedAt: now}
	if err := sessions.Create(ctx, sessA); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Create(ctx, sessB); err != nil {
		t.Fatal(err)
	}

	return NewConversationRepository(pool), identityA.ID, sessA.ID, identityB.ID, sessB.ID
}

func TestConversationGetOrCreateForSessionIsIdempotent(t *testing.T) {
	repo, identityA, sessA, _, _ := conversationTestSetup(t)
	ctx := context.Background()

	id1, created1, err := repo.GetOrCreateForSession(ctx, identityA, sessA)
	if err != nil {
		t.Fatal(err)
	}
	if !created1 {
		t.Fatal("first call: created = false, want true")
	}

	id2, created2, err := repo.GetOrCreateForSession(ctx, identityA, sessA)
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Fatal("second call: created = true, want false")
	}
	if id1 != id2 {
		t.Fatalf("GetOrCreateForSession returned a different conversation on second call: %q != %q", id1, id2)
	}
}

func TestConversationInsertAndListTurnsRoundTripsCorrections(t *testing.T) {
	repo, identityA, sessA, _, _ := conversationTestSetup(t)
	ctx := context.Background()

	convID, _, err := repo.GetOrCreateForSession(ctx, identityA, sessA)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	turn := storage.ConversationTurn{
		ID:          uuid.NewString(),
		Position:    1,
		LearnerText: "とても面白いでした。",
		Reply:       "なるほど、教えてくれてありがとうございます。",
		ReplyEN:     "I see, thanks for telling me.",
		Followup:    "それについてもっと聞かせてください。",
		Corrections: []correction.Correction{{
			ID:          uuid.NewString(),
			Original:    "面白いでした",
			Replacement: "面白かったです",
			Type:        correction.Type("conjugation"),
			Severity:    correction.Severity("incorrect"),
			Explanation: correction.Explanation{JA: "説明", EN: "explanation"},
			Concepts:    []string{"i-adjective-past"},
		}},
		CreatedAt: now,
	}
	if err := repo.InsertTurn(ctx, identityA, convID, turn); err != nil {
		t.Fatal(err)
	}

	turns, err := repo.ListTurns(ctx, identityA, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1", len(turns))
	}
	got := turns[0]
	if got.LearnerText != turn.LearnerText || got.Reply != turn.Reply || got.ReplyEN != turn.ReplyEN || got.Followup != turn.Followup {
		t.Fatalf("turn = %+v, want %+v", got, turn)
	}
	if got.SessionID != sessA {
		t.Fatalf("SessionID = %q, want %q (populated via ListConversationTurns' join back to conversations)", got.SessionID, sessA)
	}
	if len(got.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(got.Corrections), got.Corrections)
	}
	if got.Corrections[0].Original != "面白いでした" || got.Corrections[0].Replacement != "面白かったです" {
		t.Fatalf("Corrections[0] = %+v, want the round-tripped i-adjective-past correction", got.Corrections[0])
	}
	if len(got.Corrections[0].Concepts) != 1 || got.Corrections[0].Concepts[0] != "i-adjective-past" {
		t.Fatalf("Corrections[0].Concepts = %+v, want [i-adjective-past]", got.Corrections[0].Concepts)
	}
}

// TestConversationDuplicatePositionRejected pins Finding I-5's 00022
// migration (UNIQUE(conversation_id, position)): the window between
// ListTurns and InsertTurn in application/conversation.Service.Say
// spans a full multi-second AI call, so two Say calls for the same
// conversation (e.g. the learner sending a second message before the
// first reply returns — the chat input isn't otherwise disabled, or
// wasn't before this fix) can both compute the same position. Without
// this constraint, both inserts would silently succeed, corrupting
// transcript order and "delayed" timing's position%3 batch boundary.
func TestConversationDuplicatePositionRejected(t *testing.T) {
	repo, identityA, sessA, _, _ := conversationTestSetup(t)
	ctx := context.Background()

	convID, _, err := repo.GetOrCreateForSession(ctx, identityA, sessA)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	first := storage.ConversationTurn{ID: uuid.NewString(), Position: 1, LearnerText: "一つ目", Reply: "了解1", CreatedAt: now}
	if err := repo.InsertTurn(ctx, identityA, convID, first); err != nil {
		t.Fatalf("first insert at position 1: %v", err)
	}

	second := storage.ConversationTurn{ID: uuid.NewString(), Position: 1, LearnerText: "二つ目（レース）", Reply: "了解2", CreatedAt: now}
	if err := repo.InsertTurn(ctx, identityA, convID, second); err == nil {
		t.Fatal("second insert at the SAME (conversation_id, position) unexpectedly succeeded — the race this migration closes is still open")
	}
}

// TestConversationCrossIdentityMisses pins the identity-scoping
// contract every repository in this package shares: identity B can
// neither read nor write against identity A's conversation — both come
// back storage.ErrNotFound, never identity A's data.
func TestConversationCrossIdentityMisses(t *testing.T) {
	repo, identityA, sessA, identityB, _ := conversationTestSetup(t)
	ctx := context.Background()

	convID, _, err := repo.GetOrCreateForSession(ctx, identityA, sessA)
	if err != nil {
		t.Fatal(err)
	}

	// B cannot insert a turn into A's conversation ID.
	turn := storage.ConversationTurn{ID: uuid.NewString(), Position: 1, LearnerText: "hi", Reply: "hi", CreatedAt: time.Now().UTC()}
	if err := repo.InsertTurn(ctx, identityB, convID, turn); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("InsertTurn(identityB, A's conversation) error = %v, want storage.ErrNotFound", err)
	}

	// A's turn list stays empty for B.
	turns, err := repo.ListTurns(ctx, identityB, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("ListTurns(identityB, A's conversation) returned %d turns, want 0", len(turns))
	}

	// A can insert into (and read back from) their own conversation.
	if err := repo.InsertTurn(ctx, identityA, convID, turn); err != nil {
		t.Fatal(err)
	}
	turnsA, err := repo.ListTurns(ctx, identityA, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turnsA) != 1 {
		t.Fatalf("ListTurns(identityA, A's conversation) returned %d turns, want 1", len(turnsA))
	}
}
