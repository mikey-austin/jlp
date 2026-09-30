package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/epub"
	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	agentreading "github.com/mikeyaustin/jlp/internal/agent/reading"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appreading "github.com/mikeyaustin/jlp/internal/application/reading"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/publishing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeReadingRepo is an in-memory storage.ReadingRepository with the SQL's
// semantics: content-hash idempotency, claim tokens, in-flight
// delivery uniqueness, soft delete hiding editions.
type fakeReadingRepo struct {
	mu         sync.Mutex
	articles   map[string]*reading.Article
	deleted    map[string]bool
	figures    map[string][]reading.Figure
	editions   map[string]*fakeEditionRow
	deliveries map[string]*fakeDeliveryRow
	drafts     map[learner.IdentityID]*fakeDraft
	nextDraft  int
}

type fakeEditionRow struct {
	reading.StudyEdition
	next    time.Time
	claimed *time.Time
}

type fakeDeliveryRow struct {
	reading.Delivery
	next    time.Time
	claimed *time.Time
}

func newFakeReadingRepo() *fakeReadingRepo {
	return &fakeReadingRepo{articles: map[string]*reading.Article{}, deleted: map[string]bool{}, figures: map[string][]reading.Figure{}, editions: map[string]*fakeEditionRow{}, deliveries: map[string]*fakeDeliveryRow{}}
}

func (m *fakeReadingRepo) SaveTranslation(_ context.Context, id string, t reading.Article) error {
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

func (m *fakeReadingRepo) UpsertArticle(_ context.Context, a reading.Article) (reading.Article, bool, error) {
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

func (m *fakeReadingRepo) GetArticle(_ context.Context, id learner.IdentityID, aid string) (reading.Article, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] {
		return reading.Article{}, storage.ErrNotFound
	}
	return *a, nil
}

func (m *fakeReadingRepo) SoftDeleteArticle(_ context.Context, id learner.IdentityID, aid string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id {
		return storage.ErrNotFound
	}
	m.deleted[aid] = true
	return nil
}

func (m *fakeReadingRepo) RestoreArticle(_ context.Context, id learner.IdentityID, aid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id {
		return storage.ErrNotFound
	}
	delete(m.deleted, aid)
	return nil
}

func (m *fakeReadingRepo) AttachFigures(_ context.Context, id learner.IdentityID, aid string, figs []reading.Figure) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[aid]
	if !ok || a.IdentityID != id || m.deleted[aid] || len(m.figures[aid]) > 0 || len(figs) == 0 {
		return false, nil
	}
	m.figures[aid] = append([]reading.Figure(nil), figs...)
	return true, nil
}

func (m *fakeReadingRepo) ListFigures(_ context.Context, id learner.IdentityID, aid string) ([]reading.Figure, error) {
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

func (m *fakeReadingRepo) FigureData(_ context.Context, id learner.IdentityID, aid string, ordinal int) (reading.Figure, error) {
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

func (m *fakeReadingRepo) InsertEdition(_ context.Context, e reading.StudyEdition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.editions[e.ID] = &fakeEditionRow{StudyEdition: e, next: e.CreatedAt}
	return nil
}

func (m *fakeReadingRepo) sortedEditions() []*fakeEditionRow {
	var out []*fakeEditionRow
	for _, e := range m.editions {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt) || (out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID)
	})
	return out
}

