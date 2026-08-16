package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cannedAudioQuery is a deliberately minimal stand-in for VOICEVOX's
// real /audio_query response — a large, engine-specific JSON object
// (accent phrases, mora timing, pitch curve, ...) this adapter never
// inspects; it only round-trips it verbatim into /synthesis, exactly
// as VOICEVOX's own documented two-step flow requires. See this
// package's doc comment for where "verified against a real
// voicevox/voicevox_engine container" is documented.
const cannedAudioQuery = `{"accent_phrases":[],"speedScale":1.0,"kana":"コンニチワ"}`

// cannedWAV is a tiny, syntactically-real RIFF/WAVE header (44 bytes,
// zero audio frames) — enough to exercise the adapter's byte/MIME
// pass-through without needing an actual audio payload.
var cannedWAV = []byte{
	'R', 'I', 'F', 'F', 36, 0, 0, 0, 'W', 'A', 'V', 'E',
	'f', 'm', 't', ' ', 16, 0, 0, 0, 1, 0, 1, 0,
	0x80, 0x3e, 0, 0, 0, 0x7d, 0, 0, 2, 0, 16, 0,
	'd', 'a', 't', 'a', 0, 0, 0, 0,
}

type capturedCall struct {
	path        string
	method      string
	query       string
	contentType string
	body        []byte
}

func newTestServer(t *testing.T, calls *[]capturedCall) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("server: read body: %v", err)
		}
		*calls = append(*calls, capturedCall{
			path:        r.URL.Path,
			method:      r.Method,
			query:       r.URL.RawQuery,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})

		switch r.URL.Path {
		case "/audio_query":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cannedAudioQuery))
		case "/synthesis":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(cannedWAV)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSpeakDrivesTheTwoStepVoicevoxFlow(t *testing.T) {
	var calls []capturedCall
	srv := newTestServer(t, &calls)
	defer srv.Close()

	synth := New(srv.URL, 1)
	audio, mime, err := synth.Speak(context.Background(), "こんにちは")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (audio_query then synthesis)", len(calls))
	}
	first, second := calls[0], calls[1]

	if first.method != http.MethodPost || first.path != "/audio_query" {
		t.Errorf("first call = %s %s, want POST /audio_query", first.method, first.path)
	}
	if !strings.Contains(first.query, "text=") || !strings.Contains(first.query, "speaker=1") {
		t.Errorf("first call query = %q, want text= and speaker=1", first.query)
	}

	if second.method != http.MethodPost || second.path != "/synthesis" {
		t.Errorf("second call = %s %s, want POST /synthesis", second.method, second.path)
	}
	if !strings.Contains(second.query, "speaker=1") {
		t.Errorf("second call query = %q, want speaker=1", second.query)
	}
	if !strings.Contains(second.contentType, "application/json") {
		t.Errorf("second call content-type = %q, want application/json", second.contentType)
	}
	// The audio_query response must be forwarded to /synthesis
	// VERBATIM — VOICEVOX's contract is "echo back whatever
	// /audio_query gave you, editing only what you mean to change",
	// and this adapter changes nothing.
	var forwarded map[string]any
	if err := json.Unmarshal(second.body, &forwarded); err != nil {
		t.Fatalf("second call body not JSON: %v", err)
	}
	if forwarded["kana"] != "コンニチワ" {
		t.Errorf("forwarded audio_query kana = %v, want コンニチワ (unmodified passthrough)", forwarded["kana"])
	}

	if string(audio) != string(cannedWAV) {
		t.Errorf("audio bytes did not match the canned WAV response")
	}
	if mime != "audio/wav" {
		t.Errorf("mime = %q, want audio/wav", mime)
	}
}

func TestSpeakWrapsAudioQueryNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "speaker not found", http.StatusBadRequest)
	}))
	defer srv.Close()

	synth := New(srv.URL, 1)
	_, _, err := synth.Speak(context.Background(), "こんにちは")
	if err == nil {
		t.Fatal("Speak: want error on audio_query non-2xx, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want it to mention the 400 status", err)
	}
}

func TestSpeakWrapsSynthesisNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/audio_query" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cannedAudioQuery))
			return
		}
		http.Error(w, "synthesis engine crashed", http.StatusInternalServerError)
	}))
	defer srv.Close()

	synth := New(srv.URL, 1)
	_, _, err := synth.Speak(context.Background(), "こんにちは")
	if err == nil {
		t.Fatal("Speak: want error on synthesis non-2xx, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want it to mention the 500 status", err)
	}
}

func TestSpeakWrapsTransportError(t *testing.T) {
	synth := New("http://127.0.0.1:1", 1) // nothing listens on port 1
	_, _, err := synth.Speak(context.Background(), "こんにちは")
	if err == nil {
		t.Fatal("Speak: want error on unreachable server, got nil")
	}
}
