// Package fakeai is a deterministic ai.StructuredGenerator: no network
// calls, no API key, same output every time for the same input. It
// exists so the rest of the system — and every test that isn't
// specifically about the Anthropic adapter — can run fully offline
// against a generator that still returns schema-valid JSON.
//
// It recognizes a small, fixed set of Japanese learner mistakes by
// substring match on the rendered user prompt; anything else yields
// zero corrections.
package fakeai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const (
	provider = "fake"
	model    = "fake-1"

	// schemaV1/schemaV2/schemaExerciseV1/schemaExerciseEvalV1 are the
	// only schemas the fake provider knows how to produce (see
	// supportedSchemas below). A request for anything else is a caller
	// bug (a new capability wired up without teaching the fake provider
	// its shape) and must fail loudly rather than silently return JSON
	// that doesn't match what was asked for.
	schemaV1 = "correction_result.v1"
	schemaV2 = "correction_result.v2"
	// schemaExerciseV1/schemaExerciseEvalV1 back internal/agent/drill
	// (Phase 2 Task 9, PRD §17.2/§58): unlike schemaV1/schemaV2, which
	// are pattern-matched against the rendered prompt (a small set of
	// known learner mistakes), these two always return the SAME canned
	// response regardless of prompt content — the fake provider has
	// exactly one drill exercise and one evaluation in its repertoire.
	// That's enough for tests/offline dev exercising the drill pipeline
	// shape (schema-valid generate/evaluate round trips), not a
	// simulation of varied exercise content.
	schemaExerciseV1     = "exercise.v1"
	schemaExerciseEvalV1 = "exercise_eval.v1"

	// schemaAnkiCardV1 backs internal/agent/anki (Phase 3 Task 3, PRD
	// §19): same "one fixed canned shape, keyed purely on SchemaName, no
	// prompt pattern-matching" treatment as the exercise schemas above.
	schemaAnkiCardV1 = "anki_card.v1"

	// schemaLessonPlanV1 backs internal/agent/lesson (Phase 3 Task 4, PRD
	// §18): same "one fixed canned shape, keyed purely on SchemaName, no
	// prompt pattern-matching" treatment as the exercise/anki schemas
	// above.
	schemaLessonPlanV1 = "lesson_plan.v1"

	// schemaWeeklySummaryV1 backs internal/agent/summary (Phase 3 Task 5,
	// PRD §21): same "one fixed canned shape, keyed purely on SchemaName,
	// no prompt pattern-matching" treatment as the exercise/anki/lesson
	// schemas above.
	schemaWeeklySummaryV1 = "weekly_summary.v1"

	// schemaConversationTurnV1 backs internal/agent/conversation (Phase 4
	// Task 6, PRD §17.4): UNLIKE the fixed-canned schemas above, this one
	// IS pattern-matched against the rendered prompt — the same
	// iAdjectivePastRules/を行きます substring rules schemaV1/schemaV2
	// match below — so a conversation turn that repeats one of the
	// teacher fixture's known mistakes gets the same corrections a
	// writing review would, keeping every fake-provider fixture in this
	// file checkable against one consistent mistake.
	schemaConversationTurnV1 = "conversation_turn.v1"

	// schemaPassageV1 backs internal/agent/drill's GeneratePassage
	// (練習's reading drill): same "one fixed canned shape, keyed purely
	// on SchemaName" treatment as the exercise schemas above. The canned
	// passage does NOT use the requested words — the fake has no way to
	// write prose — which is exactly the point: a test asserting on
	// content would be asserting on this fixture, not on the feature.
	schemaPassageV1 = "passage.v1"

	// schemaExampleV1 backs internal/agent/drill's GenerateExample. The
	// canned sentence contains the fixture word 面白い, because the agent
	// REFUSES a sentence that does not contain the word it was asked
	// about — a fixture that failed that check would make the check
	// untestable through the fake.
	schemaExampleV1 = "example.v1"

	// schemaStudyEditionV1 backs internal/agent/reading (the 読解 Kindle
	// pipeline): one fixed canned lesson, keyed purely on SchemaName, like
	// the lesson/anki fixtures. It is written about a monetary-policy
	// article so the /reading page and the EPUB renderer have realistic
	// content to show offline; 金融引き締め is its first vocabulary item,
	// which the reading tests' sample article contains, so furigana
	// annotation is exercised end to end through the fake.
	schemaStudyEditionV1 = "study_edition.v1"

	// schemaTranslationV1 backs internal/agent/reading's Translator. Not
	// a fixed canned response: the agent rejects a translation whose
	// paragraph count differs from the article's, so the fake reads the
	// count from the numbered "[n] …" user prompt and answers with that
	// many placeholder paragraphs. The text is not a translation — the
	// fake cannot write prose — it only has the right shape.
	schemaTranslationV1 = "reading_translation.v1"

	// socraticMarker is the EXACT line internal/agent/teacher's
	// teacher.feedback.v3 USER template renders when — and only when —
	// the session's TeacherMode is "socratic" (that template's opening
	// "Teacher mode: {{.TeacherMode}}" line; see its doc comment). This
	// must be the full "Teacher mode: socratic" phrase, not a bare
	// "socratic" substring: a bare substring would also match the word
	// "socratic" appearing anywhere else in User — including inside
	// session.Session.Purpose, learner-supplied free text rendered
	// verbatim into the SAME template's "Session purpose: {{.Purpose}}"
	// line (e.g. a "teacher"-mode session whose Purpose happens to be
	// "practicing the socratic method" would otherwise get hints
	// attached despite not being in socratic mode at all) — and it
	// cannot collide with the v3 SYSTEM template's own, always-present
	// "Teacher mode 'socratic': ..." hint instruction either (different
	// punctuation — quote vs colon right after "mode" — though that
	// sentence living in System, never checked here, already rules it
	// out on its own).
	socraticMarker = "Teacher mode: socratic"
)

