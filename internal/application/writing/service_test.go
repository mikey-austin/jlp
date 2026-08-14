package writing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // inprocbus is a port-shaped test double injected via learning.NewRecorder(..., events.EventBus); PRD §75 forbids application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeDocRepo is an in-memory storage.DocumentRepository, identity-scoped
// like the real adapter: every key is identity+"/"+id (or +session id for
// the by-session lookup), so cross-identity access simply misses.
type fakeDocRepo struct {
	bySession map[string]writing.DocumentID
	docs      map[string]writing.Document
	versions  map[string][]writing.Document
	nextID    int
}

func newFakeDocRepo() *fakeDocRepo {
	return &fakeDocRepo{
		bySession: map[string]writing.DocumentID{},
		docs:      map[string]writing.Document{},
		versions:  map[string][]writing.Document{},
	}
}

func docKey(identity learner.IdentityID, id writing.DocumentID) string {
	return string(identity) + "/" + string(id)
}

func sessionKey(identity learner.IdentityID, sid session.ID) string {
	return string(identity) + "/" + string(sid)
}

func (f *fakeDocRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, bool, error) {
	sk := sessionKey(identity, sid)
	if id, ok := f.bySession[sk]; ok {
		return f.docs[docKey(identity, id)], false, nil
	}
	f.nextID++
	id := writing.DocumentID(fmt.Sprintf("doc-%d", f.nextID))
	doc := writing.Document{ID: id, SessionID: sid, IdentityID: identity, Content: "", Version: 1}
	f.bySession[sk] = id
	f.docs[docKey(identity, id)] = doc
	return doc, true, nil
}

func (f *fakeDocRepo) Save(_ context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	k := docKey(identity, id)
	doc, ok := f.docs[k]
	if !ok {
		return writing.Document{}, storage.ErrNotFound
	}
	doc.Content = content
	doc.Version++
	f.docs[k] = doc
	f.versions[k] = append(f.versions[k], doc)
	return doc, nil
}

func (f *fakeDocRepo) Get(_ context.Context, identity learner.IdentityID, id writing.DocumentID) (writing.Document, error) {
	doc, ok := f.docs[docKey(identity, id)]
	if !ok {
		return writing.Document{}, storage.ErrNotFound
	}
	return doc, nil
}

func (f *fakeDocRepo) ListVersions(_ context.Context, identity learner.IdentityID, id writing.DocumentID, limit int) ([]writing.Document, error) {
	vs := f.versions[docKey(identity, id)]
	if limit > 0 && limit < len(vs) {
		vs = vs[len(vs)-limit:]
	}
	return vs, nil
}

// fakeEventStore is an in-memory storage.LearningEventRepository used to
// assert which events the writing service records, without depending on
// postgres. appendErr, when set, simulates the event store being down so
// tests can assert Open/Autosave still succeed for the caller.
type fakeEventStore struct {
	events    []event.LearningEvent
	appendErr error
}

func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return f.events, nil
}

// newTestRecorder builds a real learning.Recorder over a fake store and a
// real in-process bus, so recording behavior (append-then-publish, ID/time
// defaulting) is exercised honestly rather than stubbed out.
func newTestRecorder() (*learning.Recorder, *fakeEventStore) {
	store := &fakeEventStore{}
	return learning.NewRecorder(store, inprocbus.New()), store
}

