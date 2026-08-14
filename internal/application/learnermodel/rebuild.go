package learnermodel

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Rebuild reconstructs identity's whole learner model from scratch:
// DeleteAll, then replay every one of identity's learning events,
// oldest first, through a fresh Updater's HandleEvent — the exact same
// detection logic live processing uses. This is PRD §14's
// rebuildability guarantee: the observations Rebuild produces are
// identical (modulo ID/FirstSeen/UpdatedAt bookkeeping) to what live
// processing would have produced over the same event stream, because
// HandleEvent's trailing-window math is anchored to each event's own
// OccurredAt rather than wall-clock time — see NewUpdater's doc
// comment. `jlp rebuild-model` calls this once per row in `identities`.
func Rebuild(ctx context.Context, identity learner.IdentityID, events storage.LearningEventRepository, obs storage.ObservationRepository, clock func() time.Time) error {
	if err := obs.DeleteAll(ctx, identity); err != nil {
		return err
	}
	all, err := events.ListAll(ctx, identity)
	if err != nil {
		return err
	}
	updater := NewUpdater(events, obs, clock)
	for _, ev := range all {
		if err := updater.HandleEvent(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}
