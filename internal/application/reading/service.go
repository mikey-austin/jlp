// Package reading is the 読解 pipeline: a Japanese article the learner
// chose to read comes in (from the Chrome extension or the /reading
// page), a background worker asks the reading agent for a study edition
// of it, and the edition is rendered as an EPUB for download or sent to
// the learner's Kindle.
//
// The pipeline is staged and durable, with every stage's state in
// Postgres rather than in a request or a goroutine:
//
//	Submit ─▶ article (idempotent on its content hash)
//	       └▶ edition: pending ─▶ analysing ─▶ ready ─▶ (deliver) ─▶ sent
//	                      ▲           │
//	                      └─ backoff ─┘ … failed
//
// Submit only records work and returns; Worker does it. So a request
// never waits on a model, a crash mid-analysis is picked up again once
// its claim goes stale, and a failed email is retried without calling
// the model again — the edition it sends is already stored.
package reading

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	agentreading "github.com/mikeyaustin/jlp/internal/agent/reading"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/publishing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Errors the HTTP layer maps to specific responses.
var (
	// ErrNotReady: the edition has not finished analysis (or failed), so
	// there is nothing to render or send yet.
	ErrNotReady = errors.New("reading: the study edition is not ready yet")
	// ErrDeliveryNotConfigured: Send-to-Kindle is not set up in this
	// deployment (APP_READING_KINDLE_TO/FROM unset).
	ErrDeliveryNotConfigured = errors.New("reading: Kindle delivery is not configured")
)

// Analyser is what the worker asks for a study edition — the reading
// agent in production.
type Analyser interface {
	Analyse(ctx context.Context, in agentreading.Input) (reading.Lesson, ai.StructuredResponse, error)
}

// Translator turns a non-Japanese article into Japanese, paragraph for
// paragraph — the translation agent in production.
type Translator interface {
	Translate(ctx context.Context, identity learner.IdentityID, a reading.Article) (reading.Translation, ai.StructuredResponse, error)
}

// VocabularyService is the part of application/vocabulary this pipeline
// uses: adding an edition's words to the learner's list, and seeing
// which of them are already there.
type VocabularyService interface {
	Ingest(ctx context.Context, identity learner.IdentityID, ev appvocabulary.IngestEvent) (vocabulary.Item, error)
	GetByExpressions(ctx context.Context, identity learner.IdentityID, expressions []string) ([]vocabulary.Item, error)
}

// AnkiService is the part of application/anki this pipeline uses.
type AnkiService interface {
	CreateDraft(ctx context.Context, identity learner.IdentityID, drafts []appanki.Draft) (int, error)
}

// Config is the pipeline's tunable behaviour.
type Config struct {
	// MaxArticleRunes caps one article's body (0 = the domain default).
	MaxArticleRunes int
	// LearnerLevel is rendered into the prompt to pitch vocabulary
	// selection; empty uses the agent's default.
	LearnerLevel string
	// KindleTo is the Send-to-Kindle address. Empty disables delivery,
	// whatever Deliverer is.
	KindleTo string
	// AnalysisTimeout bounds one analysis attempt (default 5m).
	AnalysisTimeout time.Duration
}

// Deps are the service's collaborators. Only Repo and Recorder are
// needed for reads and deletes; cmd/jlp's restore command builds a
// Service with just those two.
type Deps struct {
	Repo       storage.ReadingRepository
	Recorder   *learning.Recorder
	Analyser   Analyser
	Translator Translator // nil: only Japanese articles can be analysed
	Renderer   publishing.Renderer
	Deliverer  publishing.Deliverer // nil: delivery not configured
	Vocabulary VocabularyService
	Anki       AnkiService
	Cover      publishing.CoverDesigner // nil: no cover
}

// Service is the pipeline's application layer.
type Service struct {
	d    Deps
	cfg  Config
	now  func() time.Time
	wake chan struct{}
}

