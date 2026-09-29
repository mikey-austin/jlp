package reading

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	agentreading "github.com/mikeyaustin/jlp/internal/agent/reading"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/publishing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	// pollInterval is how often an idle worker looks for due work
	// (retries whose backoff has elapsed, stale claims). New work does
	// not wait for it: Submit and Deliver nudge the worker directly.
	pollInterval = 15 * time.Second
	// deliveryStaleAfter is how long a delivery can sit in "sending"
	// before another worker assumes its sender died and takes it over.
	// An SMTP send with a few-MB attachment is seconds.
	deliveryStaleAfter = 5 * time.Minute
	// errorLimit keeps a stored last_error readable on a page.
	errorLimit = 500
)

// Run processes the pipeline until ctx is done: every due edition and
// delivery, then sleeps until the next tick or a nudge. Safe to run in
// several replicas at once — claims are SKIP LOCKED.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		s.Drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// Drain processes work until none is due. Exposed for tests and for a
// one-shot run.
func (s *Service) Drain(ctx context.Context) {
	for ctx.Err() == nil {
		did, err := s.ProcessNext(ctx)
		if err != nil {
			slog.Error("reading: worker", "err", err)
			return
		}
		if !did {
			return
		}
	}
}

// ProcessNext does one unit of work — one analysis attempt, else one
// delivery attempt — and reports whether there was any. Errors returned
// here are the worker's own (the database is unreachable); an attempt
// that fails is recorded on its row and is not an error.
func (s *Service) ProcessNext(ctx context.Context) (did bool, err error) {
	defer func() {
		// A panic in a model adapter or the renderer must not kill the
		// worker goroutine — and with it the whole pipeline — for every
		// learner. The claim it held goes stale and is retried.
		if r := recover(); r != nil {
			slog.Error("reading: worker panicked", "panic", r, "stack", string(debug.Stack()))
			did, err = true, nil
		}
	}()
	now := s.now()
	if c, ok, err := s.d.Repo.ClaimDueEdition(ctx, now, now.Add(-(s.cfg.AnalysisTimeout + time.Minute))); err != nil {
		return false, fmt.Errorf("claim edition: %w", err)
	} else if ok {
		s.analyse(ctx, c)
		return true, nil
	}
	if c, ok, err := s.d.Repo.ClaimDueDelivery(ctx, now, now.Add(-deliveryStaleAfter)); err != nil {
		return false, fmt.Errorf("claim delivery: %w", err)
	} else if ok {
		s.deliver(ctx, c)
		return true, nil
	}
	return false, nil
}

func (s *Service) analyse(ctx context.Context, c storage.ClaimedEdition) {
	log := slog.With("edition", c.ID, "identity", c.IdentityID, "attempt", c.Attempts)
	fail := func(msg string, retry bool) {
		if retry && c.Attempts >= reading.MaxAnalysisAttempts {
			retry = false
		}
		now := s.now()
		if err := s.d.Repo.FailEditionAttempt(ctx, c, clipError(msg), retry, now.Add(reading.RetryDelay(c.Attempts)), now); err != nil {
			log.Error("reading: record failed analysis", "err", err)
			return
		}
		log.Warn("reading: analysis failed", "retry", retry, "err", msg)
	}

	// A claim past the attempt budget is a worker that kept dying on
	// this edition (a stale reclaim still counts an attempt): stop.
	if c.Attempts > reading.MaxAnalysisAttempts {
		fail("gave up: analysis did not finish after repeated attempts", false)
		return
	}
	a, err := s.d.Repo.GetArticle(ctx, c.IdentityID, c.ArticleID)
	if errors.Is(err, storage.ErrNotFound) {
		fail("the article was deleted", false)
		return
	}
	if err != nil {
		fail("could not load the article: "+err.Error(), true)
		return
	}

	actx, cancel := context.WithTimeout(ctx, s.cfg.AnalysisTimeout)
	defer cancel()
	published := ""
	if a.PublishedAt != nil {
		published = a.PublishedAt.Format("2006-01-02")
	}
	lesson, resp, err := s.d.Analyser.Analyse(actx, agentreading.Input{
		Identity:     c.IdentityID,
		Title:        a.Title,
		Source:       a.SourceName,
		Published:    published,
		Text:         a.Text(),
		LearnerLevel: s.cfg.LearnerLevel,
	})
	if err != nil {
		fail(err.Error(), true)
		return
	}
	raw, err := lessonJSON(lesson)
	if err != nil {
		fail("encode lesson: "+err.Error(), false)
		return
	}
	if err := s.d.Repo.CompleteEdition(ctx, c, raw, resp.RequestID, s.now()); err != nil {
		// ErrNotFound: our claim went stale and another worker took the
		// edition over; its outcome stands. Anything else: the row is
		// left analysing and will be reclaimed.
		log.Warn("reading: could not complete edition", "err", err)
		return
	}
	log.Info("reading: edition ready", "vocabulary", len(lesson.Vocabulary))

	if err := s.d.Recorder.Record(ctx, event.LearningEvent{
		IdentityID: c.IdentityID,
		Type:       event.TypeReadingEditionCreated,
		Subject:    c.ID,
		Evidence: map[string]any{
			"article_id": c.ArticleID,
			"vocabulary": len(lesson.Vocabulary),
			"grammar":    len(lesson.Grammar),
			"characters": a.Runes(),
		},
	}); err != nil {
		log.Error("record reading.edition.created", "err", err)
	}

	if c.DeliverWhenReady && s.DeliveryEnabled() {
		if _, err := s.queueDelivery(ctx, c.IdentityID, c.ID); err != nil {
			log.Error("reading: queue delivery after analysis", "err", err)
		}
	}
}

