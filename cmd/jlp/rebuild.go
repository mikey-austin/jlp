package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	"github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/config"
)

// runRebuildModel is `jlp rebuild-model` (wrapped by `make rebuild-model`):
// PRD §14's rebuildability guarantee made runnable — it recomputes every
// identity's learner_observations from scratch by replaying learning_events
// through learnermodel.Rebuild, the exact same detection logic the live
// bus consumer (wired in main's "serve" case) uses. Safe to rerun any
// time: DeleteAll then replay always lands on the same state the event
// stream implies, regardless of whatever ad-hoc observations existed
// before.
func runRebuildModel(ctx context.Context, cfg config.Config) error {
	pool, err := postgres.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("rebuild-model: connect: %w", err)
	}
	defer pool.Close()

	identityRepo := postgres.NewIdentityRepository(pool)
	eventRepo := postgres.NewLearningEventRepository(pool)
	obsRepo := postgres.NewObservationRepository(pool)

	identities, err := identityRepo.ListIdentities(ctx)
	if err != nil {
		return fmt.Errorf("rebuild-model: list identities: %w", err)
	}

	for _, id := range identities {
		if err := learnermodel.Rebuild(ctx, id.ID, eventRepo, obsRepo, time.Now); err != nil {
			return fmt.Errorf("rebuild-model: identity %s: %w", id.ID, err)
		}
		slog.Info("rebuild-model: identity rebuilt", "identity", id.ID)
	}
	return nil
}