// NewService wires the pipeline.
func NewService(d Deps, cfg Config) *Service {
	if cfg.AnalysisTimeout <= 0 {
		cfg.AnalysisTimeout = 5 * time.Minute
	}
	return &Service{d: d, cfg: cfg, now: func() time.Time { return time.Now().UTC() }, wake: make(chan struct{}, 1)}
}

// DeliveryEnabled reports whether Send-to-Kindle is configured — the
// pages use it to decide whether to offer the button at all, the same
// "don't render a control that cannot work" rule AnkiConnectEnabled
// follows.
func (s *Service) DeliveryEnabled() bool {
	return s.d.Deliverer != nil && s.cfg.KindleTo != ""
}

// notify nudges the worker to look for work now instead of at its next
// tick. Never blocks: one pending nudge is as good as many.
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SubmitOptions are the per-request choices.
type SubmitOptions struct {
	// Deliver asks for the edition to be sent to the Kindle as soon as
	// it is ready. Ignored when delivery is not configured.
	Deliver bool
}

// SubmitResult reports what Submit did.
type SubmitResult struct {
	Article reading.Article
	Edition reading.StudyEdition
	// Duplicate is true when this article (same identity, same text)
	// already existed and an existing edition was reused — no new model
	// call was queued.
	Duplicate bool
	// Delivery is set when Submit queued (or found in flight) a Kindle
	// delivery for an edition that was already ready.
	Delivery *reading.Delivery
	// Figures is how many figures the article has after this submit;
	// FiguresRejected how many of the submitted ones failed validation.
	Figures, FiguresRejected int
}

// Submit ingests an article and queues its study edition. It never
// calls the model: the worker does that. Idempotent — the same text from
// the same learner returns the existing article and, unless its last
// edition failed, that edition.
func (s *Service) Submit(ctx context.Context, identity learner.IdentityID, d reading.Draft, opts SubmitOptions) (SubmitResult, error) {
	now := s.now()
	a, err := reading.NewArticle(uuid.NewString(), identity, d, s.cfg.MaxArticleRunes, now)
	if err != nil {
		return SubmitResult{}, err
	}
	stored, created, err := s.d.Repo.UpsertArticle(ctx, a)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("reading: store article: %w", err)
	}
	res := SubmitResult{Article: stored}
	res.Figures, res.FiguresRejected = s.attachFigures(ctx, identity, stored, d.Figures)
	deliver := opts.Deliver && s.DeliveryEnabled()

	if !created {
		latest, err := s.d.Repo.LatestEdition(ctx, identity, stored.ID, agentreading.PromptName, agentreading.PromptVersion)
		switch {
		case err == nil && latest.Status != reading.EditionFailed:
			res.Edition, res.Duplicate = latest, true
			if deliver && latest.Status == reading.EditionReady {
				dl, err := s.deliverOnce(ctx, identity, latest.ID)
				if err != nil {
					return SubmitResult{}, err
				}
				res.Delivery = &dl
			}
			return res, nil
		case err != nil && !errors.Is(err, storage.ErrNotFound):
			return SubmitResult{}, fmt.Errorf("reading: latest edition: %w", err)
		}
		// No edition with the current prompt, or the last one failed:
		// queue a fresh one below.
	}

	e, err := s.queueEdition(ctx, identity, stored.ID, deliver)
	if err != nil {
		return SubmitResult{}, err
	}
	res.Edition = e
	return res, nil
}

// attachFigures validates and stores an article's figures. Images are
// optional: every failure here is logged and swallowed.
func (s *Service) attachFigures(ctx context.Context, identity learner.IdentityID, a reading.Article, drafts []reading.FigureDraft) (have, rejected int) {
	log := slog.With("identity", identity, "article", a.ID)
	if len(drafts) > 0 {
		figs, bad := reading.NewFigures(drafts, a.Paragraphs)
		for _, err := range bad {
			log.Warn("reading: figure rejected", "err", err)
		}
		rejected = len(bad)
		if len(figs) > 0 {
			if _, err := s.d.Repo.AttachFigures(ctx, identity, a.ID, figs); err != nil {
				log.Warn("reading: attach figures", "err", err)
			}
		}
	}
	list, err := s.d.Repo.ListFigures(ctx, identity, a.ID)
	if err != nil {
		log.Warn("reading: list figures", "err", err)
	}
	return len(list), rejected
}

