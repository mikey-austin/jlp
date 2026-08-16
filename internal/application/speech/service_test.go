package speech_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/learning"
	appspeech "github.com/mikeyaustin/jlp/internal/application/speech"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/events"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// --- in-memory fakes, mirroring application/learning/recorder_test.go's
// own fakeEventStore/fakeBus (this package's peer, not a real adapter). ---

type fakeEventStore struct {
	appended []event.LearningEvent
}

func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	f.appended = append(f.appended, ev)
	return nil
}
func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return f.appended, nil
}
func (f *fakeEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	return f.appended, nil
}

type fakeBus struct{}

func (fakeBus) Publish(context.Context, event.LearningEvent) error { return nil }
func (fakeBus) Subscribe(event.Type, events.Handler)               {}

// fakeSessionRepo mirrors application/conversation's own test double:
// a session belongs to whichever identity created it, and Get refuses
// any other identity with storage.ErrNotFound.
type fakeSessionRepo struct {
	byKey map[string]learner.IdentityID
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byKey: map[string]learner.IdentityID{}}
}

func (f *fakeSessionRepo) seed(identity learner.IdentityID, sid session.ID) {
	f.byKey[string(sid)] = identity
}

func (f *fakeSessionRepo) Create(context.Context, session.Session) error { panic("not used") }

func (f *fakeSessionRepo) Get(_ context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	owner, ok := f.byKey[string(id)]
	if !ok || owner != identity {
		return session.Session{}, storage.ErrNotFound
	}
	return session.Session{ID: id, IdentityID: identity}, nil
}

func (f *fakeSessionRepo) List(context.Context, learner.IdentityID) ([]session.Session, error) {
	panic("not used")
}

// fakeRecognizer is a canned ai.SpeechRecognizer double.
type fakeRecognizer struct {
	transcript ai.Transcript
	err        error
	gotAudio   []byte
	gotMIME    string
}

func (f *fakeRecognizer) Transcribe(_ context.Context, audio []byte, mime string) (ai.Transcript, error) {
	f.gotAudio = audio
	f.gotMIME = mime
	return f.transcript, f.err
}

const testIdentity learner.IdentityID = "learner-a"
const testSessionID session.ID = "sess-1"

