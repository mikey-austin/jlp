// Package gemini implements ai.StructuredGenerator and ai.ToolCaller
// against Google's Gemini API (POST
// {baseurl}/v1beta/models/{model}:generateContent), so a deployment
// with no local model worth using — venus, whose Ollama is
// embeddings-only — can still run every AI capability JLP has.
//
// The whole design turns on one thing that is NOT true of the other
// hosted-API adapter in this repo: Gemini's `responseSchema` is an
// OpenAPI-3.0 subset, not JSON Schema, so JLP's schemas cannot be
// passed through verbatim the way adapters/anthropic hands req.Schema
// to a tool's input_schema and adapters/ollama hands it to "format".
// Probed against the live API with this deployment's real key:
//
//  1. correction_result.v1 sent verbatim → HTTP 400, *Unknown name
//     "$schema"* and *Unknown name "additionalProperties"*. A
//     passthrough adapter would fail 100% of calls.
//  2. Removing $schema, additionalProperties and title is enough for 7
//     of JLP's 8 schemas (correction_result.v1: 4.6 s, 2 corrections,
//     227 in / 193 out; weekly_summary.v1: 4.0 s).
//  3. minLength is ACCEPTED — the 400 named only the two keys above, so
//     translateSchema deliberately keeps it rather than stripping every
//     unfamiliar keyword.
//  4. exercise.v1 genuinely cannot be expressed: its allOf/if/then/const
//     conditional block draws *Unknown name "if" at
//     'generation_config.response_schema.all_of[0]'*.
//  5. For that case, responseMimeType: application/json with NO
//     responseSchema and the schema pasted into the prompt returns valid
//     JSON in 3.8 s — but costs 515 input tokens against 227, because
//     the schema now rides in the prompt on every single call.
//
// So: translate and send as responseSchema when the schema survives
// translation, fall back to the prompt when it does not — decided by
// the KEYWORDS a schema contains (see untranslatableKeywords), never by
// its name, so a future schema that grows an `if` takes the right path
// without anyone editing a list here.
//
// Either way this adapter does NOT validate the answer: JLP's own
// aiutil.ValidateWithRepairAndRetry validates against the REAL schema,
// so additionalProperties:false is still enforced by us even though
// Gemini cannot express it. That is the same division of labour
// adapters/ollama documents for the same reason.
//
// The API key travels in the x-goog-api-key header and appears nowhere
// else: not in the URL (which would put it in every proxy log and every
// net/http error string), and not in an error message — errors from
// this adapter are rendered on /ai, and quoted upstream bodies are run
// through redactKey before they get there.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const provider = "gemini"

// defaultBaseURL mirrors config's own APP_AI_GEMINI_BASEURL default, so
// a hand-constructed config.Gemini (a test that only sets APIKey, or a
// ModelLister built from one) still targets the real API.
const defaultBaseURL = "https://generativelanguage.googleapis.com"

// defaultTimeout bounds one generateContent round trip when config
// supplies none. The zero value of http.Client has no timeout at all,
// so this must exist: a wedged connection would otherwise hang the
// handler goroutine forever. Two minutes is generous against the ~4 s
// the measured calls took, with room for a thinking-heavy answer.
const defaultTimeout = 2 * time.Minute

// maxErrorBodyExcerpt caps how much of an upstream error body an error
// message quotes. Gemini's 400s carry the one thing worth having — the
// name of the rejected field — in the first line.
const maxErrorBodyExcerpt = 1000

type generator struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
	// resolver, when non-nil, is consulted on every call (see
	// resolveModel) so a /settings override reaches the very next
	// request without reconstructing this generator. nil means "no
	// resolver wired up" — the model is always cfg.Model.
	resolver ai.ModelResolver
}

// New returns a value implementing BOTH ai.StructuredGenerator and
// ai.ToolCaller against the Gemini API — the concrete type rather than
// either interface alone, for the same reason adapters/anthropic.New
// returns one: cmd/jlp's buildAIGenerator and buildToolCaller each need
// their own view of the same value.
//
// Both interfaces are genuinely required here, not optional polish:
// APP_AI_PROVIDER=gemini is a supported configuration, and cmd/jlp's
// buildToolCaller treats a default provider that is not an
// ai.ToolCaller as a boot error. A structured-generation-only Gemini
// adapter would make the very deployment this exists for fail to start.
//
// resolver (may be nil) is consulted at CALL time, never cached here.
func New(cfg config.Gemini, resolver ai.ModelResolver) *generator {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &generator{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		client:   &http.Client{Timeout: timeout},
		resolver: resolver,
	}
}

