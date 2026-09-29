package reading

import (
	"errors"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// EditionStatus is where a study edition is in its pipeline.
//
//	pending ──claim──▶ analysing ──ok──▶ ready
//	   ▲                   │
//	   └──retry (backoff)──┤
//	                       └──attempts exhausted──▶ failed
//
// Rendering the EPUB is not a stage: it is a pure function of a ready
// edition, cheap and deterministic, so it runs on demand (download,
// delivery) instead of storing a blob that could drift from the data it
// was made from.
type EditionStatus string

const (
	EditionPending   EditionStatus = "pending"
	EditionAnalysing EditionStatus = "analysing"
	EditionReady     EditionStatus = "ready"
	EditionFailed    EditionStatus = "failed"
)

// MaxAnalysisAttempts bounds how often one edition's analysis is tried
// before it is marked failed. Each attempt already includes
// aiutil.ValidateWithRepairAndRetry's own repair-and-retry, so three
// attempts can be up to six model calls — enough to ride out a flaky
// provider, not enough to run up a bill on an article the model cannot
// handle.
const MaxAnalysisAttempts = 3

// RetryDelay is the backoff before attempt n+1 (n = attempts so far).
func RetryDelay(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return time.Minute
	case attempts == 2:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// StudyEdition is one analysis of an Article. Lesson is nil until the
// edition is ready.
type StudyEdition struct {
	ID            string
	ArticleID     string
	IdentityID    learner.IdentityID
	Status        EditionStatus
	PromptName    string
	PromptVersion string
	SchemaName    string
	Lesson        *Lesson
	AIRequestID   string
	Attempts      int
	LastError     string
	// DeliverWhenReady asks the pipeline to queue a Kindle delivery the
	// moment analysis succeeds — the extension's "send to Kindle"
	// checkbox. Ignored when Kindle delivery is not configured.
	DeliverWhenReady bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Terminal reports whether the pipeline will do no more work on e.
func (e StudyEdition) Terminal() bool {
	return e.Status == EditionReady || e.Status == EditionFailed
}

// DeliveryStatus is where one Send-to-Kindle attempt is.
type DeliveryStatus string

const (
	DeliveryPending DeliveryStatus = "pending"
	DeliverySending DeliveryStatus = "sending"
	DeliverySent    DeliveryStatus = "sent"
	DeliveryFailed  DeliveryStatus = "failed"
)

// MaxDeliveryAttempts bounds SMTP retries for one delivery.
const MaxDeliveryAttempts = 3

// Delivery is one request to send an edition's EPUB to a destination
// (a Send-to-Kindle address). At most one delivery per edition and
// destination can be in flight at a time — the storage layer enforces
// it — so a double click never mails the same book twice.
type Delivery struct {
	ID          string
	EditionID   string
	IdentityID  learner.IdentityID
	Destination string
	Status      DeliveryStatus
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	SentAt      *time.Time
}

// Lesson is a validated study_edition.v1 document, field for field. The
// JSON tags ARE the schema's property names: adapters/http and the EPUB
// renderer decode stored editions straight into this type, so a field
// renamed here and not in the schema fails the schema test rather than
// silently rendering blank.
type Lesson struct {
	SummaryEN        string             `json:"summary_en"`
	SummaryJA        string             `json:"summary_ja"`
	Level            string             `json:"level"`
	Vocabulary       []VocabularyItem   `json:"vocabulary"`
	Grammar          []GrammarPoint     `json:"grammar"`
	SentenceAnalyses []SentenceAnalysis `json:"sentence_analyses"`
	Review           Review             `json:"review"`
}

// VocabularyItem is one 重要語彙 entry.
type VocabularyItem struct {
	Expression string `json:"expression"`
	Reading    string `json:"reading"`
	MeaningEN  string `json:"meaning_en"`
	UsageEN    string `json:"usage_en"`
	ExampleJA  string `json:"example_ja"`
	ExampleEN  string `json:"example_en"`
}

// GrammarPoint is one 表現・文法 entry: a construction as it appears in
// the article, explained, with a fresh example.
type GrammarPoint struct {
	Pattern       string `json:"pattern"`
	MeaningEN     string `json:"meaning_en"`
	ExplanationEN string `json:"explanation_en"`
	FromArticle   string `json:"from_article"`
	ExampleJA     string `json:"example_ja"`
	ExampleEN     string `json:"example_en"`
}

// SentenceAnalysis is one 精読 entry: a difficult sentence from the
// article, broken into its structural chunks.
type SentenceAnalysis struct {
	Sentence      string  `json:"sentence"`
	TranslationEN string  `json:"translation_en"`
	Chunks        []Chunk `json:"chunks"`
	NoteEN        string  `json:"note_en"`
}

// Chunk is one structural piece of an analysed sentence: the text, its
// reading when it contains kanji, and the role it plays ("relative
// clause modifying 対策", "topic", …).
type Chunk struct {
	Text    string `json:"text"`
	Reading string `json:"reading"`
	RoleEN  string `json:"role_en"`
}

// Review is the 復習 section.
type Review struct {
	Comprehension []QA `json:"comprehension"`
	Vocabulary    []QA `json:"vocabulary"`
}

// QA is one review question with its answer. The EPUB puts answers in
// their own section at the back, so reading the question does not show
// you the answer.
type QA struct {
	QuestionJA string `json:"question_ja"`
	AnswerJA   string `json:"answer_ja"`
}

// ErrEmptyLesson is returned by Normalise when nothing usable is left —
// a response that satisfied the schema but carries no vocabulary at all
// is not a lesson, and is treated as a failed attempt rather than
// shipped to a Kindle.
var ErrEmptyLesson = errors.New("reading: analysis contains no vocabulary")

// Normalise is the semantic validation the JSON Schema cannot express:
// strings trimmed, entries with no headword dropped, duplicate
// vocabulary (the same expression listed twice) collapsed, and chunks
// with no text removed. It runs after schema validation and before
// anything is persisted, so the renderer and the pages only ever see
// clean data.
func (l *Lesson) Normalise() error {
	l.SummaryEN = strings.TrimSpace(l.SummaryEN)
	l.SummaryJA = strings.TrimSpace(l.SummaryJA)
	l.Level = strings.TrimSpace(l.Level)

	seen := map[string]bool{}
	vocab := l.Vocabulary[:0]
	for _, v := range l.Vocabulary {
		v.Expression = strings.TrimSpace(v.Expression)
		v.Reading = strings.TrimSpace(v.Reading)
		v.MeaningEN = strings.TrimSpace(v.MeaningEN)
		v.UsageEN = strings.TrimSpace(v.UsageEN)
		v.ExampleJA = strings.TrimSpace(v.ExampleJA)
		v.ExampleEN = strings.TrimSpace(v.ExampleEN)
		if v.Expression == "" || v.MeaningEN == "" || seen[v.Expression] {
			continue
		}
		seen[v.Expression] = true
		// A reading identical to the headword (a kana-only word) adds
		// nothing and would render as ruby over itself.
		if v.Reading == v.Expression {
			v.Reading = ""
		}
		vocab = append(vocab, v)
	}
	l.Vocabulary = vocab
	if len(l.Vocabulary) == 0 {
		return ErrEmptyLesson
	}

	grammar := l.Grammar[:0]
	for _, g := range l.Grammar {
		g.Pattern = strings.TrimSpace(g.Pattern)
		g.MeaningEN = strings.TrimSpace(g.MeaningEN)
		g.ExplanationEN = strings.TrimSpace(g.ExplanationEN)
		g.FromArticle = strings.TrimSpace(g.FromArticle)
		g.ExampleJA = strings.TrimSpace(g.ExampleJA)
		g.ExampleEN = strings.TrimSpace(g.ExampleEN)
		if g.Pattern == "" {
			continue
		}
		grammar = append(grammar, g)
	}
	l.Grammar = grammar

	sentences := l.SentenceAnalyses[:0]
	for _, s := range l.SentenceAnalyses {
		s.Sentence = strings.TrimSpace(s.Sentence)
		s.TranslationEN = strings.TrimSpace(s.TranslationEN)
		s.NoteEN = strings.TrimSpace(s.NoteEN)
		chunks := s.Chunks[:0]
		for _, c := range s.Chunks {
			c.Text = strings.TrimSpace(c.Text)
			c.Reading = strings.TrimSpace(c.Reading)
			c.RoleEN = strings.TrimSpace(c.RoleEN)
			if c.Text == "" {
				continue
			}
			if c.Reading == c.Text {
				c.Reading = ""
			}
			chunks = append(chunks, c)
		}
		s.Chunks = chunks
		if s.Sentence == "" {
			continue
		}
		sentences = append(sentences, s)
	}
	l.SentenceAnalyses = sentences

	l.Review.Comprehension = cleanQA(l.Review.Comprehension)
	l.Review.Vocabulary = cleanQA(l.Review.Vocabulary)
	return nil
}

func cleanQA(in []QA) []QA {
	out := in[:0]
	for _, q := range in {
		q.QuestionJA = strings.TrimSpace(q.QuestionJA)
		q.AnswerJA = strings.TrimSpace(q.AnswerJA)
		if q.QuestionJA == "" {
			continue
		}
		out = append(out, q)
	}
	return out
}