// Figure returns one of an article's figures with its bytes.
func (s *Service) Figure(ctx context.Context, identity learner.IdentityID, articleID string, ordinal int) (reading.Figure, error) {
	return s.d.Repo.FigureData(ctx, identity, articleID, ordinal)
}

func (s *Service) queueEdition(ctx context.Context, identity learner.IdentityID, articleID string, deliver bool) (reading.StudyEdition, error) {
	now := s.now()
	e := reading.StudyEdition{
		ID:               uuid.NewString(),
		ArticleID:        articleID,
		IdentityID:       identity,
		Status:           reading.EditionPending,
		PromptName:       agentreading.PromptName,
		PromptVersion:    agentreading.PromptVersion,
		SchemaName:       agentreading.SchemaName,
		DeliverWhenReady: deliver,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.d.Repo.InsertEdition(ctx, e); err != nil {
		return reading.StudyEdition{}, fmt.Errorf("reading: queue edition: %w", err)
	}
	s.notify()
	return e, nil
}

// Regenerate queues a new edition of editionID's article with the
// current prompt — for a lesson that came out badly, or after a prompt
// upgrade. If an edition of that article is already waiting or being
// analysed, that one is returned instead of queueing a second.
func (s *Service) Regenerate(ctx context.Context, identity learner.IdentityID, editionID string) (reading.StudyEdition, error) {
	e, err := s.d.Repo.GetEdition(ctx, identity, editionID)
	if err != nil {
		return reading.StudyEdition{}, err
	}
	latest, err := s.d.Repo.LatestEdition(ctx, identity, e.ArticleID, agentreading.PromptName, agentreading.PromptVersion)
	if err == nil && !latest.Terminal() {
		return latest, nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return reading.StudyEdition{}, err
	}
	return s.queueEdition(ctx, identity, e.ArticleID, false)
}

// EditionDetail is everything the detail page and the JSON status
// endpoint show about one edition.
type EditionDetail struct {
	Article    reading.Article
	Edition    reading.StudyEdition
	Deliveries []reading.Delivery
	// Known is the set of the lesson's vocabulary expressions already on
	// the learner's vocabulary list.
	Known map[string]bool
	// Figures are the article's images, without their bytes.
	Figures []reading.Figure
}

// Edition reads one edition with its article and deliveries.
func (s *Service) Edition(ctx context.Context, identity learner.IdentityID, editionID string) (EditionDetail, error) {
	e, err := s.d.Repo.GetEdition(ctx, identity, editionID)
	if err != nil {
		return EditionDetail{}, err
	}
	a, err := s.d.Repo.GetArticle(ctx, identity, e.ArticleID)
	if err != nil {
		return EditionDetail{}, err
	}
	dls, err := s.d.Repo.ListDeliveries(ctx, identity, e.ID)
	if err != nil {
		return EditionDetail{}, err
	}
	out := EditionDetail{Article: a, Edition: e, Deliveries: dls, Known: map[string]bool{}}
	figs, err := s.d.Repo.ListFigures(ctx, identity, a.ID)
	if err != nil {
		slog.Warn("reading: list figures", "identity", identity, "article", a.ID, "err", err)
	}
	out.Figures = figs
	if e.Lesson != nil && s.d.Vocabulary != nil {
		exprs := make([]string, 0, len(e.Lesson.Vocabulary))
		for _, v := range e.Lesson.Vocabulary {
			exprs = append(exprs, v.Expression)
		}
		items, err := s.d.Vocabulary.GetByExpressions(ctx, identity, exprs)
		if err != nil {
			// A decoration, not the page: log and render without it.
			slog.Warn("reading: known vocabulary lookup", "identity", identity, "edition", e.ID, "err", err)
		}
		for _, it := range items {
			out.Known[it.Expression] = true
		}
	}
	return out, nil
}

// List returns the learner's articles with their newest editions.
func (s *Service) List(ctx context.Context, identity learner.IdentityID) ([]storage.ReadingEditionSummary, error) {
	return s.d.Repo.ListEditions(ctx, identity)
}

// Ebook is a rendered edition, ready to download.
type Ebook struct {
	Filename  string
	MediaType string
	Data      []byte
}

// RenderEbook renders a ready edition.
func (s *Service) RenderEbook(ctx context.Context, identity learner.IdentityID, editionID string) (Ebook, error) {
	e, err := s.d.Repo.GetEdition(ctx, identity, editionID)
	if err != nil {
		return Ebook{}, err
	}
	a, err := s.d.Repo.GetArticle(ctx, identity, e.ArticleID)
	if err != nil {
		return Ebook{}, err
	}
	return s.render(ctx, a, e)
}

func (s *Service) render(ctx context.Context, a reading.Article, e reading.StudyEdition) (Ebook, error) {
	if e.Status != reading.EditionReady || e.Lesson == nil {
		return Ebook{}, ErrNotReady
	}
	figs := s.figuresWithData(ctx, a)
	book := publishing.Ebook{ID: e.ID, Article: a, Lesson: *e.Lesson, GeneratedAt: e.UpdatedAt, Figures: figs}
	book.Cover = s.cover(ctx, a, figs)
	data, err := s.d.Renderer.Render(ctx, book)
	if err != nil {
		return Ebook{}, fmt.Errorf("reading: render: %w", err)
	}
	return Ebook{Filename: Filename(a, e) + s.d.Renderer.Extension(), MediaType: s.d.Renderer.MediaType(), Data: data}, nil
}

// figuresWithData loads an article's figures with their bytes; one that
// cannot be loaded is left out of the book.
func (s *Service) figuresWithData(ctx context.Context, a reading.Article) []reading.Figure {
	list, err := s.d.Repo.ListFigures(ctx, a.IdentityID, a.ID)
	if err != nil {
		slog.Warn("reading: list figures for render", "article", a.ID, "err", err)
		return nil
	}
	out := make([]reading.Figure, 0, len(list))
	for _, f := range list {
		full, err := s.d.Repo.FigureData(ctx, a.IdentityID, a.ID, f.Ordinal)
		if err != nil {
			slog.Warn("reading: load figure for render", "article", a.ID, "ordinal", f.Ordinal, "err", err)
			continue
		}
		out = append(out, full)
	}
	return out
}

// cover draws the book's cover from the lead figure, falling back to the
// no-photo design and then to no cover: a cover never fails a book.
func (s *Service) cover(ctx context.Context, a reading.Article, figs []reading.Figure) []byte {
	if s.d.Cover == nil {
		return nil
	}
	in := publishing.CoverInput{Title: a.Title, Source: a.SourceName}
	if a.PublishedAt != nil {
		in.Date = a.PublishedAt.Format("2006年1月2日")
	}
	for _, f := range figs {
		if f.Lead {
			in.Photo = f.Data
			break
		}
	}
	if in.Photo == nil && len(figs) > 0 {
		in.Photo = figs[0].Data
	}
	img, err := s.design(ctx, in)
	if err != nil && in.Photo != nil {
		slog.Warn("reading: cover with photo", "article", a.ID, "err", err)
		in.Photo = nil
		img, err = s.design(ctx, in)
	}
	if err != nil {
		slog.Warn("reading: cover", "article", a.ID, "err", err)
		return nil
	}
	return img
}

// design calls the cover designer, turning a panic into an error: a
// cover never fails a book, and a crash here would take the worker down.
func (s *Service) design(ctx context.Context, in publishing.CoverInput) (img []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cover: designer panicked: %v", r)
		}
	}()
	return s.d.Cover.Design(ctx, in)
}