// resolveModel returns the model this call should use: the resolver's
// current override for "gemini" when it has one, else the
// APP_AI_GEMINI_MODEL value this generator was constructed with. Called
// fresh on every call — never cached — so a /settings change takes
// effect on the very next request with no restart.
func (g *generator) resolveModel() string {
	if g.resolver != nil {
		if m, _ := g.resolver.Model(provider); m != "" {
			return m
		}
	}
	return g.model
}

// --- wire types ------------------------------------------------------

// generateRequest mirrors the generateContent request body. Only the
// fields this adapter sends; every one of them was exercised against
// the live API.
type generateRequest struct {
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Contents          []content         `json:"contents"`
	Tools             []tool            `json:"tools,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

// content is one conversation turn. Role is "user" or "model" —
// Gemini's name for the assistant — and is omitted on
// systemInstruction, which is a bare parts wrapper with no role.
type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

// part is one piece of a turn: prose, a call the model made, or the
// result of running one. Exactly one field is ever set.
type part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type functionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// functionResponse feeds one tool's output back. Note what it does NOT
// have: a per-call id. Gemini correlates a result to a call by the
// function's NAME, which is why toGeminiContents has to look an
// ai.ToolResult's ID back up in the invocations that preceded it.
type functionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

// functionDeclaration's Parameters is subject to exactly the same
// OpenAPI-subset restriction as responseSchema, so tool schemas go
// through translateSchema too — every tool in internal/tools carries
// additionalProperties:false, which the live API rejects.
type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// generationConfig deliberately carries no maxOutputTokens, even though
// ai.StructuredRequest has a MaxTokens field the Anthropic adapter maps
// straight onto its own max_tokens. On this model the budget is shared
// with thinking: the captured schema-path response spent 649 thinking
// tokens to produce 193 tokens of answer, so a caller's 2048 — a number
// sized for answer text — would silently truncate the answer to nothing
// and report finishReason MAX_TOKENS. Leaving it unset lets the model's
// own output limit apply. Temperature is likewise unset: the measured
// calls that produced testdata/ used the model's default, and pinning a
// value nobody measured would make those fixtures describe a request
// this adapter no longer sends.
type generationConfig struct {
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

// generateResponse mirrors the response body, confirmed against the two
// captured live transcripts in testdata/.
type generateResponse struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		// ThoughtsTokenCount is billed at the output rate — Gemini's own
		// docs: "response pricing is the sum of output tokens and
		// thinking tokens" — and the captured transcript only balances
		// when it is counted (227 + 193 + 649 = totalTokenCount 1069).
		// Folding it into the reported OutputTokens is what keeps /ai's
		// cost column honest; reporting candidatesTokenCount alone would
		// have understated that call by 3.4x.
		ThoughtsTokenCount int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	// ModelVersion is the model that actually served the request —
	// preferred over the requested name, same reasoning as
	// adapters/ollama's chatResponse.Model.
	ModelVersion   string `json:"modelVersion"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback,omitempty"`
}