func (m *fakeReadingRepo) LatestEdition(_ context.Context, id learner.IdentityID, aid, pn, pv string) (reading.StudyEdition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *fakeEditionRow
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

func (m *fakeReadingRepo) GetEdition(_ context.Context, id learner.IdentityID, eid string) (reading.StudyEdition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.editions[eid]
	if !ok || e.IdentityID != id || m.deleted[e.ArticleID] {
		return reading.StudyEdition{}, storage.ErrNotFound
	}
	return e.StudyEdition, nil
}

func (m *fakeReadingRepo) ListEditions(_ context.Context, id learner.IdentityID) ([]storage.ReadingEditionSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	latest := map[string]*fakeEditionRow{}
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

func (m *fakeReadingRepo) ClaimDueEdition(_ context.Context, now, stale time.Time) (storage.ClaimedEdition, bool, error) {
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

func (m *fakeReadingRepo) current(c storage.ClaimedEdition) (*fakeEditionRow, error) {
	e, ok := m.editions[c.ID]
	if !ok || e.Status != reading.EditionAnalysing || e.claimed == nil || !e.claimed.Equal(c.ClaimedAt) {
		return nil, storage.ErrNotFound
	}
	return e, nil
}

func (m *fakeReadingRepo) CompleteEdition(_ context.Context, c storage.ClaimedEdition, lesson []byte, reqID string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.current(c)
	if err != nil {
		return err
	}
	var l reading.Lesson
	if err := json.Unmarshal(lesson, &l); err != nil {
		return err
	}
	e.Status, e.Lesson, e.AIRequestID, e.claimed, e.UpdatedAt, e.LastError = reading.EditionReady, &l, reqID, nil, now, ""
	return nil
}

func (m *fakeReadingRepo) FailEditionAttempt(_ context.Context, c storage.ClaimedEdition, msg string, retry bool, at, now time.Time) error {
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

func (m *fakeReadingRepo) QueueDelivery(_ context.Context, d reading.Delivery) (reading.Delivery, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.deliveries {
		if x.EditionID == d.EditionID && x.Destination == d.Destination && (x.Status == reading.DeliveryPending || x.Status == reading.DeliverySending) {
			return x.Delivery, false, nil
		}
	}
	m.deliveries[d.ID] = &fakeDeliveryRow{Delivery: d, next: d.CreatedAt}
	return d, true, nil
}

func (m *fakeReadingRepo) ListDeliveries(_ context.Context, id learner.IdentityID, eid string) ([]reading.Delivery, error) {
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

func (m *fakeReadingRepo) ClaimDueDelivery(_ context.Context, now, stale time.Time) (storage.ClaimedDelivery, bool, error) {
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

func (m *fakeReadingRepo) curDelivery(c storage.ClaimedDelivery) (*fakeDeliveryRow, error) {
	d, ok := m.deliveries[c.ID]
	if !ok || d.Status != reading.DeliverySending || d.claimed == nil || !d.claimed.Equal(c.ClaimedAt) {
		return nil, storage.ErrNotFound
	}
	return d, nil
}

func (m *fakeReadingRepo) MarkDeliverySent(_ context.Context, c storage.ClaimedDelivery, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.curDelivery(c)
	if err != nil {
		return err
	}
	d.Status, d.SentAt, d.claimed = reading.DeliverySent, &at, nil
	return nil
}

func (m *fakeReadingRepo) FailDeliveryAttempt(_ context.Context, c storage.ClaimedDelivery, msg string, retry bool, at time.Time) error {
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

type stubReadingVocab struct{ n int }

func (v *stubReadingVocab) Ingest(_ context.Context, _ learner.IdentityID, ev appvocabulary.IngestEvent) (vocabulary.Item, error) {
	v.n++
	return vocabulary.Item{Expression: ev.Expression}, nil
}
func (*stubReadingVocab) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	return nil, nil
}

type stubReadingAnki struct{}

func (stubReadingAnki) CreateDraft(_ context.Context, _ learner.IdentityID, d []appanki.Draft) (int, error) {
	return len(d), nil
}

type stubDeliverer struct{ sent []publishing.Parcel }

func (s *stubDeliverer) Deliver(_ context.Context, p publishing.Parcel) error {
	s.sent = append(s.sent, p)
	return nil
}

const readingArticle = "政府が発表した新たな経済対策をめぐり、議論が続いている。\n\n中央銀行は金融引き締めを続けている。"

func readingTestServer(t *testing.T, kindle bool) (http.Handler, *appreading.Service, *stubDeliverer) {
	t.Helper()
	d := &stubDeliverer{}
	deps := appreading.Deps{
		Repo:       newFakeReadingRepo(),
		Recorder:   learning.NewRecorder(newFakeEventRepo(), inprocbus.New()),
		Analyser:   agentreading.New(fakeai.New()),
		Translator: agentreading.NewTranslator(fakeai.New()),
		Renderer:   epub.New(),
		Vocabulary: &stubReadingVocab{},
		Anki:       stubReadingAnki{},
	}
	cfg := appreading.Config{}
	if kindle {
		deps.Deliverer, cfg.KindleTo = d, "me@kindle.com"
	}
	svc := appreading.NewService(deps, cfg)
	opts := testOptions()
	opts.Reading = svc
	return NewServer(opts).HandlerForTest(), svc, d
}

func readingPostJSON(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func readingPostForm(h http.Handler, path string, v url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPIReadingSubmitPollDeliver(t *testing.T) {
	h, svc, sender := readingTestServer(t, true)
	body, _ := json.Marshal(map[string]any{
		"url": "https://jp.wsj.com/articles/x", "title": "経済対策", "source": "WSJ日本版",
		"published_at": "2026-09-29T08:00:00+09:00", "content": "ナビゲーション\n" + readingArticle, "selection": readingArticle,
	})
	rec := readingPostJSON(h, "/api/v1/reading/articles", string(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	var sub submitArticleResponseDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Duplicate || sub.Edition.Status != "pending" || sub.Edition.Terminal || !sub.Edition.DeliveryEnabled || sub.Edition.PageURL != "/reading/"+sub.Edition.ID {
		t.Fatalf("submit response = %+v", sub)
	}
	// The selection won over the page content.
	if sub.Article.Chars != len([]rune(strings.ReplaceAll(readingArticle, "\n", ""))) {
		t.Fatalf("chars = %d — selection should win over content", sub.Article.Chars)
	}

	// Same text again: 200 and the same edition.
	if rec := readingPostJSON(h, "/api/v1/reading/articles", string(body)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sub.Edition.ID) {
		t.Fatalf("resubmit = %d %s", rec.Code, rec.Body)
	}

	// Not ready: deliver is a 409.
	if rec := readingPostJSON(h, "/api/v1/reading/editions/"+sub.Edition.ID+"/deliver", ""); rec.Code != http.StatusConflict {
		t.Fatalf("deliver before ready = %d", rec.Code)
	}

	svc.Drain(context.Background())
	rec = get(h, "/api/v1/reading/editions/"+sub.Edition.ID)
	var ed readingEditionDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &ed)
	if rec.Code != http.StatusOK || ed.Status != "ready" || !ed.Terminal || ed.EpubURL == "" || ed.Vocabulary == 0 {
		t.Fatalf("poll = %d %+v", rec.Code, ed)
	}
	if rec := readingPostJSON(h, "/api/v1/reading/editions/"+sub.Edition.ID+"/deliver", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("deliver = %d %s", rec.Code, rec.Body)
	}
	if rec := readingPostJSON(h, "/api/v1/reading/editions/"+sub.Edition.ID+"/deliver", ""); rec.Code != http.StatusOK {
		t.Fatalf("second deliver while in flight = %d, want 200 (the same delivery)", rec.Code)
	}
	svc.Drain(context.Background())
	if len(sender.sent) != 1 {
		t.Fatalf("sent = %d", len(sender.sent))
	}
	if rec := get(h, "/api/v1/reading/editions/does-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown edition = %d", rec.Code)
	}
}

func TestAPIReadingSubmitValidation(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	for _, c := range []struct{ body, want string }{
		{`{"content":"   "}`, "empty"},
		{`{"content":"日本語の本文です。","url":"ftp://x"}`, "url"},
		{`not json`, "invalid request body"},
	} {
		rec := readingPostJSON(h, "/api/v1/reading/articles", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s → %d %s", c.body, rec.Code, rec.Body)
		}
	}
}

func TestAPIReadingDeliverNotConfigured(t *testing.T) {
	h, svc, _ := readingTestServer(t, false)
	rec := readingPostJSON(h, "/api/v1/reading/articles", `{"content":"`+strings.ReplaceAll(readingArticle, "\n", "\\n")+`"}`)
	var sub submitArticleResponseDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &sub)
	svc.Drain(context.Background())
	if rec := readingPostJSON(h, "/api/v1/reading/editions/"+sub.Edition.ID+"/deliver", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("deliver without config = %d", rec.Code)
	}
}

func TestReadingPages(t *testing.T) {
	h, svc, _ := readingTestServer(t, true)
	rec := readingPostForm(h, "/reading", url.Values{"title": {"経済対策"}, "source": {"WSJ日本版"}, "content": {readingArticle}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	page := rec.Header().Get("Location")
	id := strings.TrimPrefix(page, "/reading/")

	// While pending: the page says so and refreshes itself.
	rec = get(h, page)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "作成しています") || !strings.Contains(rec.Body.String(), "location.reload") {
		t.Fatalf("pending page = %d", rec.Code)
	}
	if rec := get(h, page+"/epub"); rec.Code != http.StatusConflict {
		t.Fatalf("epub before ready = %d", rec.Code)
	}

	svc.Drain(context.Background())
	rec = get(h, page)
	body := rec.Body.String()
	for _, want := range []string{"<ruby>金融引き締め<rt>きんゆうひきしめ</rt></ruby>", "重要語彙", "精読", "Kindleに送信", "EPUBをダウンロード"} {
		if !strings.Contains(body, want) {
			t.Fatalf("ready page missing %q", want)
		}
	}
	if strings.Contains(body, "location.reload") {
		t.Fatal("a ready page must not keep refreshing")
	}

	rec = get(h, page+"/epub")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/epub+zip" || !strings.HasPrefix(rec.Body.String(), "PK") {
		t.Fatalf("epub = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".epub") {
		t.Fatalf("content-disposition = %q", cd)
	}

	for path, notice := range map[string]string{"/deliver": "Kindleへの送信を開始しました", "/vocabulary": "語彙リストに", "/anki": "Ankiカードを"} {
		rec := readingPostForm(h, page+path, nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		if got := get(h, rec.Header().Get("Location")).Body.String(); !strings.Contains(got, notice) {
			t.Fatalf("%s: notice %q missing", path, notice)
		}
	}

	// List, then delete and undo.
	if list := get(h, "/reading").Body.String(); !strings.Contains(list, "経済対策") || !strings.Contains(list, "/reading/"+id) {
		t.Fatal("list does not show the article")
	}
	d, _ := svc.Edition(context.Background(), "dev", id)
	rec = readingPostForm(h, "/reading/articles/"+d.Article.ID+"/delete", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d", rec.Code)
	}
	if got := get(h, rec.Header().Get("Location")).Body.String(); !strings.Contains(got, "/reading/articles/"+d.Article.ID+"/restore") {
		t.Fatal("undo affordance missing")
	}
	if rec := get(h, page); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted edition page = %d", rec.Code)
	}
	if rec := readingPostForm(h, "/reading/articles/"+d.Article.ID+"/restore", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("restore = %d", rec.Code)
	}
	if rec := get(h, page); rec.Code != http.StatusOK {
		t.Fatalf("restored edition page = %d", rec.Code)
	}
}

func TestReadingPasteFormShowsValidationError(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	rec := readingPostForm(h, "/reading", url.Values{"content": {"  "}, "title": {"keep me"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "本文が空です") || !strings.Contains(rec.Body.String(), "keep me") {
		t.Fatalf("form error = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Kindleに送信") {
		t.Fatal("the Kindle option must not render when delivery is not configured")
	}
}

// Another identity's edition is a 404 on every route, like every other
// identity-scoped resource.
func TestReadingOtherIdentity(t *testing.T) {
	h, svc, _ := readingTestServer(t, true)
	res, err := svc.Submit(context.Background(), "someone-else", reading.Draft{Content: readingArticle}, appreading.SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc.Drain(context.Background())
	for _, path := range []string{"/reading/" + res.Edition.ID, "/reading/" + res.Edition.ID + "/epub", "/api/v1/reading/editions/" + res.Edition.ID} {
		if rec := get(h, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/reading/" + res.Edition.ID + "/deliver", "/reading/articles/" + res.Article.ID + "/delete"} {
		if rec := readingPostForm(h, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d", path, rec.Code)
		}
	}
}

func TestReadingRoutesAbsentWithoutService(t *testing.T) {
	h := NewServer(testOptions()).HandlerForTest()
	if rec := get(h, "/reading"); rec.Code != http.StatusNotFound {
		t.Fatalf("/reading without a service = %d", rec.Code)
	}
}

// reading:write covers exactly the extension's three reading routes.
func TestReadingScope(t *testing.T) {
	cases := []struct {
		scope, method, path string
		want                int
	}{
		{apitoken.ScopeReadingWrite, http.MethodPost, "/api/v1/reading/articles", http.StatusOK},
		{apitoken.ScopeReadingWrite, http.MethodGet, "/api/v1/reading/editions/abc", http.StatusOK},
		{apitoken.ScopeReadingWrite, http.MethodPost, "/api/v1/reading/editions/abc/deliver", http.StatusOK},
		{apitoken.ScopeSessionsWrite, http.MethodPost, "/api/v1/reading/articles", http.StatusForbidden},
		{apitoken.ScopeReadingWrite, http.MethodPost, "/api/v1/sessions", http.StatusForbidden},
		{apitoken.ScopeReadingWrite, http.MethodGet, "/api/v1/reading/editions/abc/other", http.StatusForbidden},
		{apitoken.ScopeReadingWrite, http.MethodGet, "/api/v1/learner/statistics", http.StatusForbidden},
	}
	for _, c := range cases {
		repo := &tokenRepoStub{hash: apitoken.Hash("jlp_tok"), identity: "dev", scopes: []string{c.scope}}
		mw := APIAuth(apitoken.New(repo), testAuth{id: learner.Identity{ID: "session-user"}}, testIdentityRepo{})
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer jlp_tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s with %s %s = %d, want %d", c.scope, c.method, c.path, rec.Code, c.want)
		}
	}
	var mintable bool
	for _, s := range apitoken.Scopes {
		mintable = mintable || s == apitoken.ScopeReadingWrite
	}
	if !mintable {
		t.Fatal("reading:write must be mintable")
	}
}

func multipartSubmit(t *testing.T, meta string, images ...[]byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("metadata", meta)
	for i, img := range images {
		w, _ := mw.CreateFormFile(fmt.Sprintf("image-%d", i), fmt.Sprintf("f%d", i))
		_, _ = w.Write(img)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reading/articles", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func smallJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	_ = jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil)
	return b.Bytes()
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type figureSubmitOut struct {
	Figures         int `json:"figures"`
	FiguresRejected int `json:"figures_rejected"`
	Article         struct {
		ID string `json:"id"`
	} `json:"article"`
	Edition struct {
		FigureCount int    `json:"figure_count"`
		ID          string `json:"id"`
		ArticleID   string `json:"article_id"`
	} `json:"edition"`
}

func submitFigures(t *testing.T, h http.Handler, meta string, images ...[]byte) figureSubmitOut {
	t.Helper()
	res := serve(h, multipartSubmit(t, meta, images...))
	if res.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", res.Code, res.Body)
	}
	var out figureSubmitOut
	_ = json.Unmarshal(res.Body.Bytes(), &out)
	return out
}

func TestAPIReadingSubmitMultipartAttachesFigures(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	meta := `{"title":"経済対策","content":"政府は新たな経済対策をまとめた。\n\n物価高への対応が柱。",
	  "figures":[{"caption":"会見","after_paragraph":0,"after_text":"政府は","lead":true},{"caption":"壊れた"}]}`
	out := submitFigures(t, h, meta, smallJPEG(t), []byte("broken"))
	if out.Figures != 1 || out.FiguresRejected != 1 || out.Edition.FigureCount != 1 {
		t.Fatalf("response = %+v", out)
	}
}

func TestAPIReadingSubmitJSONStillWorks(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	rec := readingPostJSON(h, "/api/v1/reading/articles", `{"title":"t","content":"政府は新たな経済対策をまとめた。"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestAPIReadingSubmitMultipartTooLargeIs400(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	res := serve(h, multipartSubmit(t, `{"title":"t","content":"本文です。"}`, make([]byte, 16<<20)))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.Code)
	}
}

func TestReadingFigureRoute(t *testing.T) {
	h, svc, _ := readingTestServer(t, false)
	meta := `{"title":"t","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0}]}`
	out := submitFigures(t, h, meta, smallJPEG(t))
	path := "/reading/articles/" + out.Edition.ArticleID + "/figures/0"

	got := get(h, path)
	if got.Code != 200 || got.Header().Get("Content-Type") != "image/jpeg" ||
		!strings.Contains(got.Header().Get("Cache-Control"), "immutable") || got.Header().Get("ETag") == "" {
		t.Fatalf("own figure: %d %v", got.Code, got.Header())
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("If-None-Match", got.Header().Get("ETag"))
	if r := serve(h, req); r.Code != http.StatusNotModified {
		t.Fatalf("revalidation = %d", r.Code)
	}
	if r := get(h, "/reading/articles/"+out.Edition.ArticleID+"/figures/x"); r.Code != http.StatusNotFound {
		t.Fatalf("bad ordinal = %d", r.Code)
	}

	// Another learner's figure is a 404, not a leak.
	res, err := svc.Submit(context.Background(), "someone-else", reading.Draft{
		Content: readingArticle,
		Figures: []reading.FigureDraft{{InText: true, Data: smallJPEG(t)}},
	}, appreading.SubmitOptions{})
	if err != nil || res.Figures != 1 {
		t.Fatalf("foreign submit: %v %+v", err, res)
	}
	if r := get(h, "/reading/articles/"+res.Article.ID+"/figures/0"); r.Code != http.StatusNotFound {
		t.Fatalf("foreign figure = %d, want 404", r.Code)
	}
}

func TestReadingDetailShowsTheOriginalOfATranslatedArticle(t *testing.T) {
	h, svc, _ := readingTestServer(t, false)
	rec := readingPostJSON(h, "/api/v1/reading/articles", `{"title":"Central <bank>","content":"The bank held rates.\n\nMarkets were calm."}`)
	var out struct {
		Edition struct{ ID string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	svc.Drain(context.Background())
	page := get(h, "/reading/"+out.Edition.ID).Body.String()
	for _, want := range []string{"英語から翻訳", "<span class=\"panel__title\">原文</span>", "Central &lt;bank&gt;", "Markets were calm."} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if epubRec := get(h, "/reading/"+out.Edition.ID+"/epub"); epubRec.Code != http.StatusOK {
		t.Fatalf("epub = %d", epubRec.Code)
	}
}

func TestReadingDetailShowsFigures(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	meta := `{"title":"t","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0,"caption":"会見の様子"}]}`
	out := submitFigures(t, h, meta, smallJPEG(t))
	page := get(h, "/reading/"+out.Edition.ID).Body.String()
	if !strings.Contains(page, `/figures/0"`) || !strings.Contains(page, "会見の様子") {
		t.Fatalf("figure not on the page")
	}
}

func TestAPIReadingSubmitSelectionKeepsOnlyCoverFigure(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	meta, _ := json.Marshal(map[string]any{
		"title": "t", "content": "ナビ\n" + readingArticle, "selection": readingArticle,
		"figures": []map[string]any{{"caption": "本文", "after_paragraph": 0}, {"caption": "表紙", "lead": true}},
	})
	out := submitFigures(t, h, string(meta), smallJPEG(t), smallJPEG(t))
	if out.Figures != 1 {
		t.Fatalf("figures = %d, want only the lead", out.Figures)
	}
	page := get(h, "/reading/"+out.Edition.ID).Body.String()
	if strings.Contains(page, "<figure") {
		t.Fatalf("cover-only figure must not appear in the text")
	}
}

func TestReadingFigureRouteSoftDeletedAndOutOfRangeAre404(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	meta := `{"title":"t","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0}]}`
	out := submitFigures(t, h, meta, smallJPEG(t))
	path := "/reading/articles/" + out.Edition.ArticleID + "/figures/0"
	if r := get(h, path); r.Code != http.StatusOK {
		t.Fatalf("before delete = %d", r.Code)
	}
	if r := get(h, "/reading/articles/"+out.Edition.ArticleID+"/figures/5"); r.Code != http.StatusNotFound {
		t.Fatalf("ordinal past the last figure = %d, want 404", r.Code)
	}
	if r := readingPostForm(h, "/reading/articles/"+out.Edition.ArticleID+"/delete", nil); r.Code >= 400 {
		t.Fatalf("delete = %d", r.Code)
	}
	if r := get(h, path); r.Code != http.StatusNotFound {
		t.Fatalf("deleted article's figure = %d, want 404", r.Code)
	}
}

func TestAPIReadingSubmitMultipartOversizeImageRejected(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	big := append(smallJPEG(t), make([]byte, reading.MaxFigureBytes+1)...)[:reading.MaxFigureBytes+1]
	meta := `{"title":"t","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0},{"after_paragraph":0}]}`
	out := submitFigures(t, h, meta, smallJPEG(t), big)
	if out.Figures != 1 || out.FiguresRejected != 1 {
		t.Fatalf("figures=%d rejected=%d, want 1/1", out.Figures, out.FiguresRejected)
	}
}

// --- drafts: pages of blocks, seq = page*10000 + index, one per identity ---

type fakeDraft struct {
	reading.DraftInfo
	blocks []reading.DraftBlock
}

func (m *fakeReadingRepo) draftFor(id learner.IdentityID, did string) (*fakeDraft, bool) {
	d, ok := m.drafts[id]
	if !ok || d.ID != did {
		return nil, false
	}
	return d, true
}

func (m *fakeReadingRepo) AddDraftPage(_ context.Context, id learner.IdentityID, meta reading.DraftMeta, pageURL string, blocks []reading.DraftBlock, now time.Time) (reading.DraftInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(blocks) >= 10000 {
		return reading.DraftInfo{}, fmt.Errorf("draft page has %d blocks", len(blocks))
	}
	if m.drafts == nil {
		m.drafts = map[learner.IdentityID]*fakeDraft{}
	}
	d, existed := m.drafts[id]
	if !existed {
		m.nextDraft++
		d = &fakeDraft{DraftInfo: reading.DraftInfo{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", m.nextDraft), Identity: id, Meta: meta, CreatedAt: now}}
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

func (m *fakeReadingRepo) ActiveDraft(_ context.Context, id learner.IdentityID) (reading.DraftInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.drafts[id]
	if !ok {
		return reading.DraftInfo{}, storage.ErrNotFound
	}
	return d.DraftInfo, nil
}

func (m *fakeReadingRepo) GetDraft(_ context.Context, id learner.IdentityID, did string) (reading.DraftInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.draftFor(id, did)
	if !ok {
		return reading.DraftInfo{}, storage.ErrNotFound
	}
	return d.DraftInfo, nil
}

func (m *fakeReadingRepo) ListDraftBlocks(_ context.Context, id learner.IdentityID, did string, withData bool) ([]reading.DraftBlock, error) {
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

func (m *fakeReadingRepo) DraftImage(_ context.Context, id learner.IdentityID, did string, seq int) (reading.Figure, error) {
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

func (m *fakeReadingRepo) SetDraftBlockExcluded(_ context.Context, id learner.IdentityID, did string, seq int, excluded bool, now time.Time) error {
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

func (m *fakeReadingRepo) SetDraftTitle(_ context.Context, id learner.IdentityID, did, title string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.draftFor(id, did)
	if !ok {
		return storage.ErrNotFound
	}
	d.Meta.Title, d.UpdatedAt = title, now
	return nil
}

func (m *fakeReadingRepo) DeleteDraft(_ context.Context, id learner.IdentityID, did string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.draftFor(id, did); !ok {
		return storage.ErrNotFound
	}
	delete(m.drafts, id)
	return nil
}

func (m *fakeReadingRepo) SweepDrafts(_ context.Context, before time.Time) (int, error) {
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
