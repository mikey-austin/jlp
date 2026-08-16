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

func TestTranscribeReturnsErrNotConfiguredWhenRecognizerNil(t *testing.T) {
	store := &fakeEventStore{}
	rec := learning.NewRecorder(store, fakeBus{})
	svc := appspeech.NewService(nil, rec)

	_, err := svc.Transcribe(context.Background(), testIdentity, []byte("audio"), "audio/wav")
	if !errors.Is(err, appspeech.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if len(store.appended) != 0 {
		t.Fatalf("appended = %d events, want 0 (dormant path must not record anything)", len(store.appended))
	}
}

func TestTranscribeSuccessRecordsSpeechTranscribedWithDurationEvidence(t *testing.T) {
	store := &fakeEventStore{}
	rec := learning.NewRecorder(store, fakeBus{})
	fr := &fakeRecognizer{transcript: ai.Transcript{Text: "こんにちは", DurationMS: 1500}}
	svc := appspeech.NewService(fr, rec)

	audio := []byte("raw webm bytes")
	tr, err := svc.Transcribe(context.Background(), testIdentity, audio, "audio/webm")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tr.Text != "こんにちは" || tr.DurationMS != 1500 {
		t.Fatalf("Transcript = %+v, want Text=こんにちは DurationMS=1500", tr)
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
	if ev.SessionID != nil {
		t.Fatalf("event SessionID = %v, want nil (session-less, like vocabulary.imported)", ev.SessionID)
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
		t.Fatal("event Subject must be non-empty (a generated id — see the event's own doc comment)")
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
	svc := appspeech.NewService(fr, rec)

	_, err := svc.Transcribe(context.Background(), testIdentity, []byte("x"), "audio/wav")
	if err == nil {
		t.Fatal("Transcribe: want error when the recognizer fails")
	}
	if len(store.appended) != 0 {
		t.Fatalf("appended = %d events, want 0 when the recognizer itself failed", len(store.appended))
	}
}
