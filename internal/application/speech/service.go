// Package speech is the application-layer speech-recognition pipeline
// (Phase 4 Task 8, PRD §66): Service.Transcribe is the ONE place a
// recorded clip becomes text plus a durable speech.transcribed event —
// the HTTP handler (internal/adapters/http/speech.go) is decode/
// encode plumbing around it, the same "thin adapter, real logic here"
// split every other application service in this codebase already
// follows (application/vocabulary.Service.Ingest, application/
// conversation.Service.Say, ...).
//
// This package deliberately does NOT drive the transcript into
// application/conversation.Service.Say itself — pipeline reuse (the
// whole point of Task 8: PRD §66's hypothesis that written formulation
// transfers to speech only holds if speech goes through the exact SAME
// conversation/correction pipeline typed text does) happens one layer
// up, in the browser: record.js drops the returned text into the
// conversation pane's own input and the learner submits it through the
// UNCHANGED POST /sessions/{id}/conversation route — literally the
// same handler, same appconversation.Service.Say call, same
// corrections, same events a typed message gets. Wiring Transcribe to
// call Say directly here would build a SECOND, parallel path into the
// conversation pipeline instead of reusing the one that already
// exists — exactly what the task brief warns against.
//
// Code review Important I1 (post-merge): the FIRST version of this
// package recorded a session-less event with a throwaway random
// Subject, so nothing could ever join a speech.transcribed event back
// to the conversation.turn it became — Task 9's learning-outcome
// analytics landed unable to distinguish spoken from typed production,
// defeating the entire point of sharing one pipeline. Transcribe now
// takes sid (authorized against identity exactly like
// appconversation.Service.Say does) so the event carries a real
// SessionID, and returns Transcript.EventID — the SAME value as the
// recorded event's own ID — so a caller can thread it through to
// Say's sourceEventID parameter and get a real join key, not a
// coincidence of timing.
package speech

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ErrNotConfigured is returned by Transcribe when Service was
// constructed with a nil ai.SpeechRecognizer — main.go only ever
// constructs a real one when config.Speech.STTURL is set (see that
// field's own doc comment), so this is the "dormant unless
// configured" contract's caller-matchable half: the HTTP handler maps
// this, specifically, to a 503 with a clear message, never a panic or
// a silent no-op (the task brief's explicit requirement).
var ErrNotConfigured = errors.New("speech: recognition is not configured")

// ErrEmptyText / ErrTextTooLong are Say's input guards — see sayMaxRunes.
var (
	ErrEmptyText   = errors.New("speech: nothing to say")
	ErrTextTooLong = errors.New("speech: text is too long to synthesize")
)

// Transcript is what Transcribe returns — a package-local copy of
// ports/ai.Transcript's two fields plus EventID (this package's own
// addition, not part of the port), kept separate so this package's
// public API doesn't leak the port type directly (the same "own DTO,
// don't re-export the port's" convention application/conversation.Turn
// follows relative to the domain types it wraps).
type Transcript struct {
	Text       string
	DurationMS int
	// EventID is the recorded speech.transcribed event's own ID — the
	// join key code review Important I1 asked for. A caller threads
	// this to appconversation.Service.Say's sourceEventID parameter
	// (via POST /speech/transcribe's JSON response, then the
	// conversation form's hidden "speech_event_id" field, then
	// conversationSay) so the resulting conversation.turn event can
	// reference exactly which speech.transcribed event produced its
	// text.
	EventID string
}

// Service wraps an ai.SpeechRecognizer with identity scoping and event
// recording. recognizer may be nil — see ErrNotConfigured — exactly
// like application/anki.Service's connector field starts nil until
// SetConnector is called; here there's no setter, since main.go always
// knows at construction time whether config.Speech.STTURL was set.
type Service struct {
	recognizer  ai.SpeechRecognizer
	synthesizer ai.SpeechSynthesizer
	sessions    storage.SessionRepository
	rec         *learning.Recorder
}