// supportedSchemas is the set schemaV1/schemaV2/schemaExerciseV1/
// schemaExerciseEvalV1 above name; presented as a set so
// GenerateStructured's guard reads as one membership test rather than
// an OR chain that grows every time a schema is added.
var supportedSchemas = map[string]bool{
	schemaV1:                 true,
	schemaV2:                 true,
	schemaExerciseV1:         true,
	schemaExerciseEvalV1:     true,
	schemaAnkiCardV1:         true,
	schemaLessonPlanV1:       true,
	schemaWeeklySummaryV1:    true,
	schemaConversationTurnV1: true,
	schemaPassageV1:          true,
	schemaExampleV1:          true,
	schemaStudyEditionV1:     true,
	schemaTranslationV1:      true,
}

// cannedPassage mirrors schemas/defs/passage.v1.json field-for-field.
// "answer" is one of "choices", which the schema cannot express and
// drill.GeneratePassage checks by hand — a fixture that violated it
// would make that check untestable.
var cannedPassage = map[string]any{
	"passage": "きのう、友達と映画を見に行きました。とても面白かったので、また行きたいです。",
	"question": map[string]any{
		"ja": "書いた人はどう思っていますか。",
		"en": "How does the writer feel?",
	},
	"choices": []string{"また行きたい", "もう行きたくない", "映画は退屈だった"},
	"answer":  "また行きたい",
}

