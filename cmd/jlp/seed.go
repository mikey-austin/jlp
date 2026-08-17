package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

const (
	// seedSessionTitle matches the sessions list/workspace UI exactly
	// (Japanese, no romanization) so a fresh clone looks the same as any
	// learner-created session — nothing marks it as synthetic.
	seedSessionTitle    = "旅行について書く"
	seedSessionPurpose  = "Blog post"
	seedDocumentContent = "昨日友達と映画を見に行って、とても面白いでした。京都はとてもきれいでした。"

	// seedGrammarCatalogPath is relative to the process's working
	// directory — the same convention internal/adapters/http/render.go
	// uses for web/templates: the repo root both in the container
	// (WORKDIR /src) and when run directly from a checkout.
	seedGrammarCatalogPath = "data/grammar/concepts.yaml"

	// seedExpressionBankPath is the same repo-root-relative convention
	// as seedGrammarCatalogPath, for Task 7's curated expression bank
	// (PRD §55, §17.5).
	seedExpressionBankPath = "data/expressions/core.yaml"
)

// runSeed populates a fresh dev database with the curated grammar
// concept catalog, one identity, one session, and that session's
// document: enough to click through the dashboard, open a workspace
// with real Japanese already in the editor, and run the Task 13
// feedback flow without hand-typing anything first.
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
//   - the grammar catalog is loaded from seedGrammarCatalogPath and
//     applied via GrammarRepository.UpsertConcepts, which is itself
//     idempotent (ON CONFLICT slug DO UPDATE) — rerunning just refreshes
//     every concept to match the file, never duplicates a row.
//   - the expression bank is loaded from seedExpressionBankPath and
//     applied via VocabularyRepository.SeedBank, which is
//     insert-if-absent (ON CONFLICT DO NOTHING) rather than
//     UpsertOnLookup's increment-on-conflict: a bank item's
//     lookups/productions counts are deliberately NEVER touched by a
//     re-seed, even after the learner has since looked one up or
//     produced it for real (see SeedBank's own doc comment).
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

	if err := seedGrammarCatalog(ctx, pool); err != nil {
		return err
	}

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

	if err := seedExpressionBank(ctx, pool, identity.ID); err != nil {
		return err
	}

	eventRepo := postgres.NewLearningEventRepository(pool)
	recorder := learning.NewRecorder(eventRepo, inprocbus.New())
	sessionsSvc := sessions.NewService(postgres.NewSessionRepository(pool), recorder)
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

// seedGrammarCatalog loads the curated JLPT catalog from
// seedGrammarCatalogPath and upserts it into grammar_concepts. Reference
// data, not identity-scoped, so — unlike the rest of runSeed — this has
// no per-learner state to check before writing: UpsertConcepts is
// idempotent on its own (ON CONFLICT slug DO UPDATE), so simply always
// applying the file is both correct and simplest.
func seedGrammarCatalog(ctx context.Context, pool *pgxpool.Pool) error {
	f, err := os.Open(seedGrammarCatalogPath)
	if err != nil {
		return fmt.Errorf("seed: open grammar catalog: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			slog.Error("seed: close grammar catalog file", "err", cerr)
		}
	}()

	concepts, err := grammar.LoadCatalog(f)
	if err != nil {
		return fmt.Errorf("seed: load grammar catalog: %w", err)
	}

	if err := postgres.NewGrammarRepository(pool).UpsertConcepts(ctx, concepts); err != nil {
		return fmt.Errorf("seed: upsert grammar concepts: %w", err)
	}
	slog.Info("seed: grammar catalog loaded", "concepts", len(concepts))
	return nil
}

// seedExpressionBank loads the curated expression bank (Task 7, PRD
// §55/§17.5) from seedExpressionBankPath and seeds it, whole, into
// identity's vocabulary as zero-count baseline items via
// VocabularyRepository.SeedBank — one call, one transaction (mirroring
// seedGrammarCatalog's UpsertConcepts call above it) — deliberately NOT
// UpsertOnLookup, which would increment Lookups on every rerun and
// eventually make every bank item look like a real, repeatedly-looked-up
// expression. SeedBank's insert-if-absent contract means this is safe
// to call unconditionally on every `jlp seed`, unlike the
// session/document seeding above.
func seedExpressionBank(ctx context.Context, pool *pgxpool.Pool, identity learner.IdentityID) error {
	f, err := os.Open(seedExpressionBankPath)
	if err != nil {
		return fmt.Errorf("seed: open expression bank: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			slog.Error("seed: close expression bank file", "err", cerr)
		}
	}()

	entries, err := vocabulary.LoadBank(f)
	if err != nil {
		return fmt.Errorf("seed: load expression bank: %w", err)
	}

	repo := postgres.NewVocabularyRepository(pool)
	if err := repo.SeedBank(ctx, identity, entries, time.Now().UTC()); err != nil {
		return fmt.Errorf("seed: seed expression bank: %w", err)
	}
	slog.Info("seed: expression bank loaded", "expressions", len(entries))
	return nil
}
