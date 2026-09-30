package reading

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domain "github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	// TranslatePromptName is also the APP_AI_ROUTES key, e.g.
	// APP_AI_ROUTES=reading.translate=gemini.
	TranslatePromptName    = "reading.translate"
	TranslatePromptVersion = "v1"
	TranslateSchemaName    = "reading_translation.v1"
	// translateMaxTokens: Japanese runs to more tokens than most source
	// languages, and an article can be 20k characters; a translation
	// that stops short is truncated JSON, which fails validation and is
	// retried rather than stored.
	translateMaxTokens = 16384
	// unknownLanguage stands in when the model leaves source_language
	// blank; an empty value would read as "not translated yet".
	unknownLanguage = "外国語"
)

// echoedNumber matches the "[3] " paragraph number a model may copy from
// the numbered input into its output. Only paragraph i's own number is
// stripped (see stripOwnNumber): a genuine leading citation like "[5]"
// belongs to the text.
var echoedNumber = regexp.MustCompile(`^\s*\[(\d+)\]\s*`)

// stripOwnNumber removes the leading "[n]" from paragraph p only when n
// is the paragraph's own 1-based number.
func stripOwnNumber(p string, n int) string {
	m := echoedNumber.FindStringSubmatch(p)
	if m == nil || m[1] != strconv.Itoa(n) {
		return p
	}
	return p[len(m[0]):]
}

// Translator turns a non-Japanese article into Japanese, paragraph for
// paragraph, so the lesson agent can work from Japanese text.
type Translator struct {
	gen ai.StructuredGenerator
}

// NewTranslator returns a translator backed by gen.
func NewTranslator(gen ai.StructuredGenerator) *Translator { return &Translator{gen: gen} }

type translatePromptData struct {
	Title    string
	Count    int
	Numbered string
	Note     string // set on the retry after a wrong paragraph count
}

type translationDoc struct {
	SourceLanguage string   `json:"source_language"`
	Title          string   `json:"title"`
	Paragraphs     []string `json:"paragraphs"`
}

// Translate renders reading.translate, asks gen for a
// reading_translation.v1 document (Rule 4 repair and retry), and checks
// the paragraph count, which the schema cannot express. A wrong count is
// a failed attempt like invalid JSON: the request is sent once more, and
// if the retry is still wrong the error is returned. The translation
// must line up with the original paragraph for paragraph because the
// reader shows them side by side.
func (t *Translator) Translate(ctx context.Context, identity learner.IdentityID, a domain.Article) (domain.Translation, ai.StructuredResponse, error) {
	var numbered strings.Builder
	for i, p := range a.Paragraphs {
		if i > 0 {
			numbered.WriteString("\n\n")
		}
		fmt.Fprintf(&numbered, "[%d] %s", i+1, articleMarkers.Replace(p))
	}
	schema, err := schemas.Get(TranslateSchemaName)
	if err != nil {
		return domain.Translation{}, ai.StructuredResponse{}, fmt.Errorf("reading: get translate schema: %w", err)
	}
	request := func(note string) (ai.StructuredRequest, error) {
		rendered, err := prompts.Render(TranslatePromptName, TranslatePromptVersion, translatePromptData{
			Title:    articleMarkers.Replace(a.Title),
			Count:    len(a.Paragraphs),
			Numbered: numbered.String(),
			Note:     note,
		})
		if err != nil {
			return ai.StructuredRequest{}, fmt.Errorf("reading: render translate prompt: %w", err)
		}
		return ai.StructuredRequest{
			PromptName:    TranslatePromptName,
			PromptVersion: TranslatePromptVersion,
			System:        rendered.System,
			User:          rendered.User,
			SchemaName:    TranslateSchemaName,
			Schema:        schema,
			MaxTokens:     translateMaxTokens,
			IdentityID:    identity,
			Agent:         agentName,
		}, nil
	}

	var (
		resp    ai.StructuredResponse
		lastErr error
	)
	note := ""
	for attempt := 0; attempt < 2; attempt++ {
		req, err := request(note)
		if err != nil {
			return domain.Translation{}, ai.StructuredResponse{}, err
		}
		resp, err = t.gen.GenerateStructured(ctx, req)
		if err != nil {
			return domain.Translation{}, resp, fmt.Errorf("reading: translate: %w", err)
		}
		resp, err = aiutil.ValidateWithRepairAndRetry(ctx, t.gen, TranslateSchemaName, req, resp)
		if err != nil {
			return domain.Translation{}, resp, fmt.Errorf("reading: translate: %w", err)
		}
		var doc translationDoc
		if err := json.Unmarshal(resp.JSON, &doc); err != nil {
			return domain.Translation{}, resp, fmt.Errorf("reading: decode validated translation: %w", err)
		}
		tr := domain.Translation{
			SourceLanguage: strings.TrimSpace(doc.SourceLanguage),
			Title:          strings.TrimSpace(doc.Title),
			Paragraphs:     make([]string, len(doc.Paragraphs)),
		}
		for i, p := range doc.Paragraphs {
			tr.Paragraphs[i] = stripOwnNumber(p, i+1)
		}
		if tr.SourceLanguage == "" {
			tr.SourceLanguage = unknownLanguage
		}
		if _, lastErr = a.WithTranslation(tr); lastErr == nil {
			return tr, resp, nil
		}
		note = fmt.Sprintf("Your previous answer had %d paragraphs. You must return exactly %d, one per numbered input paragraph.", len(tr.Paragraphs), len(a.Paragraphs))
	}
	return domain.Translation{}, resp, fmt.Errorf("reading: translate: %w", lastErr)
}
