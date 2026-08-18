package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

// testAPIKey is a fabricated value that never leaves this process. The
// real key lives outside the repo entirely (see the package doc
// comment): no test, fixture, or error-message assertion in this
// package has ever contained it, and every leak test below asserts
// against THIS string, which is enough — the adapter cannot tell one
// key from another, so a path that would leak this one would leak the
// real one too.
const testAPIKey = "AIza-test-key-not-a-real-credential"

// realSchema returns one of JLP's actual embedded schemas. The
// translator is tested against the REAL documents rather than
// hand-written miniatures on purpose: the whole point of this code is
// that Gemini rejects specific things JLP's schemas really contain, so
// a miniature that drifted from the real file would test nothing.
func realSchema(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := schemas.Get(name)
	if err != nil {
		t.Fatalf("schemas.Get(%q): %v", name, err)
	}
	return raw
}

// keysAnywhere collects every object key appearing anywhere in doc,
// including inside "properties" maps — deliberately broader than the
// translator's own keyword walk, so a test asserting "$schema is gone"
// really means gone from the whole document.
func keysAnywhere(t *testing.T, doc json.RawMessage) map[string]bool {
	t.Helper()
	var node any
	if err := json.Unmarshal(doc, &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				found[k] = true
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(node)
	return found
}

// --- the translator ------------------------------------------------

// TestTranslateStripsExactlyTheKeysGeminiRejects pins measured fact 2
// and measured fact 3 together: $schema/additionalProperties/title are
// the three keys that made the live API 400, and minLength is NOT one
// of them, so stripping it would needlessly weaken the constraint.
func TestTranslateStripsExactlyTheKeysGeminiRejects(t *testing.T) {
	translated, blockers, err := translateSchema(realSchema(t, "correction_result.v1"))
	if err != nil {
		t.Fatalf("translateSchema: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %v, want none — correction_result.v1 was measured working as a responseSchema", blockers)
	}

	keys := keysAnywhere(t, translated)
	for _, gone := range []string{"$schema", "additionalProperties", "title"} {
		if keys[gone] {
			t.Errorf("translated schema still contains %q — the live API 400s on it", gone)
		}
	}
	if !keys["minLength"] {
		t.Error("translated schema dropped minLength — it was measured ACCEPTED, so stripping it only loses a constraint")
	}
	for _, kept := range []string{"type", "properties", "required", "enum", "items"} {
		if !keys[kept] {
			t.Errorf("translated schema dropped %q", kept)
		}
	}
}

// TestTranslateDetectsExerciseV1AsUntranslatable pins measured fact 4.
// The assertion is on the KEYWORDS reported, never on the schema's
// name: a future schema that grows an `if` must take the fallback
// automatically, without anyone remembering to add it to a list.
func TestTranslateDetectsExerciseV1AsUntranslatable(t *testing.T) {
	_, blockers, err := translateSchema(realSchema(t, "exercise.v1"))
	if err != nil {
		t.Fatalf("translateSchema: %v", err)
	}
	for _, want := range []string{"allOf", "if", "then"} {
		if !contains(blockers, want) {
			t.Errorf("blockers = %v, want it to include %q", blockers, want)
		}
	}
}

// schemaNames lists every schema in the registry by reading the same
// directory internal/schemas embeds. Enumerated from disk rather than
// hardcoded so a ninth schema is covered by the inventory test below
// the day it is added, without anyone remembering to list it.
func schemaNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("../../schemas/defs")
	if err != nil {
		t.Fatalf("read schema defs: %v", err)
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		t.Fatal("found no schemas to check")
	}
	return names
}

// TestTranslateAcceptsEverySchemaExceptExerciseV1 is measured fact 2's
// "7 of the 8" restated as an executable inventory, so adding a ninth
// schema that Gemini cannot express is noticed here rather than in
// production.
func TestTranslateAcceptsEverySchemaExceptExerciseV1(t *testing.T) {
	for _, name := range schemaNames(t) {
		_, blockers, err := translateSchema(realSchema(t, name))
		if err != nil {
			t.Fatalf("%s: translateSchema: %v", name, err)
		}
		if name == "exercise.v1" {
			if len(blockers) == 0 {
				t.Errorf("%s: want blockers, got none", name)
			}
			continue
		}
		if len(blockers) != 0 {
			t.Errorf("%s: blockers = %v, want none", name, blockers)
		}
	}
}