// text concatenates every text part of the first candidate. A part can
// legitimately carry no text at all (the captured responses each pair
// their text with a thoughtSignature), so this skips rather than
// assumes parts[0].
func (r generateResponse) text() string {
	if len(r.Candidates) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range r.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// --- schema translation ---------------------------------------------

// strippedKeywords are the three keys measured to make the live API
// return 400. They are dropped from a translated schema; every other
// keyword — minLength included, which the same 400 pointedly did not
// name — is passed through untouched.
var strippedKeywords = map[string]bool{
	"$schema":              true,
	"additionalProperties": true,
	"title":                true,
}

// untranslatableKeywords are the JSON Schema constructs that have no
// OpenAPI-3.0-subset equivalent: a schema containing any of them cannot
// be sent as a responseSchema at all, and takes the prompt fallback
// instead. `if` is the one measured directly (exercise.v1's 400); the
// rest are its structural siblings, listed so a future schema takes the
// working path automatically instead of 400ing in production.
//
// anyOf is deliberately ABSENT: it is part of the OpenAPI subset Gemini
// accepts, and listing it here would push a perfectly expressible
// schema onto the path that costs 2.3x the input tokens.
var untranslatableKeywords = map[string]bool{
	"allOf":             true,
	"if":                true,
	"then":              true,
	"else":              true,
	"oneOf":             true,
	"not":               true,
	"$ref":              true,
	"patternProperties": true,
}

// containerKeywords are the keywords whose VALUE is a map of names to
// subschemas. Their keys are data — a property may legitimately be
// called "if" — so the walk must not read them as keywords, or a
// harmless schema would be both mangled and pushed onto the expensive
// fallback.
var containerKeywords = map[string]bool{
	"properties":        true,
	"patternProperties": true,
	"$defs":             true,
	"definitions":       true,
}

// translateSchema converts one JSON Schema into the OpenAPI-3.0 subset
// Gemini's responseSchema (and functionDeclaration.parameters) accepts,
// and reports every untranslatable keyword it found. A non-empty
// blockers slice means the caller must NOT send the returned document —
// take the prompt fallback instead. blockers is sorted so an error
// message or a test reads the same way every time.
//
// A schema is decoded with UseNumber so integer literals survive: the
// ordinary interface{} decoding turns every number into a float64, and
// a large one re-marshals as 1e+06, which is not an integer to any JSON
// Schema reader.
func translateSchema(raw json.RawMessage) (json.RawMessage, []string, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var node any
	if err := dec.Decode(&node); err != nil {
		return nil, nil, fmt.Errorf("gemini: schema is not valid JSON: %w", err)
	}

	found := map[string]bool{}
	translated := walkSchema(node, false, found)

	blockers := make([]string, 0, len(found))
	for k := range found {
		blockers = append(blockers, k)
	}
	sort.Strings(blockers)

	out, err := json.Marshal(translated)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: marshal translated schema: %w", err)
	}
	return out, blockers, nil
}

// walkSchema rewrites one node. namesNotKeywords is true when this
// node's keys are property names rather than schema keywords (see
// containerKeywords) — the distinction that keeps a property called
// "if" from being mistaken for the conditional keyword.
func walkSchema(node any, namesNotKeywords bool, found map[string]bool) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			if namesNotKeywords {
				out[key] = walkSchema(child, false, found)
				continue
			}
			if strippedKeywords[key] {
				continue
			}
			if untranslatableKeywords[key] {
				// Recorded and DROPPED: the caller will discard this whole
				// document in favour of the prompt fallback, and emitting a
				// keyword measured to draw a 400 would be the one thing
				// guaranteed to break if it ever were sent. The subtree is
				// still walked, with its rewrite thrown away, so nested
				// blockers land in the report too — exercise.v1's `if`/`then`
				// live INSIDE its allOf, and an error naming only the outer
				// allOf would send the next reader looking in the wrong place.
				found[key] = true
				walkSchema(child, false, found)
				continue
			}
			out[key] = walkSchema(child, containerKeywords[key], found)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, child := range v {
			out = append(out, walkSchema(child, false, found))
		}
		return out
	default:
		return v
	}
}

// promptSchema prepares a schema for the PROMPT fallback, which is a
// different job from translateSchema's. Nothing here has to satisfy
// Gemini's OpenAPI subset — prose can say anything — so the document
// pasted into the prompt keeps everything translateSchema had to throw
// away: the allOf/if/then that made the schema untranslatable in the
// first place, and the additionalProperties:false that tells the model
// not to invent keys. Stripping those would paste a WEAKER contract
// than the one aiutil.ValidateWithRepairAndRetry then validates
// against, which is how a fallback turns into a repair loop.
//
// The single exception is $schema, and it is not theoretical: the
// captured live fallback response
// (testdata/generatecontent_promptfallback.json) was produced by a
// prompt containing the raw schema, and the model echoed "$schema"
// straight back into its own answer object — where
// correction_result.v1's additionalProperties:false rejects it,
// costing a repair round trip on every single call.
func promptSchema(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var node any
	if err := dec.Decode(&node); err != nil {
		return "", fmt.Errorf("gemini: schema is not valid JSON: %w", err)
	}
	out, err := json.Marshal(stripDollarSchema(node, false))
	if err != nil {
		return "", fmt.Errorf("gemini: marshal prompt schema: %w", err)
	}
	return string(out), nil
}