func TestOpenCreatesDocumentOnceThenReturnsSame(t *testing.T) {
	rec, store := newTestRecorder()
	svc := appwriting.NewService(newFakeDocRepo(), rec)

	first, err := svc.Open(context.Background(), "learner-a", "sess-1")
	if err != nil {
		t.Fatalf("Open (first) returned error: %v", err)
	}
	if first.ID == "" {
		t.Fatal("expected a generated document ID")
	}
	if first.Version != 1 {
		t.Fatalf("Version = %d, want 1 for a fresh document", first.Version)
	}

	second, err := svc.Open(context.Background(), "learner-a", "sess-1")
	if err != nil {
		t.Fatalf("Open (second) returned error: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("Open returned a different document on second call: %q != %q", second.ID, first.ID)
	}

	// writing.created is recorded exactly once, on the call that created
	// the document — not on the second, GetOrCreateForSession-hits-cache
	// call.
	var created []event.LearningEvent
	for _, ev := range store.events {
		if ev.Type == event.TypeWritingCreated {
			created = append(created, ev)
		}
	}
	if len(created) != 1 {
		t.Fatalf("recorded %d writing.created events, want 1: %+v", len(created), store.events)
	}
	ev := created[0]
	if ev.IdentityID != "learner-a" {
		t.Fatalf("writing.created IdentityID = %q, want learner-a", ev.IdentityID)
	}
	if ev.SessionID == nil || *ev.SessionID != session.ID("sess-1") {
		t.Fatalf("writing.created SessionID = %v, want sess-1", ev.SessionID)
	}
	if ev.Subject != string(first.ID) {
		t.Fatalf("writing.created Subject = %q, want %q", ev.Subject, first.ID)
	}
	if ev.ID == "" || ev.OccurredAt.IsZero() {
		t.Fatalf("writing.created event missing ID/OccurredAt: %+v", ev)
	}
}

func TestAutosaveBumpsVersion(t *testing.T) {
	rec, store := newTestRecorder()
	svc := appwriting.NewService(newFakeDocRepo(), rec)

	doc, err := svc.Open(context.Background(), "learner-a", "sess-1")
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}

	saved, err := svc.Autosave(context.Background(), "learner-a", doc.ID, "昨日、映画を見た。")
	if err != nil {
		t.Fatalf("Autosave returned error: %v", err)
	}
	if saved.Version != doc.Version+1 {
		t.Fatalf("Version = %d, want %d", saved.Version, doc.Version+1)
	}
	if saved.Content != "昨日、映画を見た。" {
		t.Fatalf("Content = %q, want the saved text", saved.Content)
	}

	var updated []event.LearningEvent
	for _, ev := range store.events {
		if ev.Type == event.TypeWritingUpdated {
			updated = append(updated, ev)
		}
	}
	if len(updated) != 1 {
		t.Fatalf("recorded %d writing.updated events, want 1: %+v", len(updated), store.events)
	}
	ev := updated[0]
	if ev.Subject != string(doc.ID) {
		t.Fatalf("writing.updated Subject = %q, want %q", ev.Subject, doc.ID)
	}
	if got := ev.Evidence["rune_count"]; got != saved.RuneCount() {
		t.Fatalf("writing.updated Evidence[rune_count] = %v, want %d", got, saved.RuneCount())
	}
	if got := ev.Evidence["version"]; got != saved.Version {
		t.Fatalf("writing.updated Evidence[version] = %v, want %d", got, saved.Version)
	}
}

func TestAutosaveCrossIdentityReturnsErrNotFound(t *testing.T) {
	rec, store := newTestRecorder()
	svc := appwriting.NewService(newFakeDocRepo(), rec)

	doc, err := svc.Open(context.Background(), "learner-a", "sess-1")
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	baseline := len(store.events)

	_, err = svc.Autosave(context.Background(), "learner-b", doc.ID, "mallory's text")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Autosave (cross-identity) err = %v, want storage.ErrNotFound", err)
	}
	if len(store.events) != baseline {
		t.Fatalf("a failed Autosave recorded %d new events, want 0", len(store.events)-baseline)
	}
}

// TestOpenAndAutosaveSucceedEvenWhenEventRecordingFails asserts the
// controller decision that a learning-event recording failure must never
// mask a successful write: the document was created/saved regardless of
// whether the event ledger could be appended to, so Open and Autosave
// must still return the document with a nil error. Event loss here is an
// observability concern (logged inside the service), not a save failure.
func TestOpenAndAutosaveSucceedEvenWhenEventRecordingFails(t *testing.T) {
	store := &fakeEventStore{appendErr: errors.New("event store down")}
	rec := learning.NewRecorder(store, inprocbus.New())
	svc := appwriting.NewService(newFakeDocRepo(), rec)

	doc, err := svc.Open(context.Background(), "learner-a", "sess-1")
	if err != nil {
		t.Fatalf("Open returned error even though the document was created: %v", err)
	}
	if doc.ID == "" {
		t.Fatal("Open did not return the created document")
	}

	saved, err := svc.Autosave(context.Background(), "learner-a", doc.ID, "昨日、映画を見た。")
	if err != nil {
		t.Fatalf("Autosave returned error even though the write succeeded: %v", err)
	}
	if saved.Content != "昨日、映画を見た。" {
		t.Fatalf("Autosave did not return the saved content: got %q", saved.Content)
	}
	if saved.Version != doc.Version+1 {
		t.Fatalf("Autosave did not return the bumped version: got %d, want %d", saved.Version, doc.Version+1)
	}
	// No events made it into the store — the point is that this doesn't
	// leak into the caller's result.
	if len(store.events) != 0 {
		t.Fatalf("appendErr store somehow recorded %d events, want 0", len(store.events))
	}
}
