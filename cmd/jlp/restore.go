package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	appsessions "github.com/mikeyaustin/jlp/internal/application/sessions"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// `jlp restore <session|word|lesson> <identity> <id>` is the durable
// half of soft delete's way back (Phase 4 Task D).
//
// The UI offers an undo affordance on the list immediately after a
// delete, which covers the "I clicked the wrong row" case — but only
// until the learner navigates away. This command is what covers the
// case that actually needs covering: realising a week later that
// something is missing. Nothing was erased, so it is always still
// there to bring back.
//
// Identity is an argument here rather than something taken from a
// request context, because there is no request: this is an operator
// running a command on the box with the database password already in
// hand. It is not a hole in the HTTP layer's authorization — the
// repository still scopes by the identity given, so a wrong identity
// simply misses (ErrNotFound), exactly as it does over HTTP.
func runRestore(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: jlp restore <session|word|lesson> <identity> <id>")
	}
	kind, identity, id := args[0], learner.IdentityID(args[1]), args[2]

	pool, err := postgres.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("restore: database: %w", err)
	}
	defer pool.Close()

	// The restore is recorded as a content.restored learning event, the
	// mirror of the content.deleted a delete records — so the audit
	// trail reads the same whether the learner used the undo button or
	// an operator used this command.
	rec := learning.NewRecorder(postgres.NewLearningEventRepository(pool), inprocbus.New())

	// Restoring goes through the SAME application services the HTTP
	// undo button calls, not straight to the repositories: the
	// identity-scoping, the idempotence and the content.restored event
	// are defined once, in those services, and this command must not
	// re-derive any of them.
	//
	// applessons.NewService takes the whole lesson-generation pipeline,
	// none of which Restore touches (it uses only the repository and
	// the recorder — see its doc comment). Passing nil for the
	// generation dependencies is deliberate and safe here, and keeps
	// this command on the one code path rather than opening a second.
	switch kind {
	case "session":
		err = appsessions.NewService(postgres.NewSessionRepository(pool), rec).Restore(ctx, identity, session.ID(id))
	case "word", "vocabulary":
		err = appvocabulary.NewService(postgres.NewVocabularyRepository(pool), rec).Restore(ctx, identity, id)
	case "lesson":
		err = applessons.NewService(postgres.NewLessonRepository(pool), nil, nil, nil, nil, nil, rec).Restore(ctx, identity, id)
	default:
		return fmt.Errorf("restore: unknown kind %q (want session, word or lesson)", kind)
	}

	if errors.Is(err, storage.ErrNotFound) {
		// Deliberately the same message for "no such id" and "that id
		// belongs to someone else", matching the HTTP layer: there is no
		// reason for this command to be a better existence oracle than
		// the app is.
		return fmt.Errorf("restore: no %s %q for identity %q", kind, id, identity)
	}
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}
