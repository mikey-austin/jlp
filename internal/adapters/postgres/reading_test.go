//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func readingTestSetup(t *testing.T) (*ReadingRepository, learner.IdentityID, learner.IdentityID) {
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
	ids := NewIdentityRepository(pool)
	a := learner.Identity{ID: learner.IdentityID("test-reading-" + uuid.NewString()), DisplayName: "R"}
	b := learner.Identity{ID: learner.IdentityID("test-reading-" + uuid.NewString()), DisplayName: "S"}
	for _, i := range []learner.Identity{a, b} {
		if err := ids.Upsert(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	// These tests share the dev database with a running `jlp serve`,
	// whose worker would otherwise pick up a leftover pending edition or
	// delivery and act on it — mail included. Registered after pool.Close,
	// so it runs first.
	t.Cleanup(func() {
		for _, table := range []string{"reading_deliveries", "reading_editions", "reading_articles"} {
			if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE identity_id = ANY($1)", []string{string(a.ID), string(b.ID)}); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})
	return NewReadingRepository(pool), a.ID, b.ID
}

func testArticle(t *testing.T, identity learner.IdentityID, body string, now time.Time) reading.Article {
	t.Helper()
	pub := now.Add(-24 * time.Hour)
	a, err := reading.NewArticle(uuid.NewString(), identity, reading.Draft{
		SourceURL: "https://jp.wsj.com/articles/x", SourceName: "WSJ日本版", Title: "経済対策", Author: "記者", PublishedAt: &pub, Content: body,
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testEdition(identity learner.IdentityID, articleID string, now time.Time, deliver bool) reading.StudyEdition {
	return reading.StudyEdition{
		ID: uuid.NewString(), ArticleID: articleID, IdentityID: identity, Status: reading.EditionPending,
		PromptName: "reading.analyse", PromptVersion: "v1", SchemaName: "study_edition.v1",
		DeliverWhenReady: deliver, CreatedAt: now, UpdatedAt: now,
	}
}

// Claims are global (the worker serves every identity), so each test
// claims only work whose due time is its own "now" — far in the past
// relative to anything another test (or another run) left behind is
// impossible to guarantee, so tests assert on the rows they created.
func claimMine(t *testing.T, r *ReadingRepository, id string, now, stale time.Time) storage.ClaimedEdition {
	t.Helper()
	for i := 0; i < 50; i++ {
		c, ok, err := r.ClaimDueEdition(context.Background(), now, stale)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("edition %s was never claimed", id)
		}
		if c.ID == id {
			return c
		}
		// Someone else's leftover: fail it for good so it stops coming back.
		_ = r.FailEditionAttempt(context.Background(), c, "test cleanup", false, now, now)
	}
	t.Fatal("too many foreign claims")
	return storage.ClaimedEdition{}
}

func TestReadingArticleUpsertIsIdempotentAndScoped(t *testing.T) {
	r, me, other := readingTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	body := "政府が発表した新たな経済対策をめぐり、議論が続いている。" + uuid.NewString()

	a := testArticle(t, me, body, now)
	stored, created, err := r.UpsertArticle(ctx, a)
	if err != nil || !created || stored.ID != a.ID {
		t.Fatalf("first upsert: %v %v %+v", created, err, stored)
	}
	if stored.PublishedAt == nil || stored.Author != "記者" || len(stored.Paragraphs) != 1 {
		t.Fatalf("round trip lost fields: %+v", stored)
	}
	dup := testArticle(t, me, body, now)
	stored2, created2, err := r.UpsertArticle(ctx, dup)
	if err != nil || created2 || stored2.ID != a.ID {
		t.Fatalf("duplicate upsert: created=%v id=%s err=%v", created2, stored2.ID, err)
	}
	// Another identity's identical text is its own article.
	theirs, createdTheirs, err := r.UpsertArticle(ctx, testArticle(t, other, body, now))
	if err != nil || !createdTheirs || theirs.ID == a.ID {
		t.Fatalf("other identity upsert: %v %v", createdTheirs, err)
	}
	if _, err := r.GetArticle(ctx, other, a.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity read: %v", err)
	}

	// Soft delete hides; re-submitting restores.
	if err := r.SoftDeleteArticle(ctx, me, a.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetArticle(ctx, me, a.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted article visible: %v", err)
	}
	if err := r.SoftDeleteArticle(ctx, other, a.ID, now); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity delete: %v", err)
	}
	if _, _, err := r.UpsertArticle(ctx, testArticle(t, me, body, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetArticle(ctx, me, a.ID); err != nil {
		t.Fatalf("resubmission did not restore: %v", err)
	}
}

func TestReadingEditionLifecycle(t *testing.T) {
	r, me, other := readingTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	a, _, err := r.UpsertArticle(ctx, testArticle(t, me, "中央銀行は金融引き締めを続けている。"+uuid.NewString(), now))
	if err != nil {
		t.Fatal(err)
	}
	e := testEdition(me, a.ID, now, true)
	if err := r.InsertEdition(ctx, e); err != nil {
		t.Fatal(err)
	}
	latest, err := r.LatestEdition(ctx, me, a.ID, "reading.analyse", "v1")
	if err != nil || latest.ID != e.ID || latest.Status != reading.EditionPending || !latest.DeliverWhenReady {
		t.Fatalf("latest: %+v %v", latest, err)
	}
	if _, err := r.GetEdition(ctx, other, e.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity edition: %v", err)
	}

	claimAt := now.Add(time.Second)
	c := claimMine(t, r, e.ID, claimAt, claimAt.Add(-10*time.Minute))
	if c.Attempts != 1 || c.Status != reading.EditionAnalysing {
		t.Fatalf("claim: %+v", c)
	}
	// Fail with retry: back to pending, not due until retryAt.
	retryAt := claimAt.Add(time.Minute)
	if err := r.FailEditionAttempt(ctx, c, "boom", true, retryAt, claimAt); err != nil {
		t.Fatal(err)
	}
	// A second report on the same (now released) claim is rejected.
	if err := r.FailEditionAttempt(ctx, c, "again", true, retryAt, claimAt); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("released claim accepted: %v", err)
	}
	got, _ := r.GetEdition(ctx, me, e.ID)
	if got.Status != reading.EditionPending || got.LastError != "boom" {
		t.Fatalf("after fail: %+v", got)
	}

	c2 := claimMine(t, r, e.ID, retryAt.Add(time.Second), retryAt.Add(-10*time.Minute))
	if c2.Attempts != 2 {
		t.Fatalf("attempts = %d", c2.Attempts)
	}
	lesson, _ := json.Marshal(reading.Lesson{SummaryEN: "s", Vocabulary: []reading.VocabularyItem{{Expression: "金融引き締め", Reading: "きんゆうひきしめ", MeaningEN: "monetary tightening"}}})
	if err := r.CompleteEdition(ctx, c2, lesson, "req-1", retryAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetEdition(ctx, me, e.ID)
	if got.Status != reading.EditionReady || got.Lesson == nil || got.Lesson.Vocabulary[0].Expression != "金融引き締め" || got.AIRequestID != "req-1" || got.LastError != "" {
		t.Fatalf("after complete: %+v", got)
	}

	list, err := r.ListEditions(ctx, me)
	if err != nil || len(list) != 1 || list[0].EditionID != e.ID || list[0].Status != reading.EditionReady || list[0].DeliveryStatus != "" {
		t.Fatalf("list: %+v %v", list, err)
	}

	// Deleting the article hides its editions.
	if err := r.SoftDeleteArticle(ctx, me, a.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetEdition(ctx, me, e.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("edition of deleted article: %v", err)
	}
	if list, _ := r.ListEditions(ctx, me); len(list) != 0 {
		t.Fatalf("list after delete: %+v", list)
	}
}

func TestReadingDeliveryInFlightUniqueness(t *testing.T) {
	r, me, _ := readingTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	a, _, _ := r.UpsertArticle(ctx, testArticle(t, me, "議論が続いている。"+uuid.NewString(), now))
	e := testEdition(me, a.ID, now, false)
	if err := r.InsertEdition(ctx, e); err != nil {
		t.Fatal(err)
	}
	mk := func() reading.Delivery {
		return reading.Delivery{ID: uuid.NewString(), EditionID: e.ID, IdentityID: me, Destination: "me@kindle.invalid", Status: reading.DeliveryPending, CreatedAt: now}
	}
	d1, created, err := r.QueueDelivery(ctx, mk())
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	d2, created, err := r.QueueDelivery(ctx, mk())
	if err != nil || created || d2.ID != d1.ID {
		t.Fatalf("in-flight duplicate queued: %+v %v %v", d2, created, err)
	}

	// Claim ours (skipping any leftovers), send it, then a resend is allowed.
	var c storage.ClaimedDelivery
	for i := 0; i < 50; i++ {
		got, ok, err := r.ClaimDueDelivery(ctx, now.Add(time.Second), now.Add(-time.Hour))
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if got.ID == d1.ID {
			c = got
			break
		}
		_ = r.FailDeliveryAttempt(ctx, got, "test cleanup", false, now)
	}
	if c.ID == "" || c.Status != reading.DeliverySending || c.Attempts != 1 {
		t.Fatalf("claim = %+v", c)
	}
	if err := r.MarkDeliverySent(ctx, c, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	dls, err := r.ListDeliveries(ctx, me, e.ID)
	if err != nil || len(dls) != 1 || dls[0].Status != reading.DeliverySent || dls[0].SentAt == nil {
		t.Fatalf("deliveries: %+v %v", dls, err)
	}
	resend := mk()
	resend.CreatedAt = now.Add(3 * time.Second)
	d3, created, err := r.QueueDelivery(ctx, resend)
	if err != nil || !created || d3.ID == d1.ID {
		t.Fatalf("resend after sent: %v %v", created, err)
	}
	list, _ := r.ListEditions(ctx, me)
	if len(list) != 1 || list[0].DeliveryStatus != reading.DeliveryPending {
		t.Fatalf("list delivery status: %+v", list)
	}
}