// TestTranslateIgnoresPropertyNamesThatLookLikeKeywords: a property
// legitimately CALLED "if" or "additionalProperties" is data, not a
// schema keyword. Walking blindly would both mangle the schema and
// send a perfectly expressible document down the expensive fallback.
func TestTranslateIgnoresPropertyNamesThatLookLikeKeywords(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"if":    {"type": "string"},
			"title": {"type": "string", "minLength": 1},
			"allOf": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["if"]
	}`)

	translated, blockers, err := translateSchema(raw)
	if err != nil {
		t.Fatalf("translateSchema: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %v, want none — those are property NAMES, not keywords", blockers)
	}

	var got struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(translated, &got); err != nil {
		t.Fatalf("unmarshal translated: %v", err)
	}
	for _, name := range []string{"if", "title", "allOf"} {
		if _, ok := got.Properties[name]; !ok {
			t.Errorf("property %q was stripped from the translated schema", name)
		}
	}
}

// TestTranslatePreservesIntegerLiterals guards a quiet way to corrupt a
// schema: decoding into `any` turns every number into a float64, and a
// large one re-marshals as 1e+06 — which is not an integer to any JSON
// Schema reader, Gemini's included.
func TestTranslatePreservesIntegerLiterals(t *testing.T) {
	translated, _, err := translateSchema(json.RawMessage(`{"type":"string","minLength":1,"maxLength":1000000}`))
	if err != nil {
		t.Fatalf("translateSchema: %v", err)
	}
	if got := string(translated); !strings.Contains(got, "1000000") {
		t.Errorf("translated = %s, want maxLength to survive as the integer 1000000", got)
	}
}

// TestPromptSchemaKeepsWhatTranslationHadToDrop is the fallback's half
// of the same contract: prose has no OpenAPI subset to satisfy, so the
// pasted document keeps every constraint — only the $schema KEYWORD
// goes, and a property legitimately named "$schema" stays.
func TestPromptSchemaKeepsWhatTranslationHadToDrop(t *testing.T) {
	pasted, err := promptSchema(realSchema(t, "exercise.v1"))
	if err != nil {
		t.Fatalf("promptSchema: %v", err)
	}
	for _, want := range []string{`"allOf"`, `"if"`, `"then"`, `"const"`, `"additionalProperties"`, `"title"`} {
		if !strings.Contains(pasted, want) {
			t.Errorf("pasted schema lost %s", want)
		}
	}
	if strings.Contains(pasted, `"$schema":`) {
		t.Error("pasted schema kept the $schema keyword — the captured live fallback echoed it into the answer")
	}

	kept, err := promptSchema(json.RawMessage(`{"type":"object","properties":{"$schema":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("promptSchema: %v", err)
	}
	if !strings.Contains(kept, `"$schema"`) {
		t.Error("a property legitimately NAMED $schema was stripped — that is data, not a keyword")
	}
}

// --- the wire body -------------------------------------------------

// capturingServer stands in for generativelanguage.googleapis.com: it
// records the request the adapter built and replies with body.
type capturedRequest struct {
	path   string
	rawURL string
	header http.Header
	body   []byte
}