// cannedStudyEdition mirrors schemas/defs/study_edition.v1.json.
var cannedStudyEdition = map[string]any{
	"summary_en": "The central bank signalled it will keep tightening monetary policy while inflation stays high, and the government announced a new economic package to soften the impact on households.",
	"summary_ja": "物価が高いため、中央銀行は金融引き締めを続ける考えだ。政府は家計を助ける新しい経済対策を発表した。",
	"level":      "N1",
	"vocabulary": []map[string]any{
		{
			"expression": "金融引き締め",
			"reading":    "きんゆうひきしめ",
			"meaning_en": "monetary tightening",
			"usage_en":   "Standard term in economic and central-bank reporting; often with 続ける or 強化する.",
			"example_ja": "中央銀行はインフレを抑えるため、金融引き締めを強化した。",
			"example_en": "The central bank stepped up monetary tightening to curb inflation.",
		},
		{
			"expression": "経済対策",
			"reading":    "けいざいたいさく",
			"meaning_en": "economic stimulus package; economic measures",
			"usage_en":   "Government policy packages; typically 経済対策を発表する／打ち出す.",
			"example_ja": "政府は来月、大規模な経済対策を打ち出す方針だ。",
			"example_en": "The government plans to unveil a large economic package next month.",
		},
		{
			"expression": "めぐる",
			"reading":    "",
			"meaning_en": "to concern; to surround (an issue)",
			"usage_en":   "In news mostly as 〜をめぐる／〜をめぐり, introducing the topic of a dispute.",
			"example_ja": "予算をめぐる議論が続いている。",
			"example_en": "The debate over the budget continues.",
		},
	},
	"grammar": []map[string]any{
		{
			"pattern":        "〜をめぐり",
			"meaning_en":     "concerning; over (a contested issue)",
			"explanation_en": "Written-style topic marker for an issue under debate; the 連用中止 form of 〜をめぐって.",
			"from_article":   "新たな経済対策をめぐり",
			"example_ja":     "新しい法案をめぐり、与野党が対立している。",
			"example_en":     "The ruling and opposition parties are at odds over the new bill.",
		},
	},
	"sentence_analyses": []map[string]any{
		{
			"sentence":       "政府が発表した新たな経済対策をめぐり、議論が続いている。",
			"translation_en": "Debate continues over the new economic package the government announced.",
			"chunks": []map[string]any{
				{"text": "政府が発表した", "reading": "せいふがはっぴょうした", "role_en": "relative clause modifying 経済対策"},
				{"text": "新たな経済対策をめぐり、", "reading": "あらたなけいざいたいさくをめぐり", "role_en": "topic of the debate (〜をめぐり)"},
				{"text": "議論が続いている。", "reading": "ぎろんがつづいている", "role_en": "main clause"},
			},
			"note_en": "Find the noun the long clause modifies (経済対策) before reading the main clause.",
		},
	},
	"review": map[string]any{
		"comprehension": []map[string]any{
			{"question_ja": "中央銀行はなぜ金融引き締めを続けるのですか。", "answer_ja": "物価が高い状態が続いているから。"},
		},
		"vocabulary": []map[string]any{
			{"question_ja": "「経済対策」を使って文を一つ作ってください。", "answer_ja": "政府は新しい経済対策を発表した。"},
		},
	},
}

// cannedExample mirrors schemas/defs/example.v1.json.
var cannedExample = map[string]any{
	"sentence":       "その映画はとても面白いですね。",
	"translation_en": "That film is very interesting, isn't it?",
}

type explanation struct {
	JA string `json:"ja"`
	EN string `json:"en"`
}

type correction struct {
	Original    string      `json:"original"`
	Replacement string      `json:"replacement"`
	Type        string      `json:"type"`
	Severity    string      `json:"severity"`
	Explanation explanation `json:"explanation"`
	Concepts    []string    `json:"concepts,omitempty"`
	// Hint is only ever set when the caller asked for schemaV2 AND the
	// rendered prompt carries socraticMarker (see hintFor below) —
	// schemaV1's item schema is additionalProperties:false with no
	// "hint" property, so attaching one to a v1 response would produce
	// JSON that fails its own schema validation.
	Hint *explanation `json:"hint,omitempty"`
}

type correctionResult struct {
	Corrections []correction `json:"corrections"`
}

// exerciseInstructions mirrors both schemas/defs/exercise.v1.json's
// "instructions" object and exercise_eval.v1.json's "feedback" object —
// the same {ja, en} shape both schemas use.
type exerciseInstructions struct {
	JA string `json:"ja"`
	EN string `json:"en"`
}

// cannedExercise mirrors schemas/defs/exercise.v1.json field-for-field.
type cannedExercise struct {
	Type         string               `json:"type"`
	Instructions exerciseInstructions `json:"instructions"`
	Prompt       string               `json:"prompt"`
	Choices      []string             `json:"choices,omitempty"`
	Answer       string               `json:"answer,omitempty"`
	Concept      string               `json:"concept"`
}

// cannedExerciseEval mirrors schemas/defs/exercise_eval.v1.json
// field-for-field.
type cannedExerciseEval struct {
	Correct  bool                 `json:"correct"`
	Score    int                  `json:"score"`
	Feedback exerciseInstructions `json:"feedback"`
}

