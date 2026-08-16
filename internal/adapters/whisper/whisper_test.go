package whisper

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cannedVerboseJSON mirrors whisper.cpp's whisper-server /inference
// response with response_format=verbose_json (confirmed against the
// real ghcr.io/ggml-org/whisper.cpp:main image's whisper-server binary
// during this task's verification — see README.md's Speech section):
// "duration" is the CLIP's length in seconds, not how long inference
// took, which is exactly what ai.Transcript.DurationMS needs.
const cannedVerboseJSON = `{"task":"transcribe","language":"japanese","duration":2.5,"text":"こんにちは\n","segments":[]}`

// capturedRequest records what the adapter actually sent, so tests can
// assert on it after the call completes — mirrors adapters/ollama's
// own requestAssertion test style.
type capturedRequest struct {
	path        string
	method      string
	contentType string
	fields      map[string]string
	fileBytes   []byte
	fileName    string
}

func newTestServer(t *testing.T, captured *capturedRequest, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.method = r.Method
		captured.contentType = r.Header.Get("Content-Type")

		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("server: parse multipart form: %v", err)
		}
		captured.fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				captured.fields[k] = v[0]
			}
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("server: read file field: %v", err)
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("server: read file bytes: %v", err)
		}
		captured.fileBytes = data
		captured.fileName = header.Filename

		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestTranscribeSendsMultipartBodyAndParsesResult(t *testing.T) {
	var captured capturedRequest
	srv := newTestServer(t, &captured, http.StatusOK, cannedVerboseJSON)
	defer srv.Close()

	rec := New(srv.URL)
	audio := []byte("fake webm bytes from MediaRecorder")
	tr, err := rec.Transcribe(context.Background(), audio, "audio/webm")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.path != "/inference" {
		t.Errorf("path = %q, want /inference", captured.path)
	}
	if !strings.HasPrefix(captured.contentType, "multipart/form-data") {
		t.Errorf("content-type = %q, want multipart/form-data", captured.contentType)
	}
	if !bytes.Equal(captured.fileBytes, audio) {
		t.Errorf("uploaded file bytes = %q, want %q", captured.fileBytes, audio)
	}
	if captured.fields["response_format"] != "verbose_json" {
		t.Errorf("response_format field = %q, want verbose_json", captured.fields["response_format"])
	}

	if tr.Text != "こんにちは" {
		t.Errorf("Text = %q, want こんにちは (trimmed)", tr.Text)
	}
	if tr.DurationMS != 2500 {
		t.Errorf("DurationMS = %d, want 2500 (2.5s * 1000)", tr.DurationMS)
	}
}

// TestTranscribeFileNameReflectsMIME pins that the multipart part's
// filename carries a sensible extension for the declared MIME type —
// whisper-server's --convert (ffmpeg) path sniffs content regardless,
// but a correct extension is still what a real MediaRecorder upload
// would send and costs nothing to get right.
func TestTranscribeFileNameReflectsMIME(t *testing.T) {
	var captured capturedRequest
	srv := newTestServer(t, &captured, http.StatusOK, cannedVerboseJSON)
	defer srv.Close()

	rec := New(srv.URL)
	if _, err := rec.Transcribe(context.Background(), []byte("x"), "audio/webm;codecs=opus"); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if !strings.HasSuffix(captured.fileName, ".webm") {
		t.Errorf("fileName = %q, want a .webm suffix for audio/webm", captured.fileName)
	}
}

func TestTranscribeWrapsNon2xxStatus(t *testing.T) {
	var captured capturedRequest
	srv := newTestServer(t, &captured, http.StatusInternalServerError, `{"error":"model not loaded"}`)
	defer srv.Close()

	rec := New(srv.URL)
	_, err := rec.Transcribe(context.Background(), []byte("x"), "audio/wav")
	if err == nil {
		t.Fatal("Transcribe: want error on non-2xx status, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want it to mention the 500 status", err)
	}
}

// TestTranscribeWrapsTransportError covers a server that's simply not
// reachable — dial failure, not an HTTP status — the other failure
// mode Transcribe must wrap rather than panic on.
func TestTranscribeWrapsTransportError(t *testing.T) {
	rec := New("http://127.0.0.1:1") // nothing listens on port 1
	_, err := rec.Transcribe(context.Background(), []byte("x"), "audio/wav")
	if err == nil {
		t.Fatal("Transcribe: want error on unreachable server, got nil")
	}
}

// TestTranscribeWrapsMalformedJSON covers a 2xx response whose body
// isn't valid JSON at all — a whisper-server protocol drift Transcribe
// must report clearly rather than returning a zero-value Transcript
// silently.
func TestTranscribeWrapsMalformedJSON(t *testing.T) {
	var captured capturedRequest
	srv := newTestServer(t, &captured, http.StatusOK, "not json")
	defer srv.Close()

	rec := New(srv.URL)
	_, err := rec.Transcribe(context.Background(), []byte("x"), "audio/wav")
	if err == nil {
		t.Fatal("Transcribe: want error on malformed JSON body, got nil")
	}
}
