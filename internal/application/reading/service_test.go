package reading_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids application importing real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	agentreading "github.com/mikeyaustin/jlp/internal/agent/reading"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appreading "github.com/mikeyaustin/jlp/internal/application/reading"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/publishing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const me = learner.IdentityID("learner-a")

const article = "政府が発表した新たな経済対策をめぐり、議論が続いている。\n\n中央銀行は金融引き締めを続けている。"

// --- fakes ---------------------------------------------------------------

// memRepo is an in-memory storage.ReadingRepository with the SQL's
// semantics: content-hash idempotency, claim tokens, in-flight
// delivery uniqueness, soft delete hiding editions.
type memRepo struct {
	mu         sync.Mutex
	articles   map[string]*reading.Article
	deleted    map[string]bool
	figures    map[string][]reading.Figure
	editions   map[string]*editionRow
	deliveries map[string]*deliveryRow
	drafts     map[learner.IdentityID]*memDraft
	nextDraft  int
}

type editionRow struct {
	reading.StudyEdition
	next    time.Time
	claimed *time.Time
}

type deliveryRow struct {
	reading.Delivery
	next    time.Time
	claimed *time.Time
}

func newMemRepo() *memRepo {
	return &memRepo{articles: map[string]*reading.Article{}, deleted: map[string]bool{}, figures: map[string][]reading.Figure{}, editions: map[string]*editionRow{}, deliveries: map[string]*deliveryRow{}}
}

func (m *memRepo) UpsertArticle(_ context.Context, a reading.Article) (reading.Article, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.articles {
		if x.IdentityID == a.IdentityID && x.ContentHash == a.ContentHash {
			if !m.deleted[x.ID] {
				return *x, false, nil
			}
			// A deleted match is purged with everything hanging off it.
			for eid, e := range m.editions {
				if e.ArticleID != x.ID {
					continue
				}
				for did, d := range m.deliveries {
					if d.EditionID == eid {
						delete(m.deliveries, did)
					}
				}
				delete(m.editions, eid)
			}
			delete(m.figures, x.ID)
			delete(m.deleted, x.ID)
			delete(m.articles, x.ID)
			break
		}
	}
	c := a
	m.articles[a.ID] = &c
	return a, true, nil
}

func (m *memRepo) GetArticle(_ context.Context, id learner.IdentityID, aid string) (reading.Article, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] {
		return reading.Article{}, storage.ErrNotFound
	}
	return *a, nil
}

func (m *memRepo) SaveTranslation(_ context.Context, id string, t reading.Article) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[id]
	if !ok {
		return storage.ErrNotFound
	}
	a.Title, a.Paragraphs = t.Title, append([]string(nil), t.Paragraphs...)
	a.OriginalLanguage, a.OriginalTitle, a.OriginalParagraphs = t.OriginalLanguage, t.OriginalTitle, append([]string(nil), t.OriginalParagraphs...)
	return nil
}

func (m *memRepo) SoftDeleteArticle(_ context.Context, id learner.IdentityID, aid string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id {
		return storage.ErrNotFound
	}
	m.deleted[aid] = true
	return nil
}

func (m *memRepo) RestoreArticle(_ context.Context, id learner.IdentityID, aid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id {
		return storage.ErrNotFound
	}
	delete(m.deleted, aid)
	return nil
}

func (m *memRepo) AttachFigures(_ context.Context, id learner.IdentityID, aid string, figs []reading.Figure) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] || len(m.figures[aid]) > 0 || len(figs) == 0 {
		return false, nil
	}
	m.figures[aid] = append([]reading.Figure(nil), figs...)
	return true, nil
}

func (m *memRepo) ListFigures(_ context.Context, id learner.IdentityID, aid string) ([]reading.Figure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] {
		return nil, nil
	}
	out := make([]reading.Figure, 0, len(m.figures[aid]))
	for _, f := range m.figures[aid] {
		f.Data = nil
		out = append(out, f)
	}
	return out, nil
}

func (m *memRepo) FigureData(_ context.Context, id learner.IdentityID, aid string, ordinal int) (reading.Figure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] {
		return reading.Figure{}, storage.ErrNotFound
	}
	for _, f := range m.figures[aid] {
		if f.Ordinal == ordinal {
			return f, nil
		}
	}
	return reading.Figure{}, storage.ErrNotFound
}

func (m *memRepo) InsertEdition(_ context.Context, e reading.StudyEdition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.editions[e.ID] = &editionRow{StudyEdition: e, next: e.CreatedAt}
	return nil
}

func (m *memRepo) sortedEditions() []*editionRow {
	var out []*editionRow
	for _, e := range m.editions {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt) || (out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID)
	})
	return out
}

func (m *memRepo) LatestEdition(_ context.Context, id learner.IdentityID, aid, pn, pv string) (reading.StudyEdition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *editionRow
	for _, e := range m.sortedEditions() {
		if e.ArticleID == aid && e.IdentityID == id && e.PromptName == pn && e.PromptVersion == pv {
			best = e
		}
	}
	if best == nil {
		return reading.StudyEdition{}, storage.ErrNotFound
	}
	return best.StudyEdition, nil
}

func (m *memRepo) GetEdition(_ context.Context, id learner.IdentityID, eid string) (reading.StudyEdition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.editions[eid]
	if !ok || e.IdentityID != id || m.deleted[e.ArticleID] {
		return reading.StudyEdition{}, storage.ErrNotFound
	}
	return e.StudyEdition, nil
}

func (m *memRepo) ListEditions(_ context.Context, id learner.IdentityID) ([]storage.ReadingEditionSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	latest := map[string]*editionRow{}
	for _, e := range m.sortedEditions() {
		if e.IdentityID == id && !m.deleted[e.ArticleID] {
			latest[e.ArticleID] = e
		}
	}
	var out []storage.ReadingEditionSummary
	for aid, e := range latest {
		out = append(out, storage.ReadingEditionSummary{EditionID: e.ID, ArticleID: aid, Title: m.articles[aid].Title, Status: e.Status})
	}
	return out, nil
}