func capturingServer(t *testing.T, status int, reply []byte) (*httptest.Server, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.rawURL = r.URL.String()
		got.header = r.Header.Clone()
		sent, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		got.body = sent
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(reply)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// wireRequest is the shape this adapter's tests assert on — declared
// here, in the test, rather than reusing the adapter's own request type,
// so a rename in the adapter that silently changes the wire contract
// fails here.
type wireRequest struct {
	SystemInstruction *struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction"`
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`
	Tools []struct {
		FunctionDeclarations []struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	GenerationConfig struct {
		ResponseMIMEType string          `json:"responseMimeType"`
		ResponseSchema   json.RawMessage `json:"responseSchema"`
	} `json:"generationConfig"`
}

func decodeWire(t *testing.T, body []byte) wireRequest {
	t.Helper()
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, body)
	}
	return req
}

func testConfig(url string) config.Gemini {
	return config.Gemini{
		APIKey:  testAPIKey,
		Model:   "gemini-3-flash-preview",
		BaseURL: url,
		Timeout: 10 * time.Second,
	}
}

func structuredRequest(t *testing.T, schemaName string) ai.StructuredRequest {
	t.Helper()
	return ai.StructuredRequest{
		PromptName: "teacher.feedback",
		System:     "You are a Japanese teacher.",
		User:       "面白いでした。",
		SchemaName: schemaName,
		Schema:     realSchema(t, schemaName),
	}
}

// TestRequestCarriesSystemInstructionContentsAndSchema pins the whole
// happy-path wire contract in one place.
func TestRequestCarriesSystemInstructionContentsAndSchema(t *testing.T) {
	srv, got := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_schema.json"))
	g := New(testConfig(srv.URL), nil)

	if _, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1")); err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}

	if want := "/v1beta/models/gemini-3-flash-preview:generateContent"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if h := got.header.Get("x-goog-api-key"); h != testAPIKey {
		t.Errorf("x-goog-api-key header = %q, want the configured key", h)
	}

	req := decodeWire(t, got.body)
	if req.SystemInstruction == nil || len(req.SystemInstruction.Parts) == 0 ||
		req.SystemInstruction.Parts[0].Text != "You are a Japanese teacher." {
		t.Errorf("systemInstruction did not carry req.System: %+v", req.SystemInstruction)
	}
	if len(req.Contents) != 1 || req.Contents[0].Role != "user" ||
		len(req.Contents[0].Parts) == 0 || req.Contents[0].Parts[0].Text != "面白いでした。" {
		t.Errorf("contents did not carry req.User: %+v", req.Contents)
	}
	if req.GenerationConfig.ResponseMIMEType != "application/json" {
		t.Errorf("responseMimeType = %q, want application/json", req.GenerationConfig.ResponseMIMEType)
	}
	if len(req.GenerationConfig.ResponseSchema) == 0 {
		t.Fatal("responseSchema was absent — correction_result.v1 is expressible and must be sent as a schema, not pasted into the prompt")
	}
	if keys := keysAnywhere(t, req.GenerationConfig.ResponseSchema); keys["$schema"] || keys["additionalProperties"] {
		t.Error("responseSchema went out untranslated — the live API 400s on $schema/additionalProperties")
	}
}

// TestUntranslatableSchemaFallsBackToThePrompt pins measured fact 5's
// design consequence: no responseSchema at all, the schema pasted into
// the user turn instead, JSON mime type still requested.
func TestUntranslatableSchemaFallsBackToThePrompt(t *testing.T) {
	srv, got := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_promptfallback.json"))
	g := New(testConfig(srv.URL), nil)

	if _, err := g.GenerateStructured(context.Background(), structuredRequest(t, "exercise.v1")); err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}

	req := decodeWire(t, got.body)
	if len(req.GenerationConfig.ResponseSchema) != 0 {
		t.Errorf("responseSchema was sent for exercise.v1 — the live API 400s on its `if` keyword: %s", req.GenerationConfig.ResponseSchema)
	}
	if req.GenerationConfig.ResponseMIMEType != "application/json" {
		t.Errorf("responseMimeType = %q, want application/json even on the fallback", req.GenerationConfig.ResponseMIMEType)
	}
	if len(req.Contents) == 0 || len(req.Contents[0].Parts) == 0 {
		t.Fatal("no user content")
	}
	text := req.Contents[0].Parts[0].Text
	if !strings.Contains(text, "面白いでした。") {
		t.Error("the fallback prompt dropped the user's own text")
	}
	// The pasted document keeps everything responseSchema could not
	// express — the allOf/if/then that forced this path at all, and the
	// additionalProperties:false that keeps the model from inventing
	// keys. Pasting the TRANSLATED schema instead would hand the model a
	// weaker contract than the one JLP then validates the answer
	// against, which turns a fallback into a repair loop.
	for _, want := range []string{`"allOf"`, `"if"`, `"then"`, `"additionalProperties"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the fallback prompt does not contain %s — that constraint is now enforced by nothing on the way out", want)
		}
	}
	if strings.Contains(text, `"$schema":`) {
		t.Error(`the fallback prompt pasted a "$schema" key: the captured live fallback response echoed that key straight back into its answer, where correction_result.v1's additionalProperties:false rejects it`)
	}
}

