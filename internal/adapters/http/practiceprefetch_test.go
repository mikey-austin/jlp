package httpx

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// slowBuilder blocks until released, so a test can observe what happens
// while a build is still running — which is the whole situation the
// prefetcher exists for.
type slowBuilder struct {
	calls   atomic.Int32
	release chan struct{}
	err     error
	made    exercise.Exercise
}

func (b *slowBuilder) Build(ctx context.Context, _ learner.IdentityID, _ practice.StartOptions) (exercise.Exercise, error) {
	b.calls.Add(1)
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return exercise.Exercise{}, ctx.Err()
		}
	}
	if b.err != nil {
		return exercise.Exercise{}, b.err
	}
	return b.made, nil
}

const prefetchIdentity = learner.IdentityID("learner-a")

// opts is one slot's options. The KIND is irrelevant to these tests —
// the prefetcher deliberately does not treat it as part of the slot's
// identity (see matchesSlot) — so they pin position and adapter only.
func opts(position int) practice.StartOptions {
	return practice.StartOptions{}
}

// The point of the whole file: a drill asked for after being prepared
// comes back without building again.
func TestAPreparedDrillIsServedWithoutRebuilding(t *testing.T) {
	b := &slowBuilder{release: make(chan struct{}), made: exercise.Exercise{Type: exercise.TypeFillInBlank, Prompt: "prepared"}}
	close(b.release)
	p := newDrillPrefetcher()

	p.start(context.Background(), b, prefetchIdentity, 3, opts(3))
	ex, ok := p.take(prefetchIdentity, 3, opts(3), nil)

	if !ok {
		t.Fatal("a drill prepared for position 3 was not served for position 3")
	}
	if ex.Prompt != "prepared" {
		t.Errorf("prompt = %q, want the prepared drill", ex.Prompt)
	}
	if got := b.calls.Load(); got != 1 {
		t.Errorf("built %d times, want 1 — the prepared drill was rebuilt", got)
	}
}

// A drill prepared for one slot must never be served for another. The
// slots differ in what they ask for — position 3 skips words, position 5
// wants a passage — so handing over the wrong one silently changes the
// question the learner was supposed to get.
func TestAPreparedDrillIsNotServedForADifferentPosition(t *testing.T) {
	b := &slowBuilder{made: exercise.Exercise{Prompt: "for position 3"}}
	p := newDrillPrefetcher()

	p.start(context.Background(), b, prefetchIdentity, 3, opts(3))
	if _, ok := p.take(prefetchIdentity, 4, opts(4), nil); ok {
		t.Fatal("a drill prepared for position 3 was served for position 4")
	}
	// And it is gone: a stale prepared drill must not linger to be
	// mistakenly matched later.
	if _, ok := p.take(prefetchIdentity, 3, opts(3), nil); ok {
		t.Error("a rejected prepared drill was still held")
	}
}

// One learner's prepared drill must never reach another. Same-position
// requests from two identities would otherwise collide.
func TestAPreparedDrillIsNotSharedBetweenLearners(t *testing.T) {
	b := &slowBuilder{made: exercise.Exercise{Prompt: "learner-a's"}}
	p := newDrillPrefetcher()

	p.start(context.Background(), b, prefetchIdentity, 3, opts(3))
	if _, ok := p.take(learner.IdentityID("learner-b"), 3, opts(3), nil); ok {
		t.Fatal("one learner was served another's prepared drill")
	}
}

// Arriving mid-build must WAIT for the running build rather than start a
// second one — otherwise a slow provider gets asked the same question
// twice and the learner waits just as long as before.
func TestARequestArrivingMidBuildWaitsRatherThanRebuilding(t *testing.T) {
	b := &slowBuilder{release: make(chan struct{}), made: exercise.Exercise{Prompt: "eventually"}}
	p := newDrillPrefetcher()
	p.start(context.Background(), b, prefetchIdentity, 3, opts(3))

	done := make(chan exercise.Exercise, 1)
	go func() {
		ex, _ := p.take(prefetchIdentity, 3, opts(3), nil)
		done <- ex
	}()

	select {
	case <-done:
		t.Fatal("take returned while the build was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(b.release)
	select {
	case ex := <-done:
		if ex.Prompt != "eventually" {
			t.Errorf("prompt = %q, want the prepared drill", ex.Prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("take never returned after the build finished")
	}
	if got := b.calls.Load(); got != 1 {
		t.Errorf("built %d times, want 1", got)
	}
}

// A failed background build must look like "nothing prepared", so the
// caller builds synchronously and the learner gets an error from their
// own request rather than from one they never made.
func TestAFailedPrefetchLooksLikeNothingPrepared(t *testing.T) {
	b := &slowBuilder{err: errors.New("provider exploded")}
	p := newDrillPrefetcher()

	p.start(context.Background(), b, prefetchIdentity, 3, opts(3))
	if _, ok := p.take(prefetchIdentity, 3, opts(3), nil); ok {
		t.Fatal("a failed prefetch was served as a drill")
	}
}

// The request that triggers a prefetch returns immediately, and its
// context is cancelled the moment it does. A build started from that
// context would die instantly and every prefetch would miss.
func TestAPrefetchSurvivesTheRequestThatStartedIt(t *testing.T) {
	b := &slowBuilder{release: make(chan struct{}), made: exercise.Exercise{Prompt: "survived"}}
	p := newDrillPrefetcher()

	ctx, cancel := context.WithCancel(context.Background())
	p.start(ctx, b, prefetchIdentity, 3, opts(3))
	cancel() // exactly what the HTTP server does once the response is written
	close(b.release)

	ex, ok := p.take(prefetchIdentity, 3, opts(3), nil)
	if !ok {
		t.Fatal("the prefetch died with the request that started it")
	}
	if ex.Prompt != "survived" {
		t.Errorf("prompt = %q", ex.Prompt)
	}
}

// ExcludeSubject is known only to the request that STARTS a prefetch,
// never to the one that collects it. Comparing it would make every
// prefetch miss — the whole file silently doing nothing while looking
// like it worked.
func TestAPreparedDrillIsServedEvenThoughItExcludedSomething(t *testing.T) {
	b := &slowBuilder{made: exercise.Exercise{Prompt: "prepared"}}
	p := newDrillPrefetcher()

	started := opts(3)
	started.ExcludeSubject = "the-card-just-shown"
	p.start(context.Background(), b, prefetchIdentity, 3, started)

	// The collecting request knows the slot, not what was excluded.
	if _, ok := p.take(prefetchIdentity, 3, opts(3), nil); !ok {
		t.Fatal("a prepared drill was rejected because it had excluded a subject")
	}
}

// A build that is still running must not hold the learner indefinitely:
// the whole point of the prefetch is that nobody waits on a model.
func TestARequestGivesUpOnASlowBuild(t *testing.T) {
	b := &slowBuilder{release: make(chan struct{})} // never released
	p := newDrillPrefetcher()
	p.start(context.Background(), b, prefetchIdentity, 3, opts(3))

	start := time.Now()
	_, ok := p.take(prefetchIdentity, 3, opts(3), nil)
	waited := time.Since(start)

	if ok {
		t.Fatal("take returned a drill that was never built")
	}
	if waited > prefetchGrace+2*time.Second {
		t.Errorf("waited %v for a stalled build, want to give up around %v", waited, prefetchGrace)
	}
	close(b.release)
}