func (s *Service) deliver(ctx context.Context, c storage.ClaimedDelivery) {
	log := slog.With("delivery", c.ID, "edition", c.EditionID, "identity", c.IdentityID, "attempt", c.Attempts)
	fail := func(msg string, retry bool) {
		if retry && c.Attempts >= reading.MaxDeliveryAttempts {
			retry = false
		}
		if err := s.d.Repo.FailDeliveryAttempt(ctx, c, clipError(msg), retry, s.now().Add(reading.RetryDelay(c.Attempts))); err != nil {
			log.Error("reading: record failed delivery", "err", err)
			return
		}
		log.Warn("reading: delivery failed", "retry", retry, "err", msg)
	}
	if c.Attempts > reading.MaxDeliveryAttempts {
		fail("gave up: delivery did not finish after repeated attempts", false)
		return
	}
	if s.d.Deliverer == nil {
		fail(ErrDeliveryNotConfigured.Error(), false)
		return
	}
	e, err := s.d.Repo.GetEdition(ctx, c.IdentityID, c.EditionID)
	if errors.Is(err, storage.ErrNotFound) {
		fail("the article was deleted", false)
		return
	}
	if err != nil {
		fail("could not load the edition: "+err.Error(), true)
		return
	}
	a, err := s.d.Repo.GetArticle(ctx, c.IdentityID, e.ArticleID)
	if err != nil {
		fail("could not load the article: "+err.Error(), !errors.Is(err, storage.ErrNotFound))
		return
	}
	book, err := s.render(ctx, a, e)
	if err != nil {
		fail(err.Error(), false)
		return
	}
	err = s.d.Deliverer.Deliver(ctx, publishing.Parcel{
		To:        c.Destination,
		Subject:   a.Title,
		Filename:  book.Filename,
		MediaType: book.MediaType,
		Data:      book.Data,
	})
	if err != nil {
		fail(err.Error(), !errors.Is(err, publishing.ErrPermanent))
		return
	}
	if err := s.d.Repo.MarkDeliverySent(ctx, c, s.now()); err != nil {
		// The mail went out; only the bookkeeping failed. Not retried —
		// that would send it again.
		log.Error("reading: delivery sent but not recorded", "err", err)
		return
	}
	log.Info("reading: delivered", "bytes", len(book.Data))
	if err := s.d.Recorder.Record(ctx, event.LearningEvent{
		IdentityID: c.IdentityID,
		Type:       event.TypeReadingEditionDelivered,
		Subject:    c.EditionID,
		Evidence:   map[string]any{"delivery_id": c.ID, "article_id": e.ArticleID},
	}); err != nil {
		log.Error("record reading.edition.delivered", "err", err)
	}
}

func clipError(msg string) string {
	if r := []rune(msg); len(r) > errorLimit {
		return string(r[:errorLimit]) + "…"
	}
	return msg
}
