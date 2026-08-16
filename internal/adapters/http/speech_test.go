package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appspeech "github.com/mikeyaustin/jlp/internal/application/speech"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// fakeRecognizer is a canned ai.SpeechRecognizer double for handler
// tests.
type fakeRecognizer struct {
	transcript ai.Transcript
	err        error
}

func (f *fakeRecognizer) Transcribe(context.Context, []byte, string) (ai.Transcript, error) {
	return f.transcript, f.err
}

// buildMultipartAudio builds a multipart/form-data body carrying one
// "audio" file field of exactly n bytes, returning the encoded body
// and its Content-Type header value.
func buildMultipartAudio(t *testing.T, n int) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, err := mw.CreateFormFile("audio", "clip.webm")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte{'a'}, n)); err != nil {
		t.Fatalf("write audio bytes: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf, mw.FormDataContentType()
}

func postSpeechTranscribe(h http.Handler, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/speech/transcribe", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSpeechTranscribeDormantReturns503 pins the brief's dormant
// contract: Options.Speech wraps a nil recognizer (config.Speech.
// STTURL empty), and the route must answer a clear 503, never a panic
// or a silent 200.
func TestSpeechTranscribeDormantReturns503(t *testing.T) {
	opts := testOptions()
	events := newFakeEventRepo()
	opts.Speech = appspeech.NewService(nil, learning.NewRecorder(events, inprocbus.New()))
	h := NewServer(opts).HandlerForTest()

	body, ct := buildMultipartAudio(t, 100)
	rec := postSpeechTranscribe(h, body, ct)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("body = %s, want a clear \"not configured\" message", rec.Body.String())
	}
}

// TestSpeechTranscribeSuccessReturnsTextAndDuration pins the response
// shape the brief specifies: {text, duration_ms}.
func TestSpeechTranscribeSuccessReturnsTextAndDuration(t *testing.T) {
	opts := testOptions()
	events := newFakeEventRepo()
	fr := &fakeRecognizer{transcript: ai.Transcript{Text: "こんにちは", DurationMS: 1200}}
	opts.Speech = appspeech.NewService(fr, learning.NewRecorder(events, inprocbus.New()))
	h := NewServer(opts).HandlerForTest()

	body, ct := buildMultipartAudio(t, 100)
	rec := postSpeechTranscribe(h, body, ct)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Text       string `json:"text"`
		DurationMS int    `json:"duration_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, rec.Body.String())
	}
	if resp.Text != "こんにちは" || resp.DurationMS != 1200 {
		t.Fatalf("response = %+v, want Text=こんにちは DurationMS=1200", resp)
	}

	if len(events.byIdentity["dev"]) != 1 || events.byIdentity["dev"][0].Type != event.TypeSpeechTranscribed {
		t.Fatalf("expected exactly one speech.transcribed event recorded under identity dev, got %+v", events.byIdentity)
	}
}

// TestSpeechTranscribeOversizedBodyReturns413 pins the brief's 10 MB
// cap: a body larger than that must be rejected as too large, not
// buffered into memory in full.
func TestSpeechTranscribeOversizedBodyReturns413(t *testing.T) {
	opts := testOptions()
	events := newFakeEventRepo()
	opts.Speech = appspeech.NewService(&fakeRecognizer{}, learning.NewRecorder(events, inprocbus.New()))
	h := NewServer(opts).HandlerForTest()

	body, ct := buildMultipartAudio(t, 11<<20) // 11 MiB > the 10 MiB cap
	rec := postSpeechTranscribe(h, body, ct)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", rec.Code, rec.Body.String())
	}
}

// TestSpeechTranscribeMissingFileReturns400 covers a malformed
// request — no "audio" field at all — which must fail cleanly rather
// than reach the recognizer with zero bytes.
func TestSpeechTranscribeMissingFileReturns400(t *testing.T) {
	opts := testOptions()
	events := newFakeEventRepo()
	opts.Speech = appspeech.NewService(&fakeRecognizer{}, learning.NewRecorder(events, inprocbus.New()))
	h := NewServer(opts).HandlerForTest()

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField("not_audio", "x"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	rec := postSpeechTranscribe(h, buf, mw.FormDataContentType())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestSpeechTranscribeRecognizerErrorReturns502 covers the recognizer
// (whisper sidecar) itself failing — a distinct failure mode from
// "not configured at all", which must not be conflated with it.
func TestSpeechTranscribeRecognizerErrorReturns502(t *testing.T) {
	opts := testOptions()
	events := newFakeEventRepo()
	fr := &fakeRecognizer{err: context.DeadlineExceeded}
	opts.Speech = appspeech.NewService(fr, learning.NewRecorder(events, inprocbus.New()))
	h := NewServer(opts).HandlerForTest()

	body, ct := buildMultipartAudio(t, 100)
	rec := postSpeechTranscribe(h, body, ct)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
}