// NewService wires the speech pipeline. recognizer nil means STT is
// dormant — every Transcribe call returns ErrNotConfigured and nothing
// is ever recorded.
func NewService(recognizer ai.SpeechRecognizer, synthesizer ai.SpeechSynthesizer, sessions storage.SessionRepository, rec *learning.Recorder) *Service {
	return &Service{recognizer: recognizer, synthesizer: synthesizer, sessions: sessions, rec: rec}
}

// sayMaxRunes caps what Say will synthesize. A drill's word, sentence or
// explanation is short; anything longer is a caller mistake or an
// attempt to make the TTS engine do a lot of work on request, and both
// are better refused than queued.
const sayMaxRunes = 400

// Say turns text into audio, returning the bytes and their MIME type.
//
// Records nothing. A transcription is evidence about the learner — they
// produced that speech — while playback is a UI affordance the learner
// invoked, and logging every tap of a speaker button would bury the
// events that mean something under ones that do not.
//
// Returns ErrNotConfigured when no synthesizer is wired, which is the
// default: APP_SPEECH_TTSURL empty means the whole capability is
// absent, exactly as it does for transcription.
func (s *Service) Say(ctx context.Context, text string) (audio []byte, mime string, err error) {
	if s.synthesizer == nil {
		return nil, "", ErrNotConfigured
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", ErrEmptyText
	}
	if len([]rune(text)) > sayMaxRunes {
		return nil, "", ErrTextTooLong
	}
	return s.synthesizer.Speak(ctx, text)
}

// Transcribe runs audio (mime is its declared Content-Type) through
// the configured recognizer and records ONE speech.transcribed event
// carrying duration/mime/length evidence (see that event Type's own
// doc comment in domain/event) — only once the recognizer has actually
// succeeded, mirroring application/conversation.Service.Say's own
// "record only what really happened" ordering. identity is whichever
// caller's audio this is.
//
// sid is optional (empty means "no session context" — the event is
// recorded session-less, same as before I1): when non-empty it's
// authorized against identity first, s.sessions.Get failing with
// storage.ErrNotFound before the recognizer is ever called or
// anything recorded — the same "wrong identity fails here" contract
// every other identity-scoped service in this codebase leads with
// (see appconversation.Service.Say's own doc comment). Once
// authorized, sid becomes the recorded event's SessionID.
func (s *Service) Transcribe(ctx context.Context, identity learner.IdentityID, sid session.ID, audio []byte, mime string) (Transcript, error) {
	if s.recognizer == nil {
		return Transcript{}, ErrNotConfigured
	}

	var sessionID *session.ID
	if sid != "" {
		if _, err := s.sessions.Get(ctx, identity, sid); err != nil {
			return Transcript{}, err
		}
		sessionID = &sid
	}

	t, err := s.recognizer.Transcribe(ctx, audio, mime)
	if err != nil {
		return Transcript{}, fmt.Errorf("speech: transcribe: %w", err)
	}

	// Generated here (not left for Recorder.Record to fill in) so it
	// can double as both the event's own ID and its Subject, and be
	// returned to the caller as Transcript.EventID — one value, three
	// uses, rather than three independent random strings that happen
	// to coexist.
	eventID := uuid.NewString()
	if err := s.rec.Record(ctx, event.LearningEvent{
		ID:         eventID,
		IdentityID: identity,
		SessionID:  sessionID,
		Type:       event.TypeSpeechTranscribed,
		Subject:    eventID,
		Evidence: map[string]any{
			"duration_ms": t.DurationMS,
			"mime":        mime,
			"chars":       len([]rune(t.Text)),
		},
	}); err != nil {
		return Transcript{}, fmt.Errorf("speech: record %s: %w", event.TypeSpeechTranscribed, err)
	}

	return Transcript{Text: t.Text, DurationMS: t.DurationMS, EventID: eventID}, nil
}
