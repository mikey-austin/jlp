package httpx

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// Generating the NEXT drill while the learner is still answering this
// one.
//
// A word drill is instant — it is built from stored data — while a
// grammar drill is a model call that takes anywhere from a few seconds
// to a couple of minutes behind a slow provider. In a run those
// alternate, so the learner gets two instant questions and then waits,
// which reads as the page having hung on question three even when it is
// working exactly as designed.
//
// Nothing about that wait is necessary: by the time question three is
// asked for, the answer to question two has already been submitted, and
// the drill could have been generated during either. So it is: serving
// a drill starts building the next one in the background, and the next
// request usually finds it already done.
//
// Deliberately NOT a queue. One drill ahead, per learner, discarded
// whenever it does not match what was actually asked for. A run is a
// single-threaded thing — one person answering one question at a time —
// and a deeper buffer would spend model calls on questions that may
// never be reached.

const (
	// prefetchTimeout bounds a background build. Generous, because the
	// whole point is to absorb a slow provider, and bounded because the
	// request context is gone: without this a wedged provider would hold
	// a goroutine and its model call indefinitely.
	prefetchTimeout = 3 * time.Minute

	// prefetchGrace is how long a request will wait for a prepared drill
	// that is still building before giving up on it.
	//
	// The wait exists because a build that is nearly done is worth a
	// moment; the BOUND exists because it is the learner sitting there.
	// Without one, a request inherits the whole prefetchTimeout — a
	// provider having a bad day turns into a page that appears to hang
	// for minutes, which is exactly what this file was meant to prevent
	// and briefly made worse instead.
	prefetchGrace = 4 * time.Second

	// prefetchMaxLearners caps how many learners may hold a prepared
	// drill at once, so this cannot grow without bound in a long-running
	// process. JLP is a single-learner deployment; the cap exists so that
	// stays true by construction rather than by assumption.
	prefetchMaxLearners = 32
)

// preparedDrill is one drill built ahead of being asked for.
type preparedDrill struct {
	// done closes when building finishes. Waiting on it is how a request
	// that arrives mid-build shares the work rather than starting a
	// second identical one.
	done chan struct{}
	// position/opts are what this drill was built FOR. A request for a
	// different SLOT cannot use it — a concept slot and a word slot are
	// different questions.
	position int
	opts     practice.StartOptions

	ex  exercise.Exercise
	err error
}

// matchesSlot reports whether this prepared drill answers the question
// being asked for.
//
// Only the position and the adapter define the slot. Everything else in
// StartOptions is an INPUT to selection, known to the request that
// started the prefetch and not to the one collecting it:
//
//   - ExcludeSubject is "not the card you just saw".
//   - PassageSubjects is what this run has covered.
//   - Want is a random draw from the allowed mix, so the two requests
//     would essentially never agree on it.
//
// Comparing any of them would make every prefetch miss, silently turning
// this whole file into dead weight while looking like it worked. What
// the collector cares about is answered instead by allows() below: not
// "was this the drill I would have chosen" but "is this a drill I am
// willing to show".
func (d *preparedDrill) matchesSlot(position int, opts practice.StartOptions) bool {
	return d.position == position && d.opts.ProviderOverride == opts.ProviderOverride
}

// allows reports whether a prepared exercise is one of the kinds the
// learner currently has selected.
//
// Checked on the built exercise rather than on the request that asked
// for it, because a prefetch chooses a kind and the build may deliver a
// different one — every kind is a preference, and an unfillable slot
// falls through. It also catches the learner unticking a box mid-run:
// the drill already prepared under the old selection is dropped rather
// than shown.
func allows(allowed []practice.Kind, ex exercise.Exercise) bool {
	if len(allowed) == 0 {
		return true
	}
	return slices.Contains(allowed, kindOf(ex))
}

// kindOf maps a built exercise back to the kind that would have asked
// for it.
func kindOf(ex exercise.Exercise) practice.Kind {
	switch ex.Type {
	case exercise.TypeWordRecall, exercise.TypeWordCloze:
		return practice.KindWord
	case exercise.TypePassageChoice:
		return practice.KindPassage
	default:
		return practice.KindGrammar
	}
}

// drillPrefetcher holds at most one prepared drill per learner.
type drillPrefetcher struct {
	mu      sync.Mutex
	pending map[learner.IdentityID]*preparedDrill
}

func newDrillPrefetcher() *drillPrefetcher {
	return &drillPrefetcher{pending: make(map[learner.IdentityID]*preparedDrill)}
}

// take returns the prepared drill for (identity, position, opts) if
// there is one, removing it either way: a prepared drill is used once,
// and one that does not match is stale and must not linger.
//
// It blocks until the build finishes when one is in flight, which is the
// case this exists for — the learner arriving while the model is still
// answering. That wait is bounded by prefetchTimeout inside the build.
func (p *drillPrefetcher) take(identity learner.IdentityID, position int, opts practice.StartOptions, allowed []practice.Kind) (exercise.Exercise, bool) {
	p.mu.Lock()
	prepared, ok := p.pending[identity]
	if ok {
		delete(p.pending, identity)
	}
	p.mu.Unlock()

	if !ok || !prepared.matchesSlot(position, opts) {
		return exercise.Exercise{}, false
	}
	select {
	case <-prepared.done:
	case <-time.After(prefetchGrace):
		// Still building. Reported as a miss so the caller serves
		// something else NOW; the goroutine finishes on its own and its
		// result is dropped, which costs one model call and saves the
		// learner from staring at a spinner.
		return exercise.Exercise{}, false
	}
	if prepared.err == nil && !allows(allowed, prepared.ex) {
		// Prepared under a selection the learner has since changed.
		return exercise.Exercise{}, false
	}
	if prepared.err != nil {
		// The background build failed. Reported as "nothing prepared" so
		// the caller builds synchronously and the learner gets a real
		// error from a real attempt rather than one from a request they
		// never made.
		return exercise.Exercise{}, false
	}
	return prepared.ex, true
}

// start begins building the drill for (position, opts) in the
// background, replacing whatever was prepared before.
//
// ctx is used only for its values; the build outlives the request that
// triggered it by construction, which is the entire point.
func (p *drillPrefetcher) start(ctx context.Context, svc drillBuilder, identity learner.IdentityID, position int, opts practice.StartOptions) {
	prepared := &preparedDrill{done: make(chan struct{}), position: position, opts: opts}

	p.mu.Lock()
	if len(p.pending) >= prefetchMaxLearners {
		// Full, and this learner has nothing pending. Skipped rather than
		// evicting someone else's in-flight work: the cost is one
		// synchronous build, and the alternative is spending a model call
		// and then throwing it away.
		if _, already := p.pending[identity]; !already {
			p.mu.Unlock()
			return
		}
	}
	p.pending[identity] = prepared
	p.mu.Unlock()

	go func() {
		defer close(prepared.done)
		// WithoutCancel: the request that triggered this returns
		// immediately, and its context is cancelled the moment it does.
		buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prefetchTimeout)
		defer cancel()

		prepared.ex, prepared.err = svc.Build(buildCtx, identity, opts)
		if prepared.err != nil {
			// Logged, not surfaced: nobody asked for this drill yet, and
			// the learner will get a real error from their own request if
			// the problem persists.
			slog.Warn("practice: prefetch failed",
				"identity", identity, "position", position, "err", prepared.err)
		}
	}()
}

// drillBuilder is the one method the prefetcher needs from
// practice.Service — narrow so a test can supply it directly.
type drillBuilder interface {
	Build(ctx context.Context, identity learner.IdentityID, opts practice.StartOptions) (exercise.Exercise, error)
}