// iAdjectivePastExercise is the fake provider's one canned drill
// exercise (Phase 2 Task 9's Step 1 pin): a multiple-choice question on
// the i-adjective-past concept, matching TestReviewWritingFakeAIHappyPath's
// 面白い/面白かったです rule above so the same concept shows up
// consistently across the teacher and drill fakes.
var iAdjectivePastExercise = cannedExercise{
	Type: "multiple-choice",
	Instructions: exerciseInstructions{
		JA: "正しい過去形を選んでください。",
		EN: "Choose the correct past tense.",
	},
	Prompt:  "昨日の映画はとても＿＿＿。",
	Choices: []string{"面白いでした", "面白かったです", "面白いだった"},
	Answer:  "面白かったです",
	Concept: "i-adjective-past",
}

// cannedAnkiCard mirrors schemas/defs/anki_card.v1.json field-for-field.
type cannedAnkiCard struct {
	Front string `json:"front"`
	Back  string `json:"back"`
	Notes string `json:"notes,omitempty"`
}

// iAdjectivePastAnkiCard is the fake provider's one canned Anki card
// (Phase 3 Task 3's Step 1 pin): the task brief's exact pinned wording,
// matching the same 面白い/面白かったです i-adjective-past mistake every
// other fake-provider fixture in this file uses.
var iAdjectivePastAnkiCard = cannedAnkiCard{
	Front: "「とても面白いでした」— 何が不自然？",
	Back:  "「とても面白かったです」\n\n理由: い形容詞の過去形は〜かったを使います。",
	Notes: "i-adjective-past",
}

// cannedLessonPlan mirrors schemas/defs/lesson_plan.v1.json field-for-
// field. It is the fake provider's one canned tutor lesson guide
// (Phase 3 Task 4's Step 1 pin): every section references the same
// i-adjective-past mistake and それはそれとして expression every other
// fake-provider fixture in this file uses, so the pin is checkable
// without inventing a second learner history.
type cannedLessonPlan struct {
	LevelSummary        string   `json:"level_summary"`
	Strengths           []string `json:"strengths"`
	Weaknesses          []string `json:"weaknesses"`
	Focus               []string `json:"focus"`
	Vocabulary          []string `json:"vocabulary"`
	GrammarConcepts     []string `json:"grammar_concepts"`
	ConversationPrompts []string `json:"conversation_prompts"`
	Exercises           []string `json:"exercises"`
	RecentExamples      []string `json:"recent_examples"`
	QuestionsForTutor   []string `json:"questions_for_tutor"`
}

var iAdjectivePastLessonPlan = cannedLessonPlan{
	LevelSummary: "中級前半。日常的な話題は書けるが、過去形の活用に一貫しない誤りが残る。",
	Strengths:    []string{"語彙は幅広く、自然な言い回しを選ぼうとする姿勢がある。"},
	Weaknesses:   []string{"い形容詞の過去形（i-adjective-past）を「〜いでした」と誤る傾向が続いている。"},
	Focus:        []string{"い形容詞の過去形の活用ルールを反復練習する。"},
	Vocabulary:   []string{"それはそれとして — 話題を切り替える際に使える表現。まだ定着していないため活性化を促す。"},
	GrammarConcepts: []string{
		"i-adjective-past: 「〜かったです」の形を徹底する。",
	},
	ConversationPrompts: []string{
		"昨日の映画はどうでしたか？（過去形の形容詞を引き出す）",
		"それはそれとして、最近何か新しいことを始めましたか？",
	},
	Exercises: []string{"い形容詞の過去形の穴埋め練習を3問。"},
	RecentExamples: []string{
		"「とても面白いでした」→「とても面白かったです」の誤りが直近の作文で見られた。",
	},
	QuestionsForTutor: []string{
		"い形容詞の過去形の誤りは口頭でも同様に見られるか、確認してほしい。",
	},
}

// cannedWeeklySummary mirrors schemas/defs/weekly_summary.v1.json
// field-for-field. It is the fake provider's one canned weekly summary
// email (Phase 3 Task 5's Step 1 pin): mentions both 面白かったです (the
// corrected i-adjective-past form every other fake-provider fixture in
// this file's mistake resolves to) and それはそれとして (the same
// activation-candidate expression iAdjectivePastLessonPlan's vocabulary
// section names), so the canned content is checkable without inventing
// a third learner history.
type cannedWeeklySummary struct {
	Subject              string   `json:"subject"`
	Accomplishments      []string `json:"accomplishments"`
	Improvements         []string `json:"improvements"`
	PersistentWeaknesses []string `json:"persistent_weaknesses"`
	NewExpressions       []string `json:"new_expressions"`
	RecommendedFocus     []string `json:"recommended_focus"`
	Challenge            string   `json:"challenge,omitempty"`
}