// stripDollarSchema removes every $schema KEYWORD, leaving a property
// legitimately named "$schema" alone — the same names-versus-keywords
// distinction walkSchema makes, for the same reason.
func stripDollarSchema(node any, namesNotKeywords bool) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			if !namesNotKeywords && key == "$schema" {
				continue
			}
			out[key] = stripDollarSchema(child, !namesNotKeywords && containerKeywords[key])
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, child := range v {
			out = append(out, stripDollarSchema(child, false))
		}
		return out
	default:
		return v
	}
}

// schemaInstruction introduces the pasted schema. Both prohibitions
// restate in prose what Gemini is not being asked to enforce on this
// path: "no keys the schema does not define" is additionalProperties,
// which the API could not express even when it WAS in a responseSchema,
// and the $schema sentence is the direct fix for the echo described on
// promptSchema above.
const schemaInstruction = "\n\nRespond with ONLY a JSON object matching this schema. " +
	"Include no keys the schema does not define, and do not copy the \"$schema\" key into your answer:\n"

// --- structured generation -------------------------------------------

func (g *generator) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()
	reqModel := g.resolveModel()

	fail := func(format string, args ...any) (ai.StructuredResponse, error) {
		return ai.StructuredResponse{Provider: provider, Model: reqModel},
			fmt.Errorf("gemini: "+format, args...)
	}

	translated, blockers, err := translateSchema(req.Schema)
	if err != nil {
		return fail("%s: %w", req.SchemaName, err)
	}

	userText := req.User
	cfg := &generationConfig{ResponseMIMEType: "application/json"}
	switch {
	case len(translated) == 0:
		// No schema at all: nothing to constrain beyond the JSON mime type.
	case len(blockers) == 0:
		cfg.ResponseSchema = translated
	default:
		// The prompt fallback: no responseSchema at all, the schema in the
		// prompt instead. What gets pasted is promptSchema's document, NOT
		// translated — see promptSchema's own comment for why the weaker,
		// translated one would be the wrong thing to send here.
		pasted, err := promptSchema(req.Schema)
		if err != nil {
			return fail("%s: %w", req.SchemaName, err)
		}
		userText += schemaInstruction + pasted
	}

	body := generateRequest{
		Contents:         []content{{Role: "user", Parts: []part{{Text: userText}}}},
		GenerationConfig: cfg,
	}
	if req.System != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: req.System}}}
	}

	resp, err := g.do(ctx, reqModel, body)
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: reqModel}, err
	}

	answer := resp.text()
	if answer == "" {
		return fail("response carried no text (%s)", resp.diagnosis())
	}
	// A truncated answer is not an answer. Handing the caller half a JSON
	// object sends it into aiutil's repair loop, which cannot recover the
	// missing half from anything it can see; failing here lets a route
	// fall through to the next provider instead of paying for a retry
	// that was never going to work. This is the failure mode a thinking
	// model reaches first — the captured call spent 649 tokens thinking
	// before it wrote a word, all of it against the same output budget.
	if reason := resp.finishReason(); reason != "" && reason != "STOP" {
		return fail("answer was cut short (finishReason %s)", reason)
	}

	return ai.StructuredResponse{
		JSON:         json.RawMessage(answer),
		Provider:     provider,
		Model:        resp.model(reqModel),
		InputTokens:  resp.UsageMetadata.PromptTokenCount,
		OutputTokens: resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount,
		Latency:      time.Since(start),
	}, nil
}

// --- tool calling -----------------------------------------------------

