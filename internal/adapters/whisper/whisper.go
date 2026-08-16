// Package whisper implements ports/ai.SpeechRecognizer (Phase 4 Task
// 8, PRD §66) against a local whisper.cpp server's HTTP API —
// specifically the `whisper-server` binary shipped in
// ghcr.io/ggml-org/whisper.cpp's Docker image, run with `--convert` so
// it accepts whatever container format a browser MediaRecorder
// actually produces (webm/opus, typically) rather than requiring raw
// WAV. See docker-compose.yml's "speech" profile (the exact image tag
// and `whisper-server` invocation this adapter is pinned to) and
// README.md's Speech section for how to bring it up and exactly what
// was verified against it during this task — real audio, a real
// container, real inference, not a canned response.
//
// Wire contract: POST {url}/inference, multipart/form-data, field
// "file" carrying the raw audio bytes, field "response_format" set to
// "verbose_json" so the response carries a top-level "duration"
// (SECONDS, the clip's own length — not how long inference took) that
// ai.Transcript.DurationMS needs; whisper-server's plain "json" format
// omits it entirely. Confirmed against the real image: `curl
// $URL/inference -F file=@sample.wav -F response_format=verbose_json`
// returns `{"text":"...","duration":11.0,...}`.
package whisper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// defaultTimeout bounds one /inference round trip. Local CPU inference
// on even a short clip can take several seconds on modest hardware —
// generous, like adapters/ollama's own defaultTimeout, for the same
// "a local model must never hang a request goroutine forever" reason.
const defaultTimeout = 2 * time.Minute

// Recognizer implements ai.SpeechRecognizer against one whisper-server
// instance at url (no trailing slash expected; New does not
// normalize it — docker-compose.yml's APP_SPEECH_STTURL is written
// without one).
type Recognizer struct {
	url    string
	client *http.Client
}

// New returns a Recognizer targeting url. Never fails — like every
// other adapter constructor in this codebase (adapters/ollama.New,
// adapters/ankiconnect.New), dialing only happens on the first real
// Transcribe call, so a misconfigured or not-yet-started sidecar is
// never a boot-time failure.
func New(url string) *Recognizer {
	return &Recognizer{url: url, client: &http.Client{Timeout: defaultTimeout}}
}

// mimeExtensions maps a declared upload Content-Type to the file
// extension the multipart part's filename carries — cosmetic (--convert
// sniffs real content via ffmpeg regardless of extension), but it's
// what a well-formed client actually sends, and costs nothing to get
// right. Falls back to ".bin" for anything unrecognized (extToMIME's
// own doc comment on http.FormFile's server-side counterpart makes the
// same "never let an unmapped MIME crash the request" choice).
var mimeExtensions = map[string]string{
	"audio/webm": ".webm",
	"audio/ogg":  ".ogg",
	"audio/wav":  ".wav",
	"audio/wave": ".wav",
	"audio/mpeg": ".mp3",
	"audio/mp4":  ".m4a",
}

// extensionFor returns mimeExtensions' entry for the base MIME type in
// mime (parameters like ";codecs=opus" are stripped first — a browser
// MediaRecorder blob's Content-Type routinely carries one), or ".bin"
// when the base type isn't in the table.
func extensionFor(mime string) string {
	base, _, _ := strings.Cut(mime, ";")
	base = strings.TrimSpace(base)
	if ext, ok := mimeExtensions[base]; ok {
		return ext
	}
	return ".bin"
}

// inferenceResponse mirrors whisper-server's /inference response body
// with response_format=verbose_json — only the two fields this adapter
// actually reads; whisper-server's real response also carries
// "language" and per-segment timing this package has no use for.
type inferenceResponse struct {
	Text     string  `json:"text"`
	Duration float64 `json:"duration"` // seconds
}

// Transcribe implements ai.SpeechRecognizer. Errors are always wrapped
// with a "whisper: ..." prefix, the same convention adapters/ollama
// uses for its own three failure modes (marshal/transport/non-2xx) —
// application/speech.Service passes these through unchanged; only its
// own ErrNotConfigured (returned when no Recognizer was constructed at
// all) is a caller-matchable sentinel.
func (r *Recognizer) Transcribe(ctx context.Context, audio []byte, mime string) (ai.Transcript, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	fw, err := mw.CreateFormFile("file", "audio"+extensionFor(mime))
	if err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: create form file: %w", err)
	}
	if _, err := fw.Write(audio); err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: write audio bytes: %w", err)
	}
	if err := mw.WriteField("response_format", "verbose_json"); err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: write response_format field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: close multipart writer: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/inference", body)
	if err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())

	httpResp, err := r.client.Do(httpReq)
	if err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: inference: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: read response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return ai.Transcript{}, fmt.Errorf("whisper: inference: status %d: %s", httpResp.StatusCode, raw)
	}

	var resp inferenceResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ai.Transcript{}, fmt.Errorf("whisper: unmarshal response: %w", err)
	}

	return ai.Transcript{
		Text:       strings.TrimSpace(resp.Text),
		DurationMS: int(resp.Duration * 1000),
	}, nil
}