var weeklySummaryFixture = cannedWeeklySummary{
	Subject: "今週の学習まとめ — 順調に前進しています！",
	Accomplishments: []string{
		"今週も作文セッションに取り組み、書く習慣を続けました。",
		"「面白かったです」のような表現を使う場面が増えてきました。",
	},
	Improvements: []string{
		"い形容詞の過去形の誤りが少しずつ減ってきています。「面白かったです」を自分から使えた場面もありました。",
	},
	PersistentWeaknesses: []string{
		"い形容詞の過去形（〜いでした→〜かったです）は、まだ時々誤ることがあります。焦らず続けましょう。",
	},
	NewExpressions: []string{
		"それはそれとして — 話題を切り替える際に使える便利な表現です。今週の作文でも使ってみましょう。",
	},
	RecommendedFocus: []string{
		"い形容詞の過去形の活用を、短い文で繰り返し練習してみましょう。",
	},
	Challenge: "次の作文で「それはそれとして」を一度使ってみましょう。",
}

// conversationTurnResponse mirrors schemas/defs/conversation_turn.v1.json
// field-for-field. Corrections reuses the same `correction` type
// schemaV2's own items use — schemaV2's item shape and this schema's
// item shape are field-for-field identical (see schemas/defs/
// conversation_turn.v1.json) — so matchedCorrections below can back
// both without a second, parallel correction type.
type conversationTurnResponse struct {
	Reply       string       `json:"reply"`
	ReplyEN     string       `json:"reply_en,omitempty"`
	Corrections []correction `json:"corrections,omitempty"`
	Followup    string       `json:"followup,omitempty"`
}

// conversationReplyClean/conversationReplyCorrected are the fake
// provider's two canned conversational replies (Phase 4 Task 6's Step 1
// pin): which one comes back depends on whether the learner's latest
// message matched one of matchedCorrections' rules below — a message
// with no known mistake gets conversationReplyClean, one that does gets
// conversationReplyCorrected — mirroring schemaV1/schemaV2's own "same
// mistake catalog drives the response" pattern.
var (
	conversationReplyClean = conversationTurnResponse{
		Reply:    "そうですか、いいですね！",
		ReplyEN:  "I see, that's nice!",
		Followup: "他に何かありましたか？",
	}
	conversationReplyCorrected = conversationTurnResponse{
		Reply:    "なるほど、教えてくれてありがとうございます。",
		ReplyEN:  "I see, thanks for telling me.",
		Followup: "それについてもっと聞かせてください。",
	}
)

// freeProductionEval is the fake provider's one canned drill evaluation.
var freeProductionEval = cannedExerciseEval{
	Correct: true,
	Score:   85,
	Feedback: exerciseInstructions{
		JA: "よく書けています！",
		EN: "Well written!",
	},
}

// iAdjectivePastRules: each entry is a dictionary-form い-adjective the
// fake provider knows learners commonly conjugate wrong as
// "<stem>いでした" instead of "<stem>かったです".
var iAdjectivePastRules = []string{"面白い", "楽しい"}

// iAdjectivePastHint/particleHint are the fake provider's socratic-mode
// hints (Phase 2 Task 8, PRD §9/§53): a nudge that names the problem
// area WITHOUT giving away the corrected form. iAdjectivePastHint's
// text is the brief's exact pinned wording — it's rule-general (not
// tied to which い-adjective matched), so it's shared by every
// iAdjectivePastRules entry.
var (
	iAdjectivePastHint = explanation{
		JA: "い形容詞の過去形の作り方を思い出してください。",
		EN: "Recall how い-adjectives form the past tense.",
	}
	particleHint = explanation{
		JA: "「行きます」と一緒に使う助詞を思い出してください。",
		EN: "Recall which particle pairs with 行きます.",
	}
)