// CallWithTools implements ai.ToolCaller against the same endpoint,
// offering req.Tools as real function declarations instead of
// constraining a single JSON object — the multi-turn shape Phase 4's
// agentic teacher and the A2A skills need. There is no prompt fallback
// here: a function declaration's parameters have nowhere to fall back
// TO, so an untranslatable tool schema is an error naming the exact
// keyword, rather than a request that would 400 with a message about a
// field the operator never wrote.
func (g *generator) CallWithTools(ctx context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	start := time.Now()
	reqModel := g.resolveModel()

	fail := func(format string, args ...any) (ai.ToolResponse, error) {
		return ai.ToolResponse{Provider: provider, Model: reqModel},
			fmt.Errorf("gemini: "+format, args...)
	}

	decls := make([]functionDeclaration, 0, len(req.Tools))
	for _, d := range req.Tools {
		params, blockers, err := translateSchema(d.Schema)
		if err != nil {
			return fail("tool %q: %w", d.Name, err)
		}
		if len(blockers) > 0 {
			return fail("tool %q: its schema uses %v, which Gemini's function declarations cannot express", d.Name, blockers)
		}
		decls = append(decls, functionDeclaration{Name: d.Name, Description: d.Description, Parameters: params})
	}

	body := generateRequest{Contents: toGeminiContents(req.Messages)}
	if req.System != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: req.System}}}
	}
	if len(decls) > 0 {
		body.Tools = []tool{{FunctionDeclarations: decls}}
	}

	resp, err := g.do(ctx, reqModel, body)
	if err != nil {
		return ai.ToolResponse{Provider: provider, Model: reqModel}, err
	}

	turn := ai.ToolTurn{}
	if len(resp.Candidates) > 0 {
		for i, p := range resp.Candidates[0].Content.Parts {
			turn.Text += p.Text
			if p.FunctionCall != nil {
				// Gemini's functionCall carries no id of its own, so — like
				// adapters/ollama — this invents an index-based one purely
				// for the CALLER's correlation bookkeeping. It is never sent
				// back: toGeminiContents resolves it to the function name
				// Gemini actually correlates on.
				turn.Invocations = append(turn.Invocations, ai.ToolInvocation{
					ID:        fmt.Sprintf("call_%d", i),
					Name:      p.FunctionCall.Name,
					Arguments: p.FunctionCall.Args,
				})
			}
		}
	}

	// An empty turn — no prose, no calls — is not "the model considers
	// the conversation finished"; agentrun.Runner would read it as
	// exactly that and stop with nothing. It means the candidate was
	// blocked, truncated, or (Gemini's own MALFORMED_FUNCTION_CALL) cut
	// off mid-call, so say which.
	if turn.Text == "" && len(turn.Invocations) == 0 {
		return fail("response carried neither text nor a function call (%s)", resp.diagnosis())
	}

	return ai.ToolResponse{
		Turn:         turn,
		Provider:     provider,
		Model:        resp.model(reqModel),
		InputTokens:  resp.UsageMetadata.PromptTokenCount,
		OutputTokens: resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount,
		Latency:      time.Since(start),
	}, nil
}

// toGeminiContents replays a ToolRequest's conversation into Gemini's
// own turn shape: a "user" ToolMessage becomes a user turn; an
// "assistant" ToolMessage becomes a "model" turn carrying its Text plus
// one functionCall part per Invocation; a "tool" ToolMessage becomes a
// USER turn (Gemini has no tool role — results are user-supplied data)
// carrying one functionResponse part per Result.
//
// The lookup from ToolResult.ID to a function name is what makes that
// last step work: ai.ToolResult correlates by the id the invocation
// carried, while functionResponse correlates by name, so the names seen
// in the preceding assistant turn are carried forward as the map
// between them.
func toGeminiContents(msgs []ai.ToolMessage) []content {
	out := make([]content, 0, len(msgs))
	nameByID := map[string]string{}

	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			parts := make([]part, 0, 1+len(m.Invocations))
			if m.Text != "" {
				parts = append(parts, part{Text: m.Text})
			}
			for _, inv := range m.Invocations {
				nameByID[inv.ID] = inv.Name
				parts = append(parts, part{FunctionCall: &functionCall{Name: inv.Name, Args: inv.Arguments}})
			}
			if len(parts) > 0 {
				out = append(out, content{Role: "model", Parts: parts})
			}
		case "tool":
			parts := make([]part, 0, len(m.Results))
			for _, r := range m.Results {
				name := nameByID[r.ID]
				if name == "" {
					// An unmatched id means the caller replayed a result whose
					// call is not in this conversation. Sending it under an
					// empty name would be rejected; sending it as prose at
					// least preserves the information for the model.
					parts = append(parts, part{Text: r.Content})
					continue
				}
				parts = append(parts, part{FunctionResponse: &functionResponse{
					Name:     name,
					Response: toolResponsePayload(r),
				}})
			}
			if len(parts) > 0 {
				out = append(out, content{Role: "user", Parts: parts})
			}
		default: // "user"
			out = append(out, content{Role: "user", Parts: []part{{Text: m.Text}}})
		}
	}
	return out
}