// TestSchemaPathIsUsedForExpressibleSchemas is the cost half of the
// same decision: pasting the schema into the prompt was measured at 515
// input tokens against 227 for the schema path, so an adapter that
// always took the fallback would cost 2.3x on every call.
func TestSchemaPathIsUsedForExpressibleSchemas(t *testing.T) {
	srv, got := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_schema.json"))
	g := New(testConfig(srv.URL), nil)

	if _, err := g.GenerateStructured(context.Background(), structuredRequest(t, "weekly_summary.v1")); err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	req := decodeWire(t, got.body)
	if len(req.GenerationConfig.ResponseSchema) == 0 {
		t.Error("weekly_summary.v1 took the prompt fallback — it was measured working as a responseSchema, and the fallback costs 2.3x the input tokens")
	}
	if strings.Contains(req.Contents[0].Parts[0].Text, `"properties"`) {
		t.Error("the schema was pasted into the prompt as well as sent as responseSchema — that is the expensive path twice")
	}
}

// --- response parsing ----------------------------------------------

// TestParsesTheCapturedSchemaPathResponse pins the parser to bytes the
// real API really produced (house style: adapters/agycli's
// testdata/agy_live.json).
func TestParsesTheCapturedSchemaPathResponse(t *testing.T) {
	srv, _ := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_schema.json"))
	g := New(testConfig(srv.URL), nil)

	resp, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1"))
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}

	if resp.Provider != "gemini" {
		t.Errorf("Provider = %q, want gemini", resp.Provider)
	}
	if resp.Model != "gemini-3-flash-preview" {
		t.Errorf("Model = %q, want the modelVersion the response reported", resp.Model)
	}
	if want := 227; resp.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d (promptTokenCount)", resp.InputTokens, want)
	}
	// candidatesTokenCount 193 + thoughtsTokenCount 649: Gemini's own
	// docs say "response pricing is the sum of output tokens and
	// thinking tokens", and this response's totalTokenCount (1069)
	// only balances when the 649 are counted.
	if want := 193 + 649; resp.OutputTokens != want {
		t.Errorf("OutputTokens = %d, want %d (candidatesTokenCount + thoughtsTokenCount, both billed as output)", resp.OutputTokens, want)
	}
	if resp.Latency <= 0 {
		t.Error("Latency was not measured")
	}

	var answer struct {
		Corrections []struct {
			Original string `json:"original"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(resp.JSON, &answer); err != nil {
		t.Fatalf("JSON is not the schema's shape: %v", err)
	}
	if len(answer.Corrections) != 2 {
		t.Fatalf("got %d corrections, want the 2 this captured response contains", len(answer.Corrections))
	}
	if got := answer.Corrections[0].Original; got != "面白いでした" {
		t.Errorf("first correction original = %q, want %q", got, "面白いでした")
	}
}

// TestParsesTheCapturedFallbackResponse pins the other captured body —
// and documents in an assertion the reason the fallback prompt no
// longer pastes "$schema": this real answer echoed it back.
func TestParsesTheCapturedFallbackResponse(t *testing.T) {
	srv, _ := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_promptfallback.json"))
	g := New(testConfig(srv.URL), nil)

	resp, err := g.GenerateStructured(context.Background(), structuredRequest(t, "exercise.v1"))
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if want := 515; resp.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d", resp.InputTokens, want)
	}
	if want := 322 + 452; resp.OutputTokens != want {
		t.Errorf("OutputTokens = %d, want %d", resp.OutputTokens, want)
	}
	if !json.Valid(resp.JSON) {
		t.Fatalf("fallback answer is not valid JSON: %s", resp.JSON)
	}
	if !strings.Contains(string(resp.JSON), `"$schema"`) {
		t.Skip("this fixture no longer echoes $schema; the fallback prompt's explicit instruction is then belt-and-braces")
	}
}

// TestMissingCandidatesIsAnError: a prompt-blocked or empty response
// must fail loudly so a route falls through to the next provider,
// rather than handing the caller an empty answer to validate.
func TestMissingCandidatesIsAnError(t *testing.T) {
	srv, _ := capturingServer(t, http.StatusOK, []byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	g := New(testConfig(srv.URL), nil)

	_, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1"))
	if err == nil {
		t.Fatal("want an error for a response with no candidates")
	}
	if !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("error %q does not mention the blockReason the API gave", err)
	}
}

// TestATruncatedAnswerIsAnError: a MAX_TOKENS candidate carries real
// text, just not all of it. Passing that half-object on would send
// aiutil's repair loop chasing a completion it cannot see; failing lets
// a route fall through to the next provider instead.
func TestATruncatedAnswerIsAnError(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"corrections\":[{\"original\":\"面"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":227,"candidatesTokenCount":2048},"modelVersion":"gemini-3-flash-preview"}`)
	srv, _ := capturingServer(t, http.StatusOK, body)
	g := New(testConfig(srv.URL), nil)

	_, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1"))
	if err == nil {
		t.Fatal("want an error for a truncated answer")
	}
	if !strings.Contains(err.Error(), "MAX_TOKENS") {
		t.Errorf("error %q does not name the finishReason", err)
	}
}

