// Package tts implements ports/ai.SpeechSynthesizer (Phase 4 Task 8,
// PRD §66) against VOICEVOX Engine — a real, working, open-source
// local Japanese text-to-speech server (voicevox/voicevox_engine on
// Docker Hub; see docker-compose.yml's "speech" profile for the exact
// image tag).
//
// This is deliberately NOT the "port + adapter shell, dormant, docs on
// what's missing" treatment the task brief allows (and Phase 4 Task
// 5's README section gave WhatsApp) for a TTS engine that genuinely
// isn't available. That treatment is for when nothing usable exists;
// here something does — VOICEVOX Engine is a real speech synthesizer,
// runs entirely offline once its image is pulled, and this adapter's
// own httptest suite plus a live container this task actually started
// (see README.md's Speech section for the exact commands and output)
// confirm the two-call flow below produces real WAV audio from
// Japanese text. Building a non-functional stub in the presence of a
// working engine would be the dishonest choice, not the cautious one.
//
// It is still dormant by default: config.Speech.TTSURL empty (the
// default — nothing enables it in docker-compose.yml's base `app`
// service, only the "speech" profile's env override) means main.go
// never constructs a Synthesizer at all, matching every other optional
// integration's "empty URL/token ⇒ absent" contract in this codebase.
// No HTTP route calls Speak yet — the task's only wired surface is
// POST /speech/transcribe; wiring a "hear this spoken" button into the
// conversation pane is left to a future task, same as
// internal/adapters/ankiconnect.Client sits unused until
// appanki.Service.SetConnector is called.
//
// Wire contract (VOICEVOX Engine's own documented API, unchanged by
// this adapter): POST {url}/audio_query?text=...&speaker=N returns an
// engine-specific "audio query" JSON object (accent phrases, mora
// timing, pitch/speed parameters) that Speak forwards VERBATIM as the
// body of POST {url}/synthesis?speaker=N, which returns raw WAV bytes.
// speaker is a VOICEVOX "style id" (not a whole character — each
// character exposes several speaking styles under GET /speakers);
// New's caller picks one at construction time since ai.
// SpeechSynthesizer.Speak takes no per-call voice parameter.
package tts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// defaultTimeout bounds each of the two HTTP calls Speak makes.
// VOICEVOX's CPU image can take several seconds per utterance on
// modest hardware — same "generous but bounded" reasoning as
// adapters/whisper.defaultTimeout.
const defaultTimeout = 2 * time.Minute

// Synthesizer implements ai.SpeechSynthesizer against one VOICEVOX
// Engine instance at url, speaking as VOICEVOX style id speaker.
type Synthesizer struct {
	url     string
	speaker int
	client  *http.Client
}

// New returns a Synthesizer targeting url as VOICEVOX style speaker.
// Never fails at construction — dialing only happens on the first
// real Speak call, same never-fails contract as adapters/whisper.New.
func New(url string, speaker int) *Synthesizer {
	return &Synthesizer{url: url, speaker: speaker, client: &http.Client{Timeout: defaultTimeout}}
}

// Speak implements ai.SpeechSynthesizer: audio_query then synthesis,
// as this package's doc comment describes. Errors are always wrapped
// "tts: ...", the same convention adapters/whisper and adapters/ollama
// use for their own wire-level failures.
func (s *Synthesizer) Speak(ctx context.Context, text string) ([]byte, string, error) {
	query := url.Values{
		"text":    {text},
		"speaker": {strconv.Itoa(s.speaker)},
	}

	audioQuery, _, err := s.post(ctx, "/audio_query", query, nil)
	if err != nil {
		return nil, "", fmt.Errorf("tts: audio_query: %w", err)
	}

	wav, mime, err := s.post(ctx, "/synthesis", query, audioQuery)
	if err != nil {
		return nil, "", fmt.Errorf("tts: synthesis: %w", err)
	}
	return wav, mime, nil
}

// post issues one POST {s.url}{path}?{query}, with body as the
// request body (application/json when non-nil — /audio_query's own
// call above passes nil, /synthesis's passes audio_query's response
// verbatim), returning the raw response bytes and its Content-Type on
// 2xx or a status-carrying error otherwise. Shared by both of Speak's
// calls: the request-building/status-check/body-read logic is
// otherwise a verbatim duplicate between them.
func (s *Synthesizer) post(ctx context.Context, path string, query url.Values, body []byte) ([]byte, string, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	u := s.url + path + "?" + query.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, reader)
	if err != nil {
		return nil, "", fmt.Errorf("new request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	httpResp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("status %d: %s", httpResp.StatusCode, raw)
	}

	mime := httpResp.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return raw, mime, nil
}