// Filename is the download/attachment name, without extension:
// "JLP 2026-09-29 <title>", filesystem-safe. The date leads so a Kindle
// library sorted by title still reads chronologically.
func Filename(a reading.Article, e reading.StudyEdition) string {
	title := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			return ' '
		}
		return r
	}, a.Title)
	title = strings.Join(strings.Fields(title), " ")
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60])
	}
	return strings.TrimSpace("JLP " + e.CreatedAt.Format("2006-01-02") + " " + title)
}

// Deliver queues a Send-to-Kindle delivery of a ready edition. created
// is false when the same edition is already on its way to the same
// address — the in-flight delivery is returned instead of mailing it
// twice.
func (s *Service) Deliver(ctx context.Context, identity learner.IdentityID, editionID string) (reading.Delivery, bool, error) {
	if !s.DeliveryEnabled() {
		return reading.Delivery{}, false, ErrDeliveryNotConfigured
	}
	e, err := s.d.Repo.GetEdition(ctx, identity, editionID)
	if err != nil {
		return reading.Delivery{}, false, err
	}
	if e.Status != reading.EditionReady {
		return reading.Delivery{}, false, ErrNotReady
	}
	d, created, err := s.d.Repo.QueueDelivery(ctx, s.newDelivery(identity, e.ID))
	if err != nil {
		return reading.Delivery{}, false, fmt.Errorf("reading: queue delivery: %w", err)
	}
	s.notify()
	return d, created, nil
}