// --- the API key never leaks ---------------------------------------

// TestNon2xxIsWrappedWithoutLeakingTheKey: every error from this
// adapter can end up rendered on /ai, so the key must not reach one.
func TestNon2xxIsWrappedWithoutLeakingTheKey(t *testing.T) {
	body := []byte(`{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \"$schema\"","status":"INVALID_ARGUMENT"}}`)
	srv, _ := capturingServer(t, http.StatusBadRequest, body)
	g := New(testConfig(srv.URL), nil)

	_, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1"))
	if err == nil {
		t.Fatal("want an error for HTTP 400")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q does not report the status code", err)
	}
	if !strings.Contains(err.Error(), "Unknown name") {
		t.Errorf("error %q dropped the API's own explanation, which is the only way to diagnose a schema rejection", err)
	}
	assertNoKey(t, err.Error())
}

// TestAnErrorBodyContainingTheKeyIsRedacted is the paranoid case: the
// key travels in a header, so nothing Google returns should contain it
// — but the adapter quotes upstream bodies verbatim into errors that
// reach /ai, and "upstream will never echo it" is an assumption about
// somebody else's server, not a property of this code.
func TestAnErrorBodyContainingTheKeyIsRedacted(t *testing.T) {
	body := []byte(`{"error":{"message":"API key not valid: ` + testAPIKey + `"}}`)
	srv, _ := capturingServer(t, http.StatusForbidden, body)
	g := New(testConfig(srv.URL), nil)

	_, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1"))
	if err == nil {
		t.Fatal("want an error for HTTP 403")
	}
	assertNoKey(t, err.Error())
}

// TestAnUnparseable2xxBodyIsRedactedToo covers the one error path that
// quotes a body which is NOT an upstream error: a 200 that fails to
// decode. It reaches /ai the same way the others do.
func TestAnUnparseable2xxBodyIsRedactedToo(t *testing.T) {
	srv, _ := capturingServer(t, http.StatusOK, []byte(`<html>proxy says: key=`+testAPIKey+`</html>`))
	g := New(testConfig(srv.URL), nil)

	_, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1"))
	if err == nil {
		t.Fatal("want an error for an unparseable body")
	}
	assertNoKey(t, err.Error())
}

// TestTheKeyIsNeverPutInTheURL: a key in a query string ends up in
// proxy logs, browser history, and every http.Client error message,
// which is why this adapter uses the x-goog-api-key header.
func TestTheKeyIsNeverPutInTheURL(t *testing.T) {
	srv, got := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_schema.json"))
	g := New(testConfig(srv.URL), nil)

	if _, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1")); err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	assertNoKey(t, got.rawURL)
}

// TestTransportErrorsDoNotLeakTheKey covers the path where the request
// never reaches a server at all — the error there is built by net/http
// out of the URL, so this is really a second assertion that the key is
// not in it.
func TestTransportErrorsDoNotLeakTheKey(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1") // nothing listens here
	g := New(cfg, nil)

	_, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1"))
	if err == nil {
		t.Fatal("want a transport error")
	}
	assertNoKey(t, err.Error())
}

func assertNoKey(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, testAPIKey) {
		t.Fatalf("the API key leaked into %q", s)
	}
}

// --- runtime model switching ---------------------------------------

type fixedResolver struct{ model string }

func (r fixedResolver) Model(string) (string, string) { return r.model, "" }

// TestResolverOverridesTheModelPerCall is the /settings contract: the
// override must land on the very next request, without reconstructing
// the generator.
func TestResolverOverridesTheModelPerCall(t *testing.T) {
	srv, got := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_schema.json"))
	g := New(testConfig(srv.URL), fixedResolver{model: "gemini-2.5-flash"})

	if _, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1")); err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if want := "/v1beta/models/gemini-2.5-flash:generateContent"; got.path != want {
		t.Errorf("path = %q, want %q — a /settings override must reach the next call", got.path, want)
	}
}

