// words.go implements POST /api/v1/words (Phase 3 Task 8): Nihongo
// Daily's bulk vocabulary ingestion contract, so an external reader
// app can sync its deck into JLP's vocabulary catalog. The contract —
// field names, the 200 (not 201) status, the {"imported":N} response
// shape, and the {"error":"..."} error shape — is implemented verbatim
// from /home/mikey/Workspace/nihongo-daily/doc/openapi.yaml's
// IngestWordsRequest/WordInput/IngestWordsResponse/ErrorResponse
// schemas: this is an external contract, not JLP's own DTO convention
// (contrast vocabularyItemDTO in api.go, which IS JLP's own
// snake_case), so it must never be "improved" to match this file's
// house style.
package httpx

import (
	"encoding/json"
	"errors"
	"net/http"

	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// maxWordsRequestBodyBytes caps POST /api/v1/words' request body —
// larger than the API's default maxRequestBodyBytes (1 MiB, api.go)
// because a legitimate bulk sync from a reader app's whole deck can
// run to hundreds of words with example sentences and tags; 4 MiB
// comfortably covers appvocabulary.Service's 1000-word batch limit
// while still bounding how much an untrusted caller can make the
// server buffer into memory.
const maxWordsRequestBodyBytes = 4 << 20

// wordInputDTO is one entry of the wire "words" array — Nihongo
// Daily's WordInput schema verbatim, including its "kanji" field name
// for what JLP calls Expression everywhere else.
type wordInputDTO struct {
	Kanji     string   `json:"kanji"`
	Reading   string   `json:"reading"`
	Meaning   string   `json:"meaning"`
	MeaningEN string   `json:"meaning_en"`
	JLPTLevel int      `json:"jlpt_level"`
	Tags      []string `json:"tags"`
	Source    string   `json:"source"`
	// Example is the sentence this word was met in — the reason the
	// learner saved it. Optional, and the single most useful optional
	// field here: 練習 drills a word in its own sentence when it has
	// one, and generates a synthetic sentence when it does not. A real
	// one from the learner's own reading is better than anything a model
	// invents, because it carries the context that made the word worth
	// keeping.
	Example string `json:"example"`
}

// ingestWordsRequestDTO is POST /api/v1/words' body — Nihongo Daily's
// IngestWordsRequest schema verbatim.
type ingestWordsRequestDTO struct {
	Words []wordInputDTO `json:"words"`
}

// toWordInput translates one wire wordInputDTO into
// storage.WordInput's own field names.
func toWordInput(dto wordInputDTO) storage.WordInput {
	return storage.WordInput{
		Expression: dto.Kanji,
		Reading:    dto.Reading,
		Meaning:    dto.Meaning,
		MeaningEN:  dto.MeaningEN,
		JLPTLevel:  dto.JLPTLevel,
		Tags:       dto.Tags,
		Source:     dto.Source,
		Example:    dto.Example,
	}
}

// apiWordsIngest handles POST /api/v1/words via
// appvocabulary.Service.IngestWords — the identity-scoped, one-
// transaction, sparse-merge bulk upsert that service documents in
// full; this handler is only decode/validate-error-mapping/encode
// plumbing around it, the same shape every other /api/v1 handler in
// this package follows (see api.go's package doc comment).
func (s *Server) apiWordsIngest(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxWordsRequestBodyBytes)
	var req ingestWordsRequestDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusBadRequest, "request body too large")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "malformed request body")
		return
	}

	words := make([]storage.WordInput, 0, len(req.Words))
	for _, dto := range req.Words {
		words = append(words, toWordInput(dto))
	}

	count, err := s.opts.Vocabulary.IngestWords(r.Context(), ident.ID, words)
	if err != nil {
		var verr *appvocabulary.WordValidationError
		switch {
		case errors.As(err, &verr),
			errors.Is(err, appvocabulary.ErrEmptyWordBatch),
			errors.Is(err, appvocabulary.ErrTooManyWords):
			writeAPIError(w, http.StatusBadRequest, err.Error())
		default:
			writeAPIError(w, http.StatusInternalServerError, "could not ingest words")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]int{"imported": count})
}
