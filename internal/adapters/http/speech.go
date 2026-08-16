// speech.go implements POST /speech/transcribe (Phase 4 Task 8, PRD
// §66): decode/encode plumbing around application/speech.Service.
// Transcribe, the same thin-handler shape every other route in this
// package follows. The transcript this returns is NOT fed into the
// conversation pipeline server-side — see application/speech's own
// package doc comment for exactly why that would be the wrong design
// (a second, parallel path into the conversation service instead of
// reusing the one POST /sessions/{id}/conversation already is): the
// browser (record.js) drops the returned text into the conversation
// pane's own input, and a normal form submission through the
// UNCHANGED conversationSay handler is what actually creates a turn.
package httpx

import (
	"errors"
	"io"
	"net/http"

	appspeech "github.com/mikeyaustin/jlp/internal/application/speech"
)

// speechMaxBytes caps POST /speech/transcribe's request body — the
// brief's own "10 MB cap", comfortably covering a spoken sentence or
// two (MediaRecorder's webm/opus encoding runs well under 100 KB/s)
// while still bounding how much an untrusted caller can make the
// server buffer.
const speechMaxBytes = 10 << 20

// speechTranscribeResponse is POST /speech/transcribe's success body —
// the brief's own frozen shape: {"text":"...", "duration_ms":123}.
type speechTranscribeResponse struct {
	Text       string `json:"text"`
	DurationMS int    `json:"duration_ms"`
}

// speechTranscribe handles POST /speech/transcribe: a multipart
// "audio" file field (any Content-Type — internal/adapters/whisper's
// --convert-enabled whisper-server sniffs the real format via ffmpeg
// regardless of what's declared), identity-scoped via the SAME
// RequireIdentity context every other route in this authenticated
// group already carries.
func (s *Server) speechTranscribe(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, speechMaxBytes)
	if err := r.ParseMultipartForm(speechMaxBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "audio must be 10 MB or smaller")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "malformed multipart request")
		return
	}

	file, header, err := r.FormFile("audio")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, `missing "audio" file field`)
		return
	}
	defer func() { _ = file.Close() }()

	// Already bounded by speechMaxBytes twice over — the outer
	// MaxBytesReader on r.Body, and ParseMultipartForm's own maxMemory
	// argument above — so a plain ReadAll here can't buffer more than
	// that regardless of what the client claims header.Size is.
	data, err := io.ReadAll(file)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "could not read audio")
		return
	}

	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}

	t, err := s.opts.Speech.Transcribe(r.Context(), ident.ID, data, mime)
	if err != nil {
		if errors.Is(err, appspeech.ErrNotConfigured) {
			writeAPIError(w, http.StatusServiceUnavailable, "speech recognition is not configured")
			return
		}
		writeAPIError(w, http.StatusBadGateway, "could not transcribe audio")
		return
	}

	writeJSON(w, http.StatusOK, speechTranscribeResponse{Text: t.Text, DurationMS: t.DurationMS})
}