// TestAnEmptyResolverOverrideFallsBackToConfig: settings.Service returns
// "" when no override row exists, which means "use APP_AI_GEMINI_MODEL",
// not "send an empty model name".
func TestAnEmptyResolverOverrideFallsBackToConfig(t *testing.T) {
	srv, got := capturingServer(t, http.StatusOK, fixture(t, "generatecontent_schema.json"))
	g := New(testConfig(srv.URL), fixedResolver{model: ""})

	if _, err := g.GenerateStructured(context.Background(), structuredRequest(t, "correction_result.v1")); err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if want := "/v1beta/models/gemini-3-flash-preview:generateContent"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
}

// --- tool calling ---------------------------------------------------

// TestCallWithToolsTranslatesToolSchemas: function declarations go
// through the same OpenAPI-subset translation as responseSchema —
// every tool in internal/tools carries additionalProperties:false,
// which the live API rejects.
func TestCallWithToolsTranslatesToolSchemas(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"list_recent_sessions","args":{"limit":3}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5},"modelVersion":"gemini-3-flash-preview"}`)
	srv, got := capturingServer(t, http.StatusOK, body)
	g := New(testConfig(srv.URL), nil)

	resp, err := g.CallWithTools(context.Background(), ai.ToolRequest{
		System: "investigate",
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "what did I write this week?"},
		},
		Tools: []ai.ToolDef{{
			Name:        "list_recent_sessions",
			Description: "recent sessions",
			Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}},"additionalProperties":false}`),
		}},
	})
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}

	req := decodeWire(t, got.body)
	if len(req.Tools) != 1 || len(req.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools were not sent as functionDeclarations: %+v", req.Tools)
	}
	params := req.Tools[0].FunctionDeclarations[0].Parameters
	if keysAnywhere(t, params)["additionalProperties"] {
		t.Error("a tool's parameters went out untranslated")
	}

	if len(resp.Turn.Invocations) != 1 {
		t.Fatalf("got %d invocations, want 1", len(resp.Turn.Invocations))
	}
	inv := resp.Turn.Invocations[0]
	if inv.Name != "list_recent_sessions" {
		t.Errorf("invocation name = %q", inv.Name)
	}
	if inv.ID == "" {
		t.Error("invocation has no ID — the agent-run loop correlates results by it")
	}
	if string(inv.Arguments) != `{"limit":3}` {
		t.Errorf("arguments = %s", inv.Arguments)
	}
}

// TestCallWithToolsReplaysAToolResult: Gemini takes tool output as a
// functionResponse part keyed by the function's NAME (it has no
// per-call id of its own), which is why the adapter has to remember
// which name each invented ID belonged to.
func TestCallWithToolsReplaysAToolResult(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"You wrote three entries."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5},"modelVersion":"gemini-3-flash-preview"}`)
	srv, got := capturingServer(t, http.StatusOK, body)
	g := New(testConfig(srv.URL), nil)

	resp, err := g.CallWithTools(context.Background(), ai.ToolRequest{
		Messages: []ai.ToolMessage{
			{Role: "user", Text: "what did I write?"},
			{Role: "assistant", Invocations: []ai.ToolInvocation{{ID: "call_0", Name: "list_recent_sessions", Arguments: json.RawMessage(`{"limit":3}`)}}},
			{Role: "tool", Results: []ai.ToolResult{{ID: "call_0", Content: `{"sessions":[]}`}}},
		},
		Tools: []ai.ToolDef{{Name: "list_recent_sessions", Schema: json.RawMessage(`{"type":"object","properties":{}}`)}},
	})
	if err != nil {
		t.Fatalf("CallWithTools: %v", err)
	}
	if resp.Turn.Text != "You wrote three entries." {
		t.Errorf("Text = %q", resp.Turn.Text)
	}

	var req struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text             string `json:"text"`
				FunctionResponse *struct {
					Name     string          `json:"name"`
					Response json.RawMessage `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(got.body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("got %d contents, want 3 (user, model, function response)", len(req.Contents))
	}
	if req.Contents[1].Role != "model" {
		t.Errorf("assistant turn role = %q, want model — Gemini's name for it", req.Contents[1].Role)
	}
	last := req.Contents[2]
	if last.Role != "user" {
		t.Errorf("tool-result turn role = %q, want user", last.Role)
	}
	if len(last.Parts) != 1 || last.Parts[0].FunctionResponse == nil {
		t.Fatalf("tool result was not sent as a functionResponse part: %+v", last.Parts)
	}
	if last.Parts[0].FunctionResponse.Name != "list_recent_sessions" {
		t.Errorf("functionResponse.name = %q, want the tool's name", last.Parts[0].FunctionResponse.Name)
	}
}

