package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

const (
	// seedSessionTitle matches the sessions list/workspace UI exactly
	// (Japanese, no romanization) so a fresh clone looks the same as any
	// learner-created session — nothing marks it as synthetic.
	seedSessionTitle    = "旅行について書く"
	seedSessionPurpose  = "Blog post"
	seedDocumentContent = "昨日友達と映画を見に行って、とても面白いでした。京都はとてもきれいでした。"
)

// runSeed populates a fresh dev database with one identity, one session,
// and that session's document: enough to click through the dashboard,
// open a workspace with real Japanese already in the editor, and run the
// Task 13 feedback flow without hand-typing anything first.
//
// It goes through the same application services production wiring uses —
// sessions.Service and appwriting.Service, not raw SQL — so the
// writing.created/writing.updated learning events those services record
// fire exactly as they would for a real learner: the dashboard's stat
// tiles and activity feed reflect the seed the same way they'd reflect
// any other session.
//
// Idempotent by design, safe to rerun (`make seed` again after `make
// migrate`, or a second time in the same session):
//   - identities.Upsert is already idempotent — it just refreshes the
//     dev/Dev Learner row.
//   - the session is created only when no session titled
//     seedSessionTitle already exists for this identity; an existing one
//     is reused as-is (title, purpose, and profile are left untouched).
//   - the document's content is set only when it is still empty, so a
//     learner who has started editing the seeded session never has their
//     text clobbered by a later `make seed`.
func runSeed(ctx context.Context, cfg config.Config) error {
	pool, err := postgres.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("seed: connect: %w", err)
	}
	defer pool.Close()

	// The identity to seed is the one static auth will actually present
	// in dev (cfg.Auth.Static.*, default dev/Dev Learner) rather than a
	// hardcoded literal, so `make seed` always matches whoever `make up`
	// logs the browser in as, even if that default is ever overridden.
	identity := learner.Identity{
		ID:          learner.IdentityID(cfg.Auth.Static.ID),
		DisplayName: cfg.Auth.Static.DisplayName,
	}
	identities := postgres.NewIdentityRepository(pool)
	if err := identities.Upsert(ctx, identity); err != nil {
		return fmt.Errorf("seed: upsert identity: %w", err)
	}
	slog.Info("seed: identity ready", "id", identity.ID, "display_name", identity.DisplayName)

	sessionsSvc := sessions.NewService(postgres.NewSessionRepository(pool))
	existing, err := sessionsSvc.List(ctx, identity.ID)
	if err != nil {
		return fmt.Errorf("seed: list sessions: %w", err)
	}
	var sess session.Session
	var found bool
	for _, s := range existing {
		if s.Title == seedSessionTitle {
			sess, found = s, true
			break
		}
	}
	if found {
		slog.Info("seed: session already exists, skipping", "id", sess.ID, "title", sess.Title)
	} else {
		sess, err = sessionsSvc.Create(ctx, identity.ID, seedSessionTitle, seedSessionPurpose, session.Profile{})
		if err != nil {
			return fmt.Errorf("seed: create session: %w", err)
		}
		slog.Info("seed: session created", "id", sess.ID, "title", sess.Title)
	}

	eventRepo := postgres.NewLearningEventRepository(pool)
	recorder := learning.NewRecorder(eventRepo, inprocbus.New())
	writingSvc := appwriting.NewService(postgres.NewDocumentRepository(pool), recorder)

	doc, err := writingSvc.Open(ctx, identity.ID, sess.ID)
	if err != nil {
		return fmt.Errorf("seed: open document: %w", err)
	}
	if doc.Content != "" {
		slog.Info("seed: document already has content, leaving it alone", "id", doc.ID, "runes", doc.RuneCount())
		return nil
	}
	doc, err = writingSvc.Autosave(ctx, identity.ID, doc.ID, seedDocumentContent)
	if err != nil {
		return fmt.Errorf("seed: set document content: %w", err)
	}
	slog.Info("seed: document content set", "id", doc.ID, "runes", doc.RuneCount())
	return nil
}
