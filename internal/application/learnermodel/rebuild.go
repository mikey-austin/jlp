package learnermodel

import (
	"context"
	"fmt"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/planner"
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
//
// The replay's own Updater is deliberately built WITHOUT a planner
// wired (unlike main.go's live bus consumer) — recomputing the whole
// priority list after every one of potentially thousands of historical
// events would be pure waste. Instead, if p is non-nil, Recompute runs
// exactly once, after the replay finishes, over the freshly rebuilt
// observations — the same "final state, not history" contract Rebuild
// itself gives observations. Pass nil when the caller doesn't need
// priorities recomputed (e.g. a test focused only on observation
// determinism).
func Rebuild(ctx context.Context, identity learner.IdentityID, events storage.LearningEventRepository, obs storage.ObservationRepository, clock func() time.Time, p *planner.Planner) error {
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
	if p != nil {
		if err := p.Recompute(ctx, identity); err != nil {
			return fmt.Errorf("learnermodel: rebuild: recompute priorities: %w", err)
		}
	}
	return nil
}