// hintFor returns a pointer to hint when socratic is true, nil
// otherwise — the single place GenerateStructured's two rules decide
// whether to attach a hint, so that decision can't drift between them.
func hintFor(socratic bool, hint explanation) *explanation {
	if !socratic {
		return nil
	}
	h := hint
	return &h
}

type generator struct{}

// New returns the fake provider. It satisfies ai.StructuredGenerator.
// New returns a value implementing BOTH ai.StructuredGenerator and
// ai.ToolCaller — see CallWithTools below for the deterministic
// two-turn script it plays for the latter.
func New() *generator { return &generator{} }

func (g *generator) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()

	if req.SchemaName != "" && !supportedSchemas[req.SchemaName] {
		return ai.StructuredResponse{Provider: provider, Model: model},
			fmt.Errorf("fakeai: schema %q not supported (only %q, %q, %q, %q, %q, %q, %q, %q)", req.SchemaName, schemaV1, schemaV2, schemaExerciseV1, schemaExerciseEvalV1, schemaAnkiCardV1, schemaLessonPlanV1, schemaWeeklySummaryV1, schemaConversationTurnV1)
	}

	// exercise.v1/exercise_eval.v1 (internal/agent/drill) and
	// anki_card.v1 (internal/agent/anki) are unrelated to the
	// correction-result shape below: always the same canned response,
	// keyed purely on SchemaName — see schemaExerciseV1's doc comment
	// for why there's no prompt pattern-matching here.
	switch req.SchemaName {
	case schemaExerciseV1:
		return g.respond(start, req, iAdjectivePastExercise)
	case schemaExerciseEvalV1:
		return g.respond(start, req, freeProductionEval)
	case schemaAnkiCardV1:
		return g.respond(start, req, iAdjectivePastAnkiCard)
	case schemaLessonPlanV1:
		return g.respond(start, req, iAdjectivePastLessonPlan)
	case schemaWeeklySummaryV1:
		return g.respond(start, req, weeklySummaryFixture)
	case schemaConversationTurnV1:
		return g.respondConversationTurn(start, req)
	case schemaPassageV1:
		return g.respond(start, req, cannedPassage)
	case schemaExampleV1:
		return g.respond(start, req, cannedExample)
	case schemaStudyEditionV1:
		return g.respond(start, req, cannedStudyEdition)
	case schemaTranslationV1:
		return g.respond(start, req, fakeTranslation(req.User))
	}

	// socratic gates hint attachment on BOTH conditions schemaV2's own
	// doc comment describes: the caller requested the schema that has a
	// "hint" property to put one in, AND the rendered prompt actually
	// carries socraticMarker for THIS request (a v2-schema request for a
	// non-socratic session must still come back hint-less).
	socratic := req.SchemaName == schemaV2 && strings.Contains(req.User, socraticMarker)

	result := correctionResult{Corrections: matchedCorrections(req.User, socratic)}

	payload, err := json.Marshal(result)
	if err != nil {
		// Provider/Model are known regardless of outcome, so set them
		// even here: the observability decorator's audit record must be
		// able to say which provider/model a failed call went through.
		return ai.StructuredResponse{Provider: provider, Model: model},
			fmt.Errorf("fakeai: marshal response: %w", err)
	}

	return ai.StructuredResponse{
		JSON:         payload,
		Provider:     provider,
		Model:        model,
		InputTokens:  runeCount(req.System) + runeCount(req.User),
		OutputTokens: runeCount(string(payload)),
		Latency:      time.Since(start),
	}, nil
}

// respond marshals payload (a cannedExercise or cannedExerciseEval) into
// an ai.StructuredResponse with the same Provider/Model/token-count/
// latency bookkeeping the correction-result path below builds by hand —
// factored out here since the exercise schemas have exactly one canned
// shape each, with no per-request branching to interleave it with.
func (g *generator) respond(start time.Time, req ai.StructuredRequest, payload any) (ai.StructuredResponse, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: model},
			fmt.Errorf("fakeai: marshal response: %w", err)
	}
	return ai.StructuredResponse{
		JSON:         raw,
		Provider:     provider,
		Model:        model,
		InputTokens:  runeCount(req.System) + runeCount(req.User),
		OutputTokens: runeCount(string(raw)),
		Latency:      time.Since(start),
	}, nil
}

