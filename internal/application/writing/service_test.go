package writing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
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

func (f *fakeDocRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error) {
	sk := sessionKey(identity, sid)
	if id, ok := f.bySession[sk]; ok {
		return f.docs[docKey(identity, id)], nil
	}
	f.nextID++
	id := writing.DocumentID(fmt.Sprintf("doc-%d", f.nextID))
	doc := writing.Document{ID: id, SessionID: sid, IdentityID: identity, Content: "", Version: 1}
	f.bySession[sk] = id
	f.docs[docKey(identity, id)] = doc
	return doc, nil
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

func TestOpenCreatesDocumentOnceThenReturnsSame(t *testing.T) {
	svc := appwriting.NewService(newFakeDocRepo())

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
}

func TestAutosaveBumpsVersion(t *testing.T) {
	svc := appwriting.NewService(newFakeDocRepo())

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
}

func TestAutosaveCrossIdentityReturnsErrNotFound(t *testing.T) {
	svc := appwriting.NewService(newFakeDocRepo())

	doc, err := svc.Open(context.Background(), "learner-a", "sess-1")
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}

	_, err = svc.Autosave(context.Background(), "learner-b", doc.ID, "mallory's text")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Autosave (cross-identity) err = %v, want storage.ErrNotFound", err)
	}
}