// toolResponsePayload wraps a tool's output in the JSON OBJECT
// functionResponse.response requires. A tool that already returned an
// object is passed through unchanged; anything else (a bare string, an
// array, an error message) is wrapped, since sending a non-object there
// is rejected outright.
func toolResponsePayload(r ai.ToolResult) json.RawMessage {
	trimmed := strings.TrimSpace(r.Content)
	if !r.IsError && strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	key := "output"
	if r.IsError {
		key = "error"
	}
	wrapped, err := json.Marshal(map[string]string{key: r.Content})
	if err != nil { // unreachable: a map[string]string always marshals
		return json.RawMessage(`{"output":""}`)
	}
	return wrapped
}

// --- transport --------------------------------------------------------

// do performs one generateContent call. Every error it returns has
// already been through redactKey, and the key itself only ever reaches
// the request as a header value.
func (g *generator) do(ctx context.Context, model string, body generateRequest) (generateResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return generateResponse{}, fmt.Errorf("gemini: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", g.baseURL, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return generateResponse{}, fmt.Errorf("gemini: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	httpResp, err := g.client.Do(httpReq)
	if err != nil {
		return generateResponse{}, fmt.Errorf("gemini: generateContent: %s", g.redactKey(err.Error()))
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return generateResponse{}, fmt.Errorf("gemini: read response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return generateResponse{}, fmt.Errorf("gemini: generateContent: status %d: %s",
			httpResp.StatusCode, g.redactKey(excerpt(string(raw))))
	}

	var resp generateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return generateResponse{}, fmt.Errorf("gemini: unmarshal response: %v: %s",
			err, g.redactKey(excerpt(string(raw))))
	}
	return resp, nil
}

// model reports the model that actually served the request, falling
// back to the one asked for when the response does not say.
func (r generateResponse) model(requested string) string {
	if r.ModelVersion != "" {
		return r.ModelVersion
	}
	return requested
}

// finishReason reports the first candidate's finish reason, "" when the
// response named none.
func (r generateResponse) finishReason() string {
	if len(r.Candidates) == 0 {
		return ""
	}
	return r.Candidates[0].FinishReason
}

// diagnosis explains an answerless response: a blocked prompt names its
// blockReason, a truncated or filtered candidate names its finishReason.
func (r generateResponse) diagnosis() string {
	if r.PromptFeedback != nil && r.PromptFeedback.BlockReason != "" {
		return "blockReason " + r.PromptFeedback.BlockReason
	}
	if len(r.Candidates) == 0 {
		return "no candidates"
	}
	if reason := r.Candidates[0].FinishReason; reason != "" {
		return "finishReason " + reason
	}
	return "no parts"
}

// redactKey removes the API key from any string headed for an error
// message. Nothing Google returns should contain it — it travels in a
// header, never the URL or body — but "upstream will never echo it" is
// an assumption about somebody else's server, and these errors are
// rendered on /ai.
func (g *generator) redactKey(s string) string {
	if g.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, g.apiKey, "[REDACTED]")
}

// excerpt caps quoted upstream output. Gemini's 400s put the useful
// part — the name of the rejected field — at the front, so this keeps
// the head, unlike adapters/agycli's tailExcerpt.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty body)"
	}
	if len(s) > maxErrorBodyExcerpt {
		return s[:maxErrorBodyExcerpt] + "..."
	}
	return s
}
