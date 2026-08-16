package ai

import "context"

// Transcript is what a SpeechRecognizer returns for one audio clip:
// the recognized text plus the clip's own duration (not the time the
// recognizer took to produce it — see whisper.Recognizer's doc
// comment for where this number comes from). Application code (the
// speech application service) reads DurationMS straight into the
// speech.transcribed event's Evidence — see that event's doc comment
// in domain/event.
type Transcript struct {
	Text       string
	DurationMS int
}

// SpeechRecognizer turns recorded speech into text (Phase 4 Task 8,
// PRD §66): audio is the raw bytes POST /speech/transcribe received
// (a browser MediaRecorder blob, or — per the task's verification
// step — a WAV fixture), mime is that upload's declared Content-Type,
// passed straight through so an implementation can pick the right
// conversion path (or reject a type it can't handle) without this
// package guessing from bytes alone. The one implementation today
// (internal/adapters/whisper) targets a local whisper.cpp server; a
// caller with no STT configured never constructs one at all — see
// application/speech.Service's "recognizer may be nil" contract, the
// same dormant-unless-configured shape as every other optional
// integration in this codebase (config.MQTT.URL, config.Anki.
// ConnectURL, ...).
type SpeechRecognizer interface {
	Transcribe(ctx context.Context, audio []byte, mime string) (Transcript, error)
}

// SpeechSynthesizer turns text into speech: the OTHER half of Task 8's
// port pair, for a future "hear the tutor's reply spoken aloud"
// surface (PRD §66 also motivates a synthesis half, though the task's
// only wired HTTP route is transcription — see internal/adapters/tts's
// own package doc comment for exactly what this implementation is and
// is not connected to yet).
type SpeechSynthesizer interface {
	Speak(ctx context.Context, text string) (audio []byte, mime string, err error)
}