func (m *memRepo) ClaimDueEdition(_ context.Context, now, stale time.Time) (storage.ClaimedEdition, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.sortedEditions() {
		due := (e.Status == reading.EditionPending && !e.next.After(now)) ||
			(e.Status == reading.EditionAnalysing && e.claimed != nil && e.claimed.Before(stale))
		if !due {
			continue
		}
		t := now
		e.Status, e.claimed = reading.EditionAnalysing, &t
		e.Attempts++
		return storage.ClaimedEdition{StudyEdition: e.StudyEdition, ClaimedAt: t}, true, nil
	}
	return storage.ClaimedEdition{}, false, nil
}

func (m *memRepo) current(c storage.ClaimedEdition) (*editionRow, error) {
	e, ok := m.editions[c.ID]
	if !ok || e.Status != reading.EditionAnalysing || e.claimed == nil || !e.claimed.Equal(c.ClaimedAt) {
		return nil, storage.ErrNotFound
	}
	return e, nil
}

func (m *memRepo) CompleteEdition(_ context.Context, c storage.ClaimedEdition, lesson []byte, reqID string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.current(c)
	if err != nil {
		return err
	}
	var l reading.Lesson
	if err := jsonUnmarshal(lesson, &l); err != nil {
		return err
	}
	e.Status, e.Lesson, e.AIRequestID, e.claimed, e.UpdatedAt, e.LastError = reading.EditionReady, &l, reqID, nil, now, ""
	return nil
}

func (m *memRepo) FailEditionAttempt(_ context.Context, c storage.ClaimedEdition, msg string, retry bool, at, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.current(c)
	if err != nil {
		return err
	}
	e.Status = reading.EditionFailed
	if retry {
		e.Status = reading.EditionPending
	}
	e.LastError, e.next, e.claimed, e.UpdatedAt = msg, at, nil, now
	return nil
}

func (m *memRepo) QueueDelivery(_ context.Context, d reading.Delivery) (reading.Delivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.deliveries {
		if x.EditionID == d.EditionID && x.Destination == d.Destination && (x.Status == reading.DeliveryPending || x.Status == reading.DeliverySending) {
			return x.Delivery, false, nil
		}
	}
	m.deliveries[d.ID] = &deliveryRow{Delivery: d, next: d.CreatedAt}
	return d, true, nil
}

func (m *memRepo) ListDeliveries(_ context.Context, id learner.IdentityID, eid string) ([]reading.Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []reading.Delivery
	for _, d := range m.deliveries {
		if d.EditionID == eid && d.IdentityID == id {
			out = append(out, d.Delivery)
		}
	}
	return out, nil
}

func (m *memRepo) ClaimDueDelivery(_ context.Context, now, stale time.Time) (storage.ClaimedDelivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.deliveries {
		due := (d.Status == reading.DeliveryPending && !d.next.After(now)) ||
			(d.Status == reading.DeliverySending && d.claimed != nil && d.claimed.Before(stale))
		if !due {
			continue
		}
		t := now
		d.Status, d.claimed = reading.DeliverySending, &t
		d.Attempts++
		return storage.ClaimedDelivery{Delivery: d.Delivery, ClaimedAt: t}, true, nil
	}
	return storage.ClaimedDelivery{}, false, nil
}

func (m *memRepo) curDelivery(c storage.ClaimedDelivery) (*deliveryRow, error) {
	d, ok := m.deliveries[c.ID]
	if !ok || d.Status != reading.DeliverySending || d.claimed == nil || !d.claimed.Equal(c.ClaimedAt) {
		return nil, storage.ErrNotFound
	}
	return d, nil
}

func (m *memRepo) MarkDeliverySent(_ context.Context, c storage.ClaimedDelivery, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.curDelivery(c)
	if err != nil {
		return err
	}
	d.Status, d.SentAt, d.claimed = reading.DeliverySent, &at, nil
	return nil
}

func (m *memRepo) FailDeliveryAttempt(_ context.Context, c storage.ClaimedDelivery, msg string, retry bool, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.curDelivery(c)
	if err != nil {
		return err
	}
	d.Status = reading.DeliveryFailed
	if retry {
		d.Status = reading.DeliveryPending
	}
	d.LastError, d.next, d.claimed = msg, at, nil
	return nil
}

// failingAnalyser fails its first n calls, then delegates.
type failingAnalyser struct {
	n, calls int
	inner    appreading.Analyser
}

func (f *failingAnalyser) Analyse(ctx context.Context, in agentreading.Input) (reading.Lesson, ai.StructuredResponse, error) {
	f.calls++
	if f.calls <= f.n {
		return reading.Lesson{}, ai.StructuredResponse{}, errors.New("provider unavailable")
	}
	return f.inner.Analyse(ctx, in)
}

// countingTranslator wraps the real translation agent (over the fake
// provider) and counts calls; it fails its first n.
type countingTranslator struct {
	n, calls int
	inner    appreading.Translator
}

func (c *countingTranslator) Translate(ctx context.Context, id learner.IdentityID, a reading.Article) (reading.Translation, ai.StructuredResponse, error) {
	c.calls++
	if c.calls <= c.n {
		return reading.Translation{}, ai.StructuredResponse{}, errors.New("translator unavailable")
	}
	return c.inner.Translate(ctx, id, a)
}

// textRecordingAnalyser remembers what the analyser was given.
type textRecordingAnalyser struct {
	appreading.Analyser
	inputs []agentreading.Input
}