func (s *Service) queueDelivery(ctx context.Context, identity learner.IdentityID, editionID string) (reading.Delivery, error) {
	d, _, err := s.d.Repo.QueueDelivery(ctx, s.newDelivery(identity, editionID))
	if err != nil {
		return reading.Delivery{}, fmt.Errorf("reading: queue delivery: %w", err)
	}
	s.notify()
	return d, nil
}

// deliverOnce is the auto-send path for a repeat submission: it queues
// a delivery only if this edition has never reached (and is not on its
// way to) the Kindle. Sending the same article from the extension twice
// must not put two copies in the library; a deliberate resend is the
// explicit Deliver, not a side effect of a duplicate click.
func (s *Service) deliverOnce(ctx context.Context, identity learner.IdentityID, editionID string) (reading.Delivery, error) {
	dls, err := s.d.Repo.ListDeliveries(ctx, identity, editionID)
	if err != nil {
		return reading.Delivery{}, fmt.Errorf("reading: list deliveries: %w", err)
	}
	for _, d := range dls {
		if d.Destination == s.cfg.KindleTo && d.Status != reading.DeliveryFailed {
			return d, nil
		}
	}
	return s.queueDelivery(ctx, identity, editionID)
}

func (s *Service) newDelivery(identity learner.IdentityID, editionID string) reading.Delivery {
	return reading.Delivery{
		ID:          uuid.NewString(),
		EditionID:   editionID,
		IdentityID:  identity,
		Destination: s.cfg.KindleTo,
		Status:      reading.DeliveryPending,
		CreatedAt:   s.now(),
	}
}

// AddVocabulary adds every 重要語彙 entry of a ready edition to the
// learner's vocabulary list, through the same Ingest path a dictionary
// lookup takes — so the words join the lookup → production tracking the
// rest of JLP already does. Idempotent: each word's client_event_id is
// derived from the edition and the expression, so a second press adds
// nothing. added counts words that were new or newly looked up.
func (s *Service) AddVocabulary(ctx context.Context, identity learner.IdentityID, editionID string) (added int, err error) {
	detail, err := s.readyEdition(ctx, identity, editionID)
	if err != nil {
		return 0, err
	}
	for _, v := range detail.Edition.Lesson.Vocabulary {
		ev := appvocabulary.IngestEvent{
			Type:          "vocabulary.lookup",
			Expression:    v.Expression,
			Reading:       v.Reading,
			Meaning:       v.MeaningEN,
			Example:       v.ExampleJA,
			ClientEventID: "reading:" + editionID + ":" + v.Expression,
		}
		ev.Source.Type = "article"
		ev.Source.Title = detail.Article.Title
		if _, err := s.d.Vocabulary.Ingest(ctx, identity, ev); err != nil {
			return added, fmt.Errorf("reading: add %q to vocabulary: %w", v.Expression, err)
		}
		added++
	}
	return added, nil
}