// numberedParagraph matches the "[n] " a numbered prompt puts at the
// start of paragraph n.
var numberedParagraph = regexp.MustCompile(`(?m)^\[(\d+)\] `)

// fakeTranslation answers reading_translation.v1 with one Japanese
// placeholder per numbered paragraph in the prompt.
func fakeTranslation(user string) map[string]any {
	n := 0
	for _, m := range numberedParagraph.FindAllStringSubmatch(user, -1) {
		if m[1] == strconv.Itoa(n+1) {
			n++
		}
	}
	paras := make([]string, n)
	for i := range paras {
		paras[i] = fmt.Sprintf("（訳）%d段落目の本文です。", i+1)
	}
	return map[string]any{
		"source_language": "英語",
		"title":           "（訳）記事のタイトル",
		"paragraphs":      paras,
	}
}

// matchedCorrections scans text (a rendered prompt's User half) for
// every known-mistake pattern the fake provider recognizes —
// iAdjectivePastRules and the を行きます particle mistake, the SAME two
// rules the correction_result.v1/v2 path below has always matched —
// and returns one correction per match, hint-attached per socratic's
// same hintFor gate. Shared by that path AND
// respondConversationTurn below, so the two capabilities can never
// drift on what counts as a "known mistake" or how its explanation is
// worded. Always returns a non-nil (possibly empty) slice, since
// correction_result's own schema requires "corrections" to be an array,
// never null.
func matchedCorrections(text string, socratic bool) []correction {
	cs := []correction{}
	for _, adj := range iAdjectivePastRules {
		if c, ok := iAdjectivePastCorrection(adj, text); ok {
			c.Hint = hintFor(socratic, iAdjectivePastHint)
			cs = append(cs, c)
		}
	}
	if strings.Contains(text, "を行きます") {
		cs = append(cs, correction{
			Original:    "を",
			Replacement: "に",
			Type:        "particle",
			Severity:    "incorrect",
			Explanation: explanation{
				JA: "移動を表す「行きます」は目的地に「に」を使います。「を」は使いません。",
				EN: "行きます (\"to go\") marks its destination with に, not を.",
			},
			Concepts: []string{"particle-ni-direction"},
			Hint:     hintFor(socratic, particleHint),
		})
	}
	return cs
}

// learnerSaidMarker is the exact line
// templates/conversation.turn.v1.user.md renders immediately before the
// learner's new message this turn ("The learner just said:\n{{.Message}}").
// respondConversationTurn below anchors on it to isolate JUST this
// turn's message from the rest of the rendered prompt — critically,
// from the "Conversation so far:" history section rendered ABOVE it,
// which quotes every prior turn's learner text verbatim. Without this,
// matchedCorrections would re-detect the SAME known mistake in a prior
// turn's quoted history on every subsequent turn, double- (then triple-,
// quadruple-...) counting it as the conversation grows.
const learnerSaidMarker = "The learner just said:\n"

// currentMessage extracts the single line of learner text
// learnerSaidMarker introduces from a rendered conversation.turn.v1
// User prompt — see that constant's doc comment for why this must be
// scoped to just this turn's message. Falls back to the whole string
// when the marker isn't found (defensive only; the real template
// always renders it).
func currentMessage(user string) string {
	idx := strings.Index(user, learnerSaidMarker)
	if idx == -1 {
		return user
	}
	rest := user[idx+len(learnerSaidMarker):]
	if nl := strings.Index(rest, "\n"); nl != -1 {
		return rest[:nl]
	}
	return rest
}

// respondConversationTurn is the fake provider's conversation_turn.v1
// path (Phase 4 Task 6, PRD §17.4): matchedCorrections finds whatever
// known mistakes the learner's latest message (currentMessage, isolated
// from the rest of the prompt — see its own doc comment) contains,
// using the SAME socraticMarker detection schemaV2's own hint-attachment
// uses; the canned reply is conversationReplyCorrected when any were
// found, conversationReplyClean otherwise. Corrections is left nil
// (omitted from the JSON, via its own omitempty tag) rather than an
// empty slice when there are none — unlike correction_result.v1/v2,
// conversation_turn.v1's own schema does NOT require "corrections" to
// be present at all.
func (g *generator) respondConversationTurn(start time.Time, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	socratic := strings.Contains(req.User, socraticMarker)
	corrections := matchedCorrections(currentMessage(req.User), socratic)

	payload := conversationReplyClean
	if len(corrections) > 0 {
		payload = conversationReplyCorrected
		payload.Corrections = corrections
	}
	return g.respond(start, req, payload)
}