func (r *textRecordingAnalyser) Analyse(ctx context.Context, in agentreading.Input) (reading.Lesson, ai.StructuredResponse, error) {
	r.inputs = append(r.inputs, in)
	return r.Analyser.Analyse(ctx, in)
}

type fakeRenderer struct {
	calls int
	last  publishing.Ebook
}

func (r *fakeRenderer) Render(_ context.Context, b publishing.Ebook) ([]byte, error) {
	r.calls++
	r.last = b
	return []byte("EPUB:" + b.ID + ":" + b.Article.Title), nil
}
func (*fakeRenderer) MediaType() string { return "application/epub+zip" }
func (*fakeRenderer) Extension() string { return ".epub" }

type fakeDeliverer struct {
	fail    []error
	parcels []publishing.Parcel
}

func (d *fakeDeliverer) Deliver(_ context.Context, p publishing.Parcel) error {
	if len(d.fail) > 0 {
		err := d.fail[0]
		d.fail = d.fail[1:]
		return err
	}
	d.parcels = append(d.parcels, p)
	return nil
}

type fakeVocab struct {
	ingested map[string]appvocabulary.IngestEvent // by client_event_id
	known    []string
}

func (v *fakeVocab) Ingest(_ context.Context, _ learner.IdentityID, ev appvocabulary.IngestEvent) (vocabulary.Item, error) {
	if v.ingested == nil {
		v.ingested = map[string]appvocabulary.IngestEvent{}
	}
	v.ingested[ev.ClientEventID] = ev
	return vocabulary.Item{Expression: ev.Expression}, nil
}

func (v *fakeVocab) GetByExpressions(_ context.Context, _ learner.IdentityID, exprs []string) ([]vocabulary.Item, error) {
	var out []vocabulary.Item
	for _, e := range exprs {
		for _, k := range v.known {
			if e == k {
				out = append(out, vocabulary.Item{Expression: e})
			}
		}
	}
	return out, nil
}

type fakeAnki struct{ drafts []appanki.Draft }

func (a *fakeAnki) CreateDraft(_ context.Context, _ learner.IdentityID, d []appanki.Draft) (int, error) {
	a.drafts = append(a.drafts, d...)
	return len(d), nil
}

type eventStore struct{ appended []event.LearningEvent }

func (e *eventStore) Append(_ context.Context, ev event.LearningEvent) error {
	e.appended = append(e.appended, ev)
	return nil
}
func (*eventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return nil, nil
}
func (*eventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	return nil, nil
}

func (e *eventStore) count(t event.Type) int {
	n := 0
	for _, ev := range e.appended {
		if ev.Type == t {
			n++
		}
	}
	return n
}

// --- harness ---------------------------------------------------------------

type harness struct {
	svc      *appreading.Service
	repo     *memRepo
	analyser *failingAnalyser
	render   *fakeRenderer
	cover    *fakeCover
	deliver  *fakeDeliverer
	vocab    *fakeVocab
	anki     *fakeAnki
	events   *eventStore
	clock    *time.Time
}

func newHarness(t *testing.T, kindle bool) *harness {
	t.Helper()
	h := &harness{
		repo:     newMemRepo(),
		analyser: &failingAnalyser{inner: agentreading.New(fakeai.New())},
		render:   &fakeRenderer{},
		cover:    &fakeCover{},
		deliver:  &fakeDeliverer{},
		vocab:    &fakeVocab{},
		anki:     &fakeAnki{},
		events:   &eventStore{},
	}
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	h.clock = &now
	deps := appreading.Deps{
		Repo:       h.repo,
		Recorder:   learning.NewRecorder(h.events, inprocbus.New()),
		Analyser:   h.analyser,
		Renderer:   h.render,
		Cover:      h.cover,
		Vocabulary: h.vocab,
		Anki:       h.anki,
	}
	cfg := appreading.Config{}
	if kindle {
		deps.Deliverer = h.deliver
		cfg.KindleTo = "me@kindle.com"
	}
	h.svc = appreading.NewService(deps, cfg)
	appreading.SetClock(h.svc, func() time.Time { return *h.clock })
	return h
}

// withTranslator swaps in a counting translator and a recording analyser
// over the same fake provider the harness already uses.
func (h *harness) withTranslator(t *testing.T, failFirst int) (*countingTranslator, *textRecordingAnalyser) {
	t.Helper()
	tr := &countingTranslator{n: failFirst, inner: agentreading.NewTranslator(fakeai.New())}
	rec := &textRecordingAnalyser{Analyser: h.analyser}
	h.svc = appreading.NewService(appreading.Deps{
		Repo:       h.repo,
		Recorder:   learning.NewRecorder(h.events, inprocbus.New()),
		Analyser:   rec,
		Translator: tr,
		Renderer:   h.render,
		Cover:      h.cover,
		Vocabulary: h.vocab,
		Anki:       h.anki,
	}, appreading.Config{})
	appreading.SetClock(h.svc, func() time.Time { return *h.clock })
	return tr, rec
}