// ankiSourceType is AnkiCard.SourceType for cards made from a study
// edition's vocabulary; SourceID is "<edition id>:<expression>".
const ankiSourceType = "reading_vocabulary"

// CreateAnkiCards queues one draft Anki card per 重要語彙 entry, in the
// /anki review queue like every other card. Idempotent per word.
func (s *Service) CreateAnkiCards(ctx context.Context, identity learner.IdentityID, editionID string) (int, error) {
	detail, err := s.readyEdition(ctx, identity, editionID)
	if err != nil {
		return 0, err
	}
	drafts := make([]appanki.Draft, 0, len(detail.Edition.Lesson.Vocabulary))
	for _, v := range detail.Edition.Lesson.Vocabulary {
		drafts = append(drafts, appanki.Draft{
			SourceType: ankiSourceType,
			SourceID:   editionID + ":" + v.Expression,
			Front:      v.Expression,
			Back:       ankiBack(v),
			Notes:      "出典: " + detail.Article.Title,
		})
	}
	return s.d.Anki.CreateDraft(ctx, identity, drafts)
}

// ankiBack is the card's answer side: reading, meaning, then the
// example — the same order the EPUB's vocabulary entry uses.
func ankiBack(v reading.VocabularyItem) string {
	var parts []string
	if v.Reading != "" {
		parts = append(parts, v.Reading)
	}
	parts = append(parts, v.MeaningEN)
	if v.ExampleJA != "" {
		ex := v.ExampleJA
		if v.ExampleEN != "" {
			ex += "（" + v.ExampleEN + "）"
		}
		parts = append(parts, ex)
	}
	return strings.Join(parts, "\n")
}

func (s *Service) readyEdition(ctx context.Context, identity learner.IdentityID, editionID string) (EditionDetail, error) {
	e, err := s.d.Repo.GetEdition(ctx, identity, editionID)
	if err != nil {
		return EditionDetail{}, err
	}
	if e.Status != reading.EditionReady || e.Lesson == nil {
		return EditionDetail{}, ErrNotReady
	}
	a, err := s.d.Repo.GetArticle(ctx, identity, e.ArticleID)
	if err != nil {
		return EditionDetail{}, err
	}
	return EditionDetail{Article: a, Edition: e}, nil
}

// Delete soft-deletes an article and with it every edition of it.
// Identity from the request context only; someone else's article is
// ErrNotFound. Nothing is erased, and Restore brings it back.
func (s *Service) Delete(ctx context.Context, identity learner.IdentityID, articleID string) error {
	if err := s.d.Repo.SoftDeleteArticle(ctx, identity, articleID, s.now()); err != nil {
		return err
	}
	if err := s.d.Recorder.RecordDeletion(ctx, identity, learning.KindReading, articleID); err != nil {
		slog.Error("record content.deleted", "identity", identity, "kind", "reading", "subject", articleID, "err", err)
	}
	return nil
}

// Restore undoes Delete.
func (s *Service) Restore(ctx context.Context, identity learner.IdentityID, articleID string) error {
	if err := s.d.Repo.RestoreArticle(ctx, identity, articleID); err != nil {
		return err
	}
	if err := s.d.Recorder.RecordRestore(ctx, identity, learning.KindReading, articleID); err != nil {
		slog.Error("record content.restored", "identity", identity, "kind", "reading", "subject", articleID, "err", err)
	}
	return nil
}

// lessonJSON is the stored form of a lesson.
func lessonJSON(l reading.Lesson) ([]byte, error) { return json.Marshal(l) }