// TestCallWithToolsRejectsAnEmptyTurn: agentrun.Runner reads "no text,
// no calls" as the model being finished, so a blocked or truncated
// candidate would end an investigation with nothing and no explanation.
func TestCallWithToolsRejectsAnEmptyTurn(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0},"modelVersion":"gemini-3-flash-preview"}`)
	srv, _ := capturingServer(t, http.StatusOK, body)
	g := New(testConfig(srv.URL), nil)

	_, err := g.CallWithTools(context.Background(), ai.ToolRequest{
		Messages: []ai.ToolMessage{{Role: "user", Text: "hello"}},
	})
	if err == nil {
		t.Fatal("want an error for a turn carrying neither text nor a call")
	}
	if !strings.Contains(err.Error(), "MALFORMED_FUNCTION_CALL") {
		t.Errorf("error %q does not name the finishReason", err)
	}
}

// contains is a tiny helper so the assertions above read as prose.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestTranslateInfersStringTypeForUntypedEnum pins the fix for a
// production 500.
//
// correction_result.v2 writes the correction `type` field as
// {"enum": [...]} with no "type" of its own — legal JSON Schema, which
// infers the type from the members. Gemini's OpenAPI-shaped schema does
// not infer, and measured against the live API it SILENTLY IGNORES such
// an enum rather than rejecting it: the model then returned "incorrect"
// (a severity value) for the type field, which satisfied Gemini and
// failed JLP's own validation after repair and retry — a 500 with the
// answer thrown away.
//
// A rejected schema would have failed loudly on the first call. This one
// only failed in production, so it gets a test naming the shape.
func TestTranslateInfersStringTypeForUntypedEnum(t *testing.T) {
	raw := json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "kind":  {"enum": ["grammar", "particle"]},
	    "typed": {"type": "string", "enum": ["a", "b"]},
	    "mixed": {"enum": ["a", 2]},
	    "count": {"type": "integer"}
	  }
	}`)

	out, blockers, err := translateSchema(raw)
	if err != nil {
		t.Fatalf("translateSchema: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %v, want none", blockers)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal translated: %v", err)
	}
	props, _ := got["properties"].(map[string]any)

	kind, _ := props["kind"].(map[string]any)
	if kind["type"] != "string" {
		t.Errorf("untyped string enum: type = %v, want \"string\" — Gemini ignores an enum it cannot type", kind["type"])
	}
	if _, ok := kind["enum"]; !ok {
		t.Error("untyped string enum lost its enum during translation")
	}

	typed, _ := props["typed"].(map[string]any)
	if typed["type"] != "string" {
		t.Errorf("already-typed enum: type = %v, want it left as \"string\"", typed["type"])
	}

	mixed, _ := props["mixed"].(map[string]any)
	if _, typedNow := mixed["type"]; typedNow {
		t.Errorf("mixed-type enum gained a type %v, want none — no single correct answer", mixed["type"])
	}

	count, _ := props["count"].(map[string]any)
	if count["type"] != "integer" {
		t.Errorf("non-enum node was rewritten: %v", count)
	}
}

// TestTranslateRealCorrectionResultV2TypesItsEnums runs the real schema
// that broke production, not a hand-written stand-in.
func TestTranslateRealCorrectionResultV2TypesItsEnums(t *testing.T) {
	raw, err := schemas.Get("correction_result.v2")
	if err != nil {
		t.Fatalf("schemas.Get: %v", err)
	}
	out, blockers, err := translateSchema(raw)
	if err != nil {
		t.Fatalf("translateSchema: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %v, want none (v2 must use responseSchema, not the fallback)", blockers)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	corrections, _ := doc["properties"].(map[string]any)["corrections"].(map[string]any)
	items, _ := corrections["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)

	for _, field := range []string{"type", "severity"} {
		node, ok := itemProps[field].(map[string]any)
		if !ok {
			t.Fatalf("correction item has no %q property", field)
		}
		if _, hasEnum := node["enum"]; !hasEnum {
			continue
		}
		if node["type"] != "string" {
			t.Errorf("%s: type = %v, want \"string\" so Gemini honours the enum", field, node["type"])
		}
	}
}