func TestTranscribeReturnsErrNotConfiguredWhenRecognizerNil(t *testing.T) {
	store := &fakeEventStore{}
	rec := learning.NewRecorder(store, fakeBus{})
	svc := appspeech.NewService(nil, newFakeSessionRepo(), rec)

	_, err := svc.Transcribe(context.Background(), testIdentity, "", []byte("audio"), "audio/wav")
	if !errors.Is(err, appspeech.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if len(store.appended) != 0 {
		t.Fatalf("appended = %d events, want 0 (dormant path must not record anything)", len(store.appended))
	}
}

// TestTranscribeSessionLessWhenSIDEmpty pins backward-compatible
// behaviour: an empty sid (the caller has no session context) skips
// authorization entirely and records a session-less event, exactly
// like the original Task 8 contract before code review Important I1.
func TestTranscribeSessionLessWhenSIDEmpty(t *testing.T) {
	store := &fakeEventStore{}
	rec := learning.NewRecorder(store, fakeBus{})
	fr := &fakeRecognizer{transcript: ai.Transcript{Text: "こんにちは", DurationMS: 1500}}
	svc := appspeech.NewService(fr, newFakeSessionRepo(), rec)

	tr, err := svc.Transcribe(context.Background(), testIdentity, "", []byte("x"), "audio/webm")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tr.Text != "こんにちは" || tr.DurationMS != 1500 {
		t.Fatalf("Transcript = %+v, want Text=こんにちは DurationMS=1500", tr)
	}
	if tr.EventID == "" {
		t.Fatal("Transcript.EventID must be non-empty — it's the join key a caller threads to conversation.Service.Say")
	}

	if len(store.appended) != 1 {
		t.Fatalf("appended = %d events, want exactly 1", len(store.appended))
	}
	ev := store.appended[0]
	if ev.SessionID != nil {
		t.Fatalf("event SessionID = %v, want nil for an empty sid", ev.SessionID)
	}
	if ev.ID != tr.EventID {
		t.Fatalf("event ID = %q, want it to equal the returned Transcript.EventID %q", ev.ID, tr.EventID)
	}
}

// TestTranscribeSetsSessionIDAndJoinableEventID is code review Important
// I1's pin: a non-empty sid is authorized against identity (like every
// other identity-scoped service in this codebase) and threaded onto
// the recorded event's SessionID, and the returned Transcript.EventID
// matches the event's own ID exactly — the key a caller (the HTTP
// handler, then the conversation form's hidden field, then
// conversation.Service.Say) can use to join a speech.transcribed event
// to the conversation.turn it becomes.
func TestTranscribeSetsSessionIDAndJoinableEventID(t *testing.T) {
	store := &fakeEventStore{}
	rec := learning.NewRecorder(store, fakeBus{})
	fr := &fakeRecognizer{transcript: ai.Transcript{Text: "こんにちは", DurationMS: 1500}}
	sessions := newFakeSessionRepo()
	sessions.seed(testIdentity, testSessionID)
	svc := appspeech.NewService(fr, sessions, rec)

	audio := []byte("raw webm bytes")
	tr, err := svc.Transcribe(context.Background(), testIdentity, testSessionID, audio, "audio/webm")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	// The recognizer must see exactly what the caller passed in.
	if string(fr.gotAudio) != string(audio) || fr.gotMIME != "audio/webm" {
		t.Fatalf("recognizer got audio=%q mime=%q, want the original upload unmodified", fr.gotAudio, fr.gotMIME)
	}

	if len(store.appended) != 1 {
		t.Fatalf("appended = %d events, want exactly 1", len(store.appended))
	}
	ev := store.appended[0]
	if ev.Type != event.TypeSpeechTranscribed {
		t.Fatalf("event type = %q, want %q", ev.Type, event.TypeSpeechTranscribed)
	}
	if ev.IdentityID != testIdentity {
		t.Fatalf("event IdentityID = %q, want %q", ev.IdentityID, testIdentity)
	}
	if ev.SessionID == nil || *ev.SessionID != testSessionID {
		t.Fatalf("event SessionID = %v, want &testSessionID", ev.SessionID)
	}
	if ev.Evidence["duration_ms"] != 1500 {
		t.Fatalf("Evidence[duration_ms] = %v, want 1500", ev.Evidence["duration_ms"])
	}
	if ev.Evidence["mime"] != "audio/webm" {
		t.Fatalf("Evidence[mime] = %v, want audio/webm", ev.Evidence["mime"])
	}
	if ev.Evidence["chars"] != len([]rune("こんにちは")) {
		t.Fatalf("Evidence[chars] = %v, want %d", ev.Evidence["chars"], len([]rune("こんにちは")))
	}
	if ev.Subject == "" {
		t.Fatal("event Subject must be non-empty")
	}
	if tr.EventID == "" || tr.EventID != ev.ID {
		t.Fatalf("Transcript.EventID = %q, want it to equal the recorded event's own ID %q", tr.EventID, ev.ID)
	}
}

// TestTranscribeRejectsSessionNotOwnedByIdentity pins the
// authorization check: a sid belonging to a DIFFERENT identity must
// fail with storage.ErrNotFound, before the recognizer is ever called
// or any event recorded — the same "wrong identity fails here" contract
// application/conversation.Service.Say's own doc comment describes.
func TestTranscribeRejectsSessionNotOwnedByIdentity(t *testing.T) {
	store := &fakeEventStore{}
	rec := learning.NewRecorder(store, fakeBus{})
	fr := &fakeRecognizer{transcript: ai.Transcript{Text: "x", DurationMS: 100}}
	sessions := newFakeSessionRepo()
	sessions.seed("someone-else", testSessionID)
	svc := appspeech.NewService(fr, sessions, rec)

	_, err := svc.Transcribe(context.Background(), testIdentity, testSessionID, []byte("x"), "audio/wav")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
	if fr.gotAudio != nil {
		t.Fatal("recognizer must not be called when session authorization fails")
	}
	if len(store.appended) != 0 {
		t.Fatalf("appended = %d events, want 0 when session authorization fails", len(store.appended))
	}
}

// TestTranscribeWrapsRecognizerErrorAndRecordsNothing pins the "an
// event only exists for a transcript that actually happened" rule —
// mirrors application/conversation.Service's own error-before-any-
// event-recorded ordering.
func TestTranscribeWrapsRecognizerErrorAndRecordsNothing(t *testing.T) {
	store := &fakeEventStore{}
	rec := learning.NewRecorder(store, fakeBus{})
	fr := &fakeRecognizer{err: errors.New("whisper: inference: status 500")}
	svc := appspeech.NewService(fr, newFakeSessionRepo(), rec)

	_, err := svc.Transcribe(context.Background(), testIdentity, "", []byte("x"), "audio/wav")
	if err == nil {
		t.Fatal("Transcribe: want error when the recognizer fails")
	}
	if len(store.appended) != 0 {
		t.Fatalf("appended = %d events, want 0 when the recognizer itself failed", len(store.appended))
	}
}