// iAdjectivePastCorrection reports the deterministic correction for
// dictionary-form い-adjective adj (e.g. "面白い") if the wrong past
// tense "<stem>いでした" appears in text.
func iAdjectivePastCorrection(adj, text string) (correction, bool) {
	stem := strings.TrimSuffix(adj, "い")
	wrong := stem + "いでした"
	right := stem + "かったです"
	if !strings.Contains(text, wrong) {
		return correction{}, false
	}
	return correction{
		Original:    wrong,
		Replacement: right,
		Type:        "conjugation",
		Severity:    "incorrect",
		Explanation: explanation{
			JA: fmt.Sprintf("い形容詞の過去形は「〜かった」を使います。「%s」→「%s」。", adj, stem+"かった"),
			EN: fmt.Sprintf("い-adjectives form the past tense with 〜かった, so %s must be %s.", wrong, right),
		},
		Concepts: []string{"i-adjective-past"},
	}, true
}

func runeCount(s string) int { return len([]rune(s)) }

// toolCallToolName is the ONE tool fakeai's CallWithTools script ever
// calls — get_learning_priorities, per the task brief's exact script:
// "first turn invokes get_learning_priorities, second turn returns
// prose mentioning the returned priority". This is a fixed, canned
// scripted sequence (same "one fixed shape, not a simulation" treatment
// every schema-keyed fixture above gets), not a general tool-calling
// simulator — enough to drive a real two-turn agent-run loop offline
// without a live model, for tests and dev.
const toolCallToolName = "get_learning_priorities"

// priorityView mirrors internal/tools.priorityView's "subject" field —
// the only piece of a get_learning_priorities tool result CallWithTools
// actually reads, to weave it into its second-turn prose.
type priorityView struct {
	Subject string `json:"subject"`
}

// CallWithTools implements ai.ToolCaller with a deterministic
// two-turn script, keyed purely on whether the LAST message in
// req.Messages is a "tool" turn (i.e. the caller already ran the
// first turn's invocation and is feeding its result back):
//
//   - No trailing "tool" message yet: return a turn that calls
//     get_learning_priorities with empty arguments and no prose — the
//     model "deciding" to look up the learner's priorities before
//     answering.
//   - A trailing "tool" message: read its first ai.ToolResult.Content
//     (the JSON array internal/tools.getLearningPrioritiesTool
//     returns), extract the first entry's "subject", and answer in
//     prose naming it, with no further Invocations — the model is
//     done.
//
// This never inspects req.Tools or which tool the caller actually
// registered under that name — like every other fixture in this file,
// it is a fixed script, not a simulation of real tool-selection
// reasoning.
func (g *generator) CallWithTools(_ context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	start := time.Now()

	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "tool" {
		subject := "your recent priorities"
		results := req.Messages[n-1].Results
		if len(results) > 0 {
			var priorities []priorityView
			if err := json.Unmarshal([]byte(results[0].Content), &priorities); err == nil && len(priorities) > 0 && priorities[0].Subject != "" {
				subject = priorities[0].Subject
			}
		}
		text := fmt.Sprintf("Based on your learning priorities, you should focus on %s next.", subject)
		return ai.ToolResponse{
			Turn:         ai.ToolTurn{Text: text},
			Provider:     provider,
			Model:        model,
			InputTokens:  runeCount(req.System),
			OutputTokens: runeCount(text),
			Latency:      time.Since(start),
		}, nil
	}

	return ai.ToolResponse{
		Turn: ai.ToolTurn{
			Invocations: []ai.ToolInvocation{
				{ID: "fake-call-1", Name: toolCallToolName, Arguments: json.RawMessage(`{}`)},
			},
		},
		Provider:     provider,
		Model:        model,
		InputTokens:  runeCount(req.System),
		OutputTokens: 0,
		Latency:      time.Since(start),
	}, nil
}