func (h *harness) submitEnglish(t *testing.T) appreading.SubmitResult {
	t.Helper()
	res, err := h.svc.Submit(context.Background(), me, reading.Draft{Title: "Central bank", Content: "The bank held rates.\n\nMarkets were calm."}, appreading.SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *harness) advance(d time.Duration) { *h.clock = h.clock.Add(d) }

func (h *harness) submit(t *testing.T, opts appreading.SubmitOptions) appreading.SubmitResult {
	t.Helper()
	res, err := h.svc.Submit(context.Background(), me, reading.Draft{Title: "経済対策", SourceName: "WSJ日本版", Content: article}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *harness) edition(t *testing.T, id string) reading.StudyEdition {
	t.Helper()
	e, err := h.repo.GetEdition(context.Background(), me, id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// --- tests ---------------------------------------------------------------

func TestSubmitQueuesThenWorkerAnalyses(t *testing.T) {
	h := newHarness(t, false)
	res := h.submit(t, appreading.SubmitOptions{})
	if res.Duplicate || res.Edition.Status != reading.EditionPending {
		t.Fatalf("submit = %+v", res)
	}
	if h.analyser.calls != 0 {
		t.Fatal("Submit must not call the model — that is the worker's job")
	}
	h.svc.Drain(context.Background())
	e := h.edition(t, res.Edition.ID)
	if e.Status != reading.EditionReady || e.Lesson == nil || len(e.Lesson.Vocabulary) == 0 {
		t.Fatalf("edition = %+v", e)
	}
	if h.events.count(event.TypeReadingEditionCreated) != 1 {
		t.Fatal("reading.edition.created not recorded")
	}
}

func TestSubmitIsIdempotentOnContent(t *testing.T) {
	h := newHarness(t, false)
	first := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	// Same text, different whitespace and title: the same article.
	res, err := h.svc.Submit(context.Background(), me, reading.Draft{Title: "別のタイトル", Content: "  " + strings.ReplaceAll(article, "\n\n", "\n\n\n") + "\n"}, appreading.SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate || res.Edition.ID != first.Edition.ID || res.Article.ID != first.Article.ID {
		t.Fatalf("second submit = %+v, want the first edition back", res)
	}
	h.svc.Drain(context.Background())
	if h.analyser.calls != 1 {
		t.Fatalf("model calls = %d, want 1", h.analyser.calls)
	}
}

func TestSubmitValidation(t *testing.T) {
	h := newHarness(t, false)
	_, err := h.svc.Submit(context.Background(), me, reading.Draft{Content: " \n "}, appreading.SubmitOptions{})
	if !errors.Is(err, reading.ErrEmptyContent) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitAcceptsOtherLanguages(t *testing.T) {
	h := newHarness(t, false)
	res, err := h.svc.Submit(context.Background(), me, reading.Draft{Content: "Only English here."}, appreading.SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Article.OriginalLanguage != "und" || !res.Article.NeedsTranslation() {
		t.Fatalf("article = %+v", res.Article)
	}
}

func TestAnalysisRetriesWithBackoffThenSucceeds(t *testing.T) {
	h := newHarness(t, false)
	h.analyser.n = 1
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	e := h.edition(t, res.Edition.ID)
	if e.Status != reading.EditionPending || e.Attempts != 1 || !strings.Contains(e.LastError, "provider unavailable") {
		t.Fatalf("after failed attempt: %+v", e)
	}
	// Not due yet: draining again does nothing.
	h.svc.Drain(context.Background())
	if h.analyser.calls != 1 {
		t.Fatalf("retried before backoff elapsed (calls=%d)", h.analyser.calls)
	}
	h.advance(reading.RetryDelay(1) + time.Second)
	h.svc.Drain(context.Background())
	if e := h.edition(t, res.Edition.ID); e.Status != reading.EditionReady || e.LastError != "" {
		t.Fatalf("after retry: %+v", e)
	}
}

func TestAnalysisGivesUpAfterMaxAttempts(t *testing.T) {
	h := newHarness(t, false)
	h.analyser.n = 100
	res := h.submit(t, appreading.SubmitOptions{})
	for i := 0; i < reading.MaxAnalysisAttempts+2; i++ {
		h.svc.Drain(context.Background())
		h.advance(time.Hour)
	}
	e := h.edition(t, res.Edition.ID)
	if e.Status != reading.EditionFailed || h.analyser.calls != reading.MaxAnalysisAttempts {
		t.Fatalf("edition = %+v, calls = %d", e, h.analyser.calls)
	}
	// A failed edition does not block a fresh attempt: resubmitting the
	// same article queues a new edition.
	h.analyser.n = 0
	again := h.submit(t, appreading.SubmitOptions{})
	if again.Duplicate || again.Edition.ID == res.Edition.ID {
		t.Fatalf("resubmit after failure = %+v", again)
	}
}

// A worker that died mid-analysis leaves the edition "analysing"; once
// the claim is stale another worker takes it over, and the dead
// worker's late result cannot overwrite the new one.
func TestStaleClaimIsReclaimed(t *testing.T) {
	h := newHarness(t, false)
	res := h.submit(t, appreading.SubmitOptions{})
	now := *h.clock
	dead, ok, err := h.repo.ClaimDueEdition(context.Background(), now, now.Add(-time.Hour))
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	h.svc.Drain(context.Background())
	if h.analyser.calls != 0 {
		t.Fatal("a fresh claim must not be taken over")
	}
	h.advance(10 * time.Minute)
	h.svc.Drain(context.Background())
	if e := h.edition(t, res.Edition.ID); e.Status != reading.EditionReady {
		t.Fatalf("stale claim not recovered: %+v", e)
	}
	if err := h.repo.FailEditionAttempt(context.Background(), dead, "late", true, now, now); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale worker overwrote the outcome: %v", err)
	}
}

func TestDeliverWhenReadySendsOnce(t *testing.T) {
	h := newHarness(t, true)
	res := h.submit(t, appreading.SubmitOptions{Deliver: true})
	h.svc.Drain(context.Background())
	if len(h.deliver.parcels) != 1 {
		t.Fatalf("parcels = %d", len(h.deliver.parcels))
	}
	p := h.deliver.parcels[0]
	if p.To != "me@kindle.com" || !strings.HasSuffix(p.Filename, ".epub") || !strings.HasPrefix(p.Filename, "JLP 2026-09-29 経済対策") || p.MediaType != "application/epub+zip" {
		t.Fatalf("parcel = %+v", p)
	}
	if h.events.count(event.TypeReadingEditionDelivered) != 1 {
		t.Fatal("reading.edition.delivered not recorded")
	}
	// Submitting the same article again with Deliver: it has already
	// reached the Kindle, so nothing is sent a second time…
	again := h.submit(t, appreading.SubmitOptions{Deliver: true})
	if !again.Duplicate || again.Delivery == nil || again.Delivery.Status != reading.DeliverySent {
		t.Fatalf("resubmit = %+v", again)
	}
	h.svc.Drain(context.Background())
	if len(h.deliver.parcels) != 1 || h.analyser.calls != 1 {
		t.Fatalf("parcels = %d, model calls = %d", len(h.deliver.parcels), h.analyser.calls)
	}
	// …while an explicit Deliver is a deliberate resend.
	if _, created, err := h.svc.Deliver(context.Background(), me, res.Edition.ID); err != nil || !created {
		t.Fatalf("explicit resend: %v %v", created, err)
	}
	h.svc.Drain(context.Background())
	if len(h.deliver.parcels) != 2 {
		t.Fatalf("parcels after resend = %d", len(h.deliver.parcels))
	}
}

func TestDeliverDeduplicatesInFlight(t *testing.T) {
	h := newHarness(t, true)
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	d1, created1, err := h.svc.Deliver(context.Background(), me, res.Edition.ID)
	if err != nil || !created1 {
		t.Fatalf("first deliver: %v %v", created1, err)
	}
	d2, created2, err := h.svc.Deliver(context.Background(), me, res.Edition.ID)
	if err != nil || created2 || d2.ID != d1.ID {
		t.Fatalf("double click queued a second delivery: %+v %v %v", d2, created2, err)
	}
	h.svc.Drain(context.Background())
	if len(h.deliver.parcels) != 1 {
		t.Fatalf("parcels = %d, want 1", len(h.deliver.parcels))
	}
}

// A failed email is retried without re-analysing; a permanent failure
// is not retried at all.
func TestDeliveryFailures(t *testing.T) {
	h := newHarness(t, true)
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())

	h.deliver.fail = []error{errors.New("451 try again later")}
	if _, _, err := h.svc.Deliver(context.Background(), me, res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.Drain(context.Background())
	if len(h.deliver.parcels) != 0 {
		t.Fatal("should have failed first")
	}
	h.advance(time.Hour)
	h.svc.Drain(context.Background())
	if len(h.deliver.parcels) != 1 || h.analyser.calls != 1 {
		t.Fatalf("retry: parcels=%d model calls=%d", len(h.deliver.parcels), h.analyser.calls)
	}

	h.deliver.fail = []error{errors.Join(publishing.ErrPermanent, errors.New("550 mailbox unavailable"))}
	d, _, err := h.svc.Deliver(context.Background(), me, res.Edition.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.Drain(context.Background())
	h.advance(time.Hour)
	h.svc.Drain(context.Background())
	dls, _ := h.repo.ListDeliveries(context.Background(), me, res.Edition.ID)
	for _, x := range dls {
		if x.ID == d.ID && (x.Status != reading.DeliveryFailed || x.Attempts != 1 || !strings.Contains(x.LastError, "550")) {
			t.Fatalf("permanent failure: %+v", x)
		}
	}
}

func TestDeliverRequiresConfigAndReadyEdition(t *testing.T) {
	h := newHarness(t, false)
	res := h.submit(t, appreading.SubmitOptions{Deliver: true})
	if _, _, err := h.svc.Deliver(context.Background(), me, res.Edition.ID); !errors.Is(err, appreading.ErrDeliveryNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	h.svc.Drain(context.Background())
	if len(h.deliver.parcels) != 0 {
		t.Fatal("Deliver option must be ignored without Kindle config")
	}

	k := newHarness(t, true)
	res = k.submit(t, appreading.SubmitOptions{})
	if _, _, err := k.svc.Deliver(context.Background(), me, res.Edition.ID); !errors.Is(err, appreading.ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if _, err := k.svc.RenderEbook(context.Background(), me, res.Edition.ID); !errors.Is(err, appreading.ErrNotReady) {
		t.Fatalf("render err = %v, want ErrNotReady", err)
	}
}

func TestOtherIdentityCannotSeeEdition(t *testing.T) {
	h := newHarness(t, true)
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	other := learner.IdentityID("someone-else")
	if _, err := h.svc.Edition(context.Background(), other, res.Edition.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Edition: %v", err)
	}
	if _, err := h.svc.RenderEbook(context.Background(), other, res.Edition.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("RenderEbook: %v", err)
	}
	if _, _, err := h.svc.Deliver(context.Background(), other, res.Edition.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Deliver: %v", err)
	}
	if err := h.svc.Delete(context.Background(), other, res.Article.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Delete: %v", err)
	}
}

func TestRegenerateQueuesNewEditionOnce(t *testing.T) {
	h := newHarness(t, false)
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	h.advance(time.Second) // editions are ordered by creation time
	n1, err := h.svc.Regenerate(context.Background(), me, res.Edition.ID)
	if err != nil || n1.ID == res.Edition.ID || n1.Status != reading.EditionPending {
		t.Fatalf("regenerate: %+v %v", n1, err)
	}
	n2, err := h.svc.Regenerate(context.Background(), me, res.Edition.ID)
	if err != nil || n2.ID != n1.ID {
		t.Fatalf("second regenerate queued another: %+v", n2)
	}
	h.svc.Drain(context.Background())
	if h.analyser.calls != 2 {
		t.Fatalf("calls = %d", h.analyser.calls)
	}
	list, _ := h.svc.List(context.Background(), me)
	if len(list) != 1 || list[0].EditionID != n1.ID {
		t.Fatalf("list should show the newest edition only: %+v", list)
	}
}

func TestAddVocabularyAndAnkiAreIdempotentKeys(t *testing.T) {
	h := newHarness(t, false)
	res := h.submit(t, appreading.SubmitOptions{})
	if _, err := h.svc.AddVocabulary(context.Background(), me, res.Edition.ID); !errors.Is(err, appreading.ErrNotReady) {
		t.Fatalf("before ready: %v", err)
	}
	h.svc.Drain(context.Background())
	n, err := h.svc.AddVocabulary(context.Background(), me, res.Edition.ID)
	if err != nil || n == 0 {
		t.Fatalf("add: %d %v", n, err)
	}
	if _, err := h.svc.AddVocabulary(context.Background(), me, res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.vocab.ingested) != n {
		t.Fatalf("client_event_ids not stable: %d ingested for %d words", len(h.vocab.ingested), n)
	}
	for id, ev := range h.vocab.ingested {
		if !strings.HasPrefix(id, "reading:"+res.Edition.ID+":") || ev.Type != "vocabulary.lookup" || ev.Source.Type != "article" || ev.Source.Title != "経済対策" {
			t.Fatalf("ingest event = %s %+v", id, ev)
		}
	}
	if _, err := h.svc.CreateAnkiCards(context.Background(), me, res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.anki.drafts) != n || h.anki.drafts[0].SourceType != "reading_vocabulary" || !strings.Contains(h.anki.drafts[0].Back, "monetary tightening") {
		t.Fatalf("drafts = %+v", h.anki.drafts)
	}
}

func TestEditionMarksKnownVocabulary(t *testing.T) {
	h := newHarness(t, false)
	h.vocab.known = []string{"経済対策"}
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	d, err := h.svc.Edition(context.Background(), me, res.Edition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Known["経済対策"] || d.Known["金融引き締め"] {
		t.Fatalf("known = %v", d.Known)
	}
}

func TestDeleteHidesAndRestoreBringsBack(t *testing.T) {
	h := newHarness(t, false)
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	if err := h.svc.Delete(context.Background(), me, res.Article.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Edition(context.Background(), me, res.Edition.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted article's edition still visible: %v", err)
	}
	if list, _ := h.svc.List(context.Background(), me); len(list) != 0 {
		t.Fatalf("list = %+v", list)
	}
	if err := h.svc.Restore(context.Background(), me, res.Article.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Edition(context.Background(), me, res.Edition.ID); err != nil {
		t.Fatalf("restored edition: %v", err)
	}
	if h.events.count(event.TypeContentDeleted) != 1 || h.events.count(event.TypeContentRestored) != 1 {
		t.Fatal("content.deleted/restored not recorded")
	}
}

// Delete is undoable (Restore), but re-importing a deleted article is
// not an undo: it starts over, with a fresh article and edition, and the
// deleted ones are gone for good.
func TestReimportingADeletedArticleStartsFresh(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	first := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(ctx)
	if err := h.svc.Delete(ctx, me, first.Article.ID); err != nil {
		t.Fatal(err)
	}
	again := h.submit(t, appreading.SubmitOptions{})
	if again.Duplicate || again.Edition.ID == first.Edition.ID || again.Article.ID == first.Article.ID {
		t.Fatalf("re-import = duplicate %v, article %s (was %s), edition %s (was %s)", again.Duplicate, again.Article.ID, first.Article.ID, again.Edition.ID, first.Edition.ID)
	}
	if list, _ := h.svc.List(ctx, me); len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if _, err := h.repo.GetEdition(ctx, me, first.Edition.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted edition survived a re-import: %v", err)
	}
}

// A queued analysis whose article is deleted before it runs fails
// without calling the model.
func TestDeletedArticleIsNotAnalysed(t *testing.T) {
	h := newHarness(t, false)
	res := h.submit(t, appreading.SubmitOptions{})
	if err := h.svc.Delete(context.Background(), me, res.Article.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.Drain(context.Background())
	if h.analyser.calls != 0 {
		t.Fatal("model called for a deleted article")
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

type fakeCover struct {
	calls []publishing.CoverInput
	fail  func(in publishing.CoverInput) bool
	boom  bool // panic instead of returning
}

func (c *fakeCover) Design(_ context.Context, in publishing.CoverInput) ([]byte, error) {
	c.calls = append(c.calls, in)
	if c.boom {
		panic("cover: font face exploded")
	}
	if c.fail != nil && c.fail(in) {
		return nil, errors.New("cover: bad photo")
	}
	return []byte("COVER"), nil
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSubmitAttachesFiguresAndResubmitAddsThemOnce(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	d := reading.Draft{Title: "経済対策", Content: "政府は新たな経済対策をまとめた。\n\n物価高への対応が柱となる。"}
	res, err := h.svc.Submit(ctx, "me", d, appreading.SubmitOptions{})
	if err != nil || res.Figures != 0 {
		t.Fatalf("text-only submit = %+v %v", res, err)
	}
	d.Figures = []reading.FigureDraft{
		{Data: jpegBytes(t), InText: true, Lead: true, Caption: "記者会見"},
		{Data: []byte("not an image")},
	}
	res2, err := h.svc.Submit(ctx, "me", d, appreading.SubmitOptions{})
	if err != nil || !res2.Duplicate || res2.Figures != 1 || res2.FiguresRejected != 1 {
		t.Fatalf("resubmit with figures = %+v %v", res2, err)
	}
	d.Figures = []reading.FigureDraft{{Data: jpegBytes(t), InText: true}, {Data: jpegBytes(t), InText: true}}
	res3, _ := h.svc.Submit(ctx, "me", d, appreading.SubmitOptions{})
	if res3.Figures != 1 {
		t.Fatalf("an article that has figures keeps them: %+v", res3)
	}
}

func TestRenderPassesFiguresAndCover(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	res, _ := h.svc.Submit(ctx, "me", reading.Draft{Title: "経済対策", SourceName: "NHK", Content: "政府は新たな経済対策をまとめた。",
		Figures: []reading.FigureDraft{{Data: jpegBytes(t), InText: true, Lead: true}}}, appreading.SubmitOptions{})
	h.svc.Drain(ctx)
	if _, err := h.svc.RenderEbook(ctx, "me", res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	got := h.render.last
	if len(got.Figures) != 1 || len(got.Figures[0].Data) == 0 || string(got.Cover) != "COVER" {
		t.Fatalf("ebook figures=%d cover=%q", len(got.Figures), got.Cover)
	}
	if in := h.cover.calls[0]; in.Title != "経済対策" || in.Source != "NHK" || len(in.Photo) == 0 {
		t.Fatalf("cover input = %+v", in)
	}
}

func TestRenderFallsBackToNoPhotoCoverThenNoCover(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	res, _ := h.svc.Submit(ctx, "me", reading.Draft{Title: "t", Content: "政府は新たな経済対策をまとめた。",
		Figures: []reading.FigureDraft{{Data: jpegBytes(t), InText: true, Lead: true}}}, appreading.SubmitOptions{})
	h.svc.Drain(ctx)
	h.cover.fail = func(in publishing.CoverInput) bool { return in.Photo != nil }
	if _, err := h.svc.RenderEbook(ctx, "me", res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.cover.calls); n != 2 || h.cover.calls[1].Photo != nil || string(h.render.last.Cover) != "COVER" {
		t.Fatalf("calls=%d cover=%q: want a photo attempt then a no-photo one", n, h.render.last.Cover)
	}
	h.cover.fail = func(publishing.CoverInput) bool { return true }
	if _, err := h.svc.RenderEbook(ctx, "me", res.Edition.ID); err != nil {
		t.Fatalf("a failed cover must not fail the book: %v", err)
	}
	if h.render.last.Cover != nil {
		t.Fatal("no cover expected after both attempts failed")
	}
}

func TestFigureIsIdentityScoped(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	res, _ := h.svc.Submit(ctx, "me", reading.Draft{Title: "t", Content: "政府は新たな経済対策をまとめた。",
		Figures: []reading.FigureDraft{{Data: jpegBytes(t), InText: true}}}, appreading.SubmitOptions{})
	if f, err := h.svc.Figure(ctx, "me", res.Article.ID, 0); err != nil || len(f.Data) == 0 {
		t.Fatalf("own figure = %v", err)
	}
	if _, err := h.svc.Figure(ctx, "them", res.Article.ID, 0); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("foreign figure err = %v", err)
	}
	d, _ := h.svc.Edition(ctx, "me", res.Edition.ID)
	if len(d.Figures) != 1 || d.Figures[0].Data != nil {
		t.Fatalf("detail figures = %+v", d.Figures)
	}
}

func TestRenderSurvivesACoverPanic(t *testing.T) {
	h := newHarness(t, false)
	h.cover.boom = true
	ctx := context.Background()
	res, _ := h.svc.Submit(ctx, "me", reading.Draft{Title: "t", Content: "政府は新たな経済対策をまとめた。",
		Figures: []reading.FigureDraft{{Data: jpegBytes(t), InText: true, Lead: true}}}, appreading.SubmitOptions{})
	h.svc.Drain(ctx)
	if _, err := h.svc.RenderEbook(ctx, "me", res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	if h.render.last.Cover != nil || len(h.cover.calls) != 2 {
		t.Fatalf("cover=%q calls=%d, want no cover after photo and no-photo attempts", h.render.last.Cover, len(h.cover.calls))
	}
}

// --- drafts: pages of blocks, seq = page*10000 + index, one per identity ---

type memDraft struct {
	reading.DraftInfo
	blocks []reading.DraftBlock
}

func (m *memRepo) draftFor(id learner.IdentityID, did string) (*memDraft, bool) {
	d, ok := m.drafts[id]
	if !ok || d.ID != did {
		return nil, false
	}
	return d, true
}

func (m *memRepo) AddDraftPage(_ context.Context, id learner.IdentityID, meta reading.DraftMeta, pageURL string, blocks []reading.DraftBlock, now time.Time) (reading.DraftInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(blocks) >= 10000 {
		return reading.DraftInfo{}, fmt.Errorf("draft page has %d blocks", len(blocks))
	}
	if m.drafts == nil {
		m.drafts = map[learner.IdentityID]*memDraft{}
	}
	d, existed := m.drafts[id]
	if !existed {
		m.nextDraft++
		d = &memDraft{DraftInfo: reading.DraftInfo{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", m.nextDraft), Identity: id, Meta: meta, CreatedAt: now}}
	}
	page, replacing := 0, false
	pages, maxPage := map[int]bool{}, 0
	for _, b := range d.blocks {
		pages[b.Page] = true
		maxPage = max(maxPage, b.Page)
		if !replacing && pageURL != "" && b.PageURL == pageURL {
			page, replacing = b.Page, true
		}
	}
	if !replacing {
		if len(pages) >= reading.MaxDraftPages {
			return reading.DraftInfo{}, reading.ErrDraftFull
		}
		page = maxPage + 1
	}
	kept := d.blocks[:0:0]
	for _, b := range d.blocks {
		if !replacing || b.Page != page {
			kept = append(kept, b)
		}
	}
	for i, b := range blocks {
		b.Page, b.Seq, b.PageURL = page, page*10000+i, pageURL
		if b.Kind != reading.BlockImage {
			b.Figure = reading.Figure{}
		}
		kept = append(kept, b)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Seq < kept[j].Seq })
	d.blocks, d.UpdatedAt = kept, now
	m.drafts[id] = d
	return d.DraftInfo, nil
}

func (m *memRepo) ActiveDraft(_ context.Context, id learner.IdentityID) (reading.DraftInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.drafts[id]
	if !ok {
		return reading.DraftInfo{}, storage.ErrNotFound
	}
	return d.DraftInfo, nil
}

func (m *memRepo) GetDraft(_ context.Context, id learner.IdentityID, did string) (reading.DraftInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.draftFor(id, did)
	if !ok {
		return reading.DraftInfo{}, storage.ErrNotFound
	}
	return d.DraftInfo, nil
}

func (m *memRepo) ListDraftBlocks(_ context.Context, id learner.IdentityID, did string, withData bool) ([]reading.DraftBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.draftFor(id, did)
	if !ok {
		return nil, nil
	}
	out := make([]reading.DraftBlock, 0, len(d.blocks))
	for _, b := range d.blocks {
		if !withData {
			b.Figure.Data = nil
		}
		out = append(out, b)
	}
	return out, nil
}

func (m *memRepo) DraftImage(_ context.Context, id learner.IdentityID, did string, seq int) (reading.Figure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.draftFor(id, did); ok {
		for _, b := range d.blocks {
			if b.Seq == seq && b.Kind == reading.BlockImage && b.Figure.Data != nil {
				return b.Figure, nil
			}
		}
	}
	return reading.Figure{}, storage.ErrNotFound
}

func (m *memRepo) SetDraftBlockExcluded(_ context.Context, id learner.IdentityID, did string, seq int, excluded bool, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.draftFor(id, did); ok {
		for i := range d.blocks {
			if d.blocks[i].Seq == seq {
				d.blocks[i].Excluded, d.UpdatedAt = excluded, now
				return nil
			}
		}
	}
	return storage.ErrNotFound
}

func (m *memRepo) SetDraftTitle(_ context.Context, id learner.IdentityID, did, title string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.draftFor(id, did)
	if !ok {
		return storage.ErrNotFound
	}
	d.Meta.Title, d.UpdatedAt = title, now
	return nil
}

func (m *memRepo) DeleteDraft(_ context.Context, id learner.IdentityID, did string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.draftFor(id, did); !ok {
		return storage.ErrNotFound
	}
	delete(m.drafts, id)
	return nil
}

func (m *memRepo) SweepDrafts(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, d := range m.drafts {
		if d.UpdatedAt.Before(before) {
			delete(m.drafts, id)
			n++
		}
	}
	return n, nil
}

func TestEnglishArticleIsTranslatedOnceThenAnalysedInJapanese(t *testing.T) {
	h := newHarness(t, false)
	tr, rec := h.withTranslator(t, 0)
	res := h.submitEnglish(t)
	h.svc.Drain(context.Background())
	if e := h.edition(t, res.Edition.ID); e.Status != reading.EditionReady {
		t.Fatalf("edition = %+v", e)
	}
	if tr.calls != 1 || len(rec.inputs) != 1 {
		t.Fatalf("translator calls = %d, analyser calls = %d", tr.calls, len(rec.inputs))
	}
	if got := rec.inputs[0].Text; !strings.Contains(got, "（訳）1段落目") || strings.Contains(got, "bank held") {
		t.Fatalf("analyser got %q, want the Japanese translation", got)
	}
	a, _ := h.repo.GetArticle(context.Background(), me, res.Article.ID)
	if a.OriginalLanguage != "英語" || a.OriginalTitle != "Central bank" || len(a.OriginalParagraphs) != 2 || a.NeedsTranslation() {
		t.Fatalf("stored article = %+v", a)
	}

	// A regenerated edition reuses the stored translation.
	h.advance(time.Second)
	if _, err := h.svc.Regenerate(context.Background(), me, res.Edition.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.Drain(context.Background())
	if tr.calls != 1 || len(rec.inputs) != 2 {
		t.Fatalf("after regenerate: translator calls = %d, analyser calls = %d", tr.calls, len(rec.inputs))
	}
}

func TestJapaneseArticleIsNeverTranslated(t *testing.T) {
	h := newHarness(t, false)
	tr, _ := h.withTranslator(t, 0)
	res := h.submit(t, appreading.SubmitOptions{})
	h.svc.Drain(context.Background())
	if e := h.edition(t, res.Edition.ID); e.Status != reading.EditionReady || tr.calls != 0 {
		t.Fatalf("edition = %+v, translator calls = %d", e, tr.calls)
	}
}

func TestTranslationFailureIsRetriedWithoutAnalysing(t *testing.T) {
	h := newHarness(t, false)
	tr, rec := h.withTranslator(t, 1)
	res := h.submitEnglish(t)
	h.svc.Drain(context.Background())
	e := h.edition(t, res.Edition.ID)
	if e.Status != reading.EditionPending || e.Attempts != 1 || !strings.Contains(e.LastError, "translator unavailable") || len(rec.inputs) != 0 {
		t.Fatalf("after failed attempt: %+v (analyser calls %d)", e, len(rec.inputs))
	}
	h.advance(reading.RetryDelay(1) + time.Second)
	h.svc.Drain(context.Background())
	if e := h.edition(t, res.Edition.ID); e.Status != reading.EditionReady || tr.calls != 2 {
		t.Fatalf("after retry: %+v (translator calls %d)", e, tr.calls)
	}
}

func TestForeignArticleWithoutTranslatorFailsForGood(t *testing.T) {
	h := newHarness(t, false) // no Translator configured
	res := h.submitEnglish(t)
	h.svc.Drain(context.Background())
	e := h.edition(t, res.Edition.ID)
	if e.Status != reading.EditionFailed || !strings.Contains(e.LastError, "no translator") || h.analyser.calls != 0 {
		t.Fatalf("edition = %+v, analyser calls = %d", e, h.analyser.calls)
	}
}
