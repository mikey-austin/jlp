package a2a_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/a2a"
	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double (implements ai.ToolCaller), injected via agentrun.NewRunner the same way internal/application/agentrun's own tests do
	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// fakeRepo is a minimal in-memory storage.AgentRunRepository double —
// this package's own copy of application/agentrun's own test fixture
// of the same name/shape (see runner_test.go), needed here too since
// building a real *agentrun.Runner (this adapter's one collaborator
// besides *tools.Registry) requires one. It also records every Start
// call (System/Agent in particular) so
// TestSendMessageUsesHonestSystemPromptForToollessSkill can assert on
// what System prompt actually reached the runner.
type fakeRepo struct {
	mu      sync.Mutex
	started []storage.AgentRun
}

func (r *fakeRepo) Start(_ context.Context, run storage.AgentRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, run)
	return nil
}
func (r *fakeRepo) Finish(context.Context, learner.IdentityID, string, string, string, string, int, time.Time) error {
	return nil
}
func (r *fakeRepo) RecordToolCall(context.Context, learner.IdentityID, storage.ToolCall) error {
	return nil
}
func (r *fakeRepo) RecordTurn(context.Context, learner.IdentityID, storage.AgentTurn) error {
	return nil
}
func (r *fakeRepo) List(context.Context, learner.IdentityID, int) ([]storage.AgentRun, error) {
	return nil, nil
}
func (r *fakeRepo) Get(context.Context, learner.IdentityID, string) (storage.AgentRun, []storage.ToolCall, []storage.AgentTurn, error) {
	return storage.AgentRun{}, nil, nil, nil
}

// recordingTool is a tools.Tool whose handler records the identity
// it was actually invoked with — this test's way of pinning "a forged
// identity in the request params is never what the tool sees" end to
// end (mirroring internal/tools's own
// TestInvokeIgnoresForgedIdentityInArguments), through the FULL HTTP
// stack this time: httptest request → handleRPC → handleSendMessage →
// agentrun.Runner.Run → tools.Registry.Invoke → this handler.
type recordingTool struct {
	name string
	seen *[]learner.IdentityID
}

func (rt recordingTool) register(reg *tools.Registry) {
	reg.Register(tools.Tool{
		Def: ai.ToolDef{Name: rt.name, Description: "test tool", Schema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(_ context.Context, identity learner.IdentityID, _ *session.ID, _ json.RawMessage) (string, error) {
			*rt.seen = append(*rt.seen, identity)
			return `[{"subject_type":"concept","subject":"i-adjective-past","score":5,"reason":"recurring"}]`, nil
		},
	})
}

// newTestServer wires an a2a.Server over a real *agentrun.Runner
// (fakeai.New() as the ai.ToolCaller, fakeRepo{} as the trace
// repository) and a *tools.Registry that Allow()s ONLY "teacher" to
// call "get_learning_priorities" — the exact name fakeai's
// CallWithTools script always invokes first (see
// internal/adapters/fakeai's own doc comment), and the exact
// allowlist cmd/jlp/main.go's real wiring grants "teacher" — so
// chat/review_writing's underlying agent gets a real (non-error) tool
// result and analyse_learner/plan_lesson's ("summary"/"lesson", not
// Allow()ed here either, matching main.go's current wiring) get a
// refusal, exactly like production.
func newTestServer(t *testing.T) (*a2a.Server, *[]learner.IdentityID, *fakeRepo) {
	t.Helper()
	reg := tools.NewRegistry()
	var seen []learner.IdentityID
	recordingTool{name: "get_learning_priorities", seen: &seen}.register(reg)
	reg.Allow("teacher", "get_learning_priorities")

	repo := &fakeRepo{}
	runner := agentrun.NewRunner(fakeai.New(), reg, repo, time.Now)
	return a2a.New(runner, reg, config.A2A{Enabled: true, Path: "/a2a"}), &seen, repo
}

// ---------------------------------------------------------------------
// JSON-RPC plumbing for the tests
// ---------------------------------------------------------------------

// rpcEnvelope is the response side of JSON-RPC 2.0, as these tests read
// it back. Result stays raw so each test can decode it into whichever
// shape that METHOD returns — which is not uniform: SendMessage's
// result is enveloped (`{"task":…}`) and GetTask's is a bare Task. See
// TestSendMessageResultIsEnveloped.
type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// a2aTask mirrors the spec's Task closely enough to assert on, decoded
// from whatever the server actually put on the wire — deliberately a
// separate declaration from the adapter's own (unexported) Task rather
// than a reuse of it, so a test failure means "the WIRE shape changed",
// not "both sides were renamed together".
type a2aTask struct {
	ID        string `json:"id"`
	ContextID string `json:"contextId"`
	Status    struct {
		State     string  `json:"state"`
		Timestamp string  `json:"timestamp"`
		Message   *a2aMsg `json:"message"`
	} `json:"status"`
	Artifacts []struct {
		ArtifactID string    `json:"artifactId"`
		Name       string    `json:"name"`
		Parts      []a2aPart `json:"parts"`
	} `json:"artifacts"`
	History  []a2aMsg       `json:"history"`
	Metadata map[string]any `json:"metadata"`
}

type a2aMsg struct {
	MessageID string    `json:"messageId"`
	ContextID string    `json:"contextId"`
	TaskID    string    `json:"taskId"`
	Role      string    `json:"role"`
	Parts     []a2aPart `json:"parts"`
}

// a2aPart is the protobuf-oneof Part: the set field IS the
// discriminator. A `kind` key would be v0.3's shape and is asserted
// absent by TestPartsUseProtobufOneofShapeNotKindDiscriminator.
type a2aPart struct {
	Text *string `json:"text"`
	Kind *string `json:"kind"`
	// The other three oneof members, so a test can assert that exactly
	// one is set rather than that text specifically is.
	Data      any     `json:"data"`
	URL       *string `json:"url"`
	Raw       []byte  `json:"raw"`
	MediaType string  `json:"mediaType"`
}

// oneofMembers counts how many of the mutually exclusive content members
// this part carries. A well-formed v1.0 part carries exactly one.
func (p a2aPart) oneofMembers() int {
	n := 0
	for _, set := range []bool{p.Text != nil, p.Data != nil, p.URL != nil, p.Raw != nil} {
		if set {
			n++
		}
	}
	return n
}

func (p a2aPart) text() string {
	if p.Text == nil {
		return ""
	}
	return *p.Text
}

// rpc posts one JSON-RPC request to the adapter's endpoint as identity
// and decodes the envelope.
func rpc(t *testing.T, h http.Handler, identity learner.IdentityID, method string, params string) (*httptest.ResponseRecorder, rpcEnvelope) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":` + params + `}`
	return rpcRaw(t, h, identity, body)
}

// rpcRaw posts an arbitrary body, for the malformed/invalid cases.
func rpcRaw(t *testing.T, h http.Handler, identity learner.IdentityID, body string) (*httptest.ResponseRecorder, rpcEnvelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if identity != "" {
		req = req.WithContext(a2a.WithIdentity(req.Context(), identity))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var env rpcEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal JSON-RPC response (%s): %v", rec.Body.String(), err)
	}
	return rec, env
}

// textMessageParams builds SendMessage params in the exact shape the
// official client emits — verified by round-tripping a plain text send
// through @a2a-js/sdk@1.0.1's own SendMessageRequest.toJSON(), which
// produces precisely
// {"message":{"messageId":…,"role":"ROLE_USER","parts":[{"text":…}]}}:
// no `kind` on the part, the full enum NAME for the role, and every
// empty field omitted.
func textMessageParams(text string) string {
	b, _ := json.Marshal(text)
	return `{"message":{"messageId":"client-msg-1","role":"ROLE_USER","parts":[{"text":` + string(b) + `}]}}`
}

// sendAndDecode runs SendMessage and unwraps its `{"task":…}` envelope.
func sendAndDecode(t *testing.T, h http.Handler, identity learner.IdentityID, params string) a2aTask {
	t.Helper()
	rec, env := rpc(t, h, identity, "SendMessage", params)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if env.Error != nil {
		t.Fatalf("SendMessage returned error %d %q", env.Error.Code, env.Error.Message)
	}
	var result struct {
		Task *a2aTask `json:"task"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal SendMessage result: %v", err)
	}
	if result.Task == nil {
		t.Fatalf("SendMessage result has no `task` member — the official client throws \"Invalid response: missing payload\" on this: %s", env.Result)
	}
	return *result.Task
}

// ---------------------------------------------------------------------
// Agent card
// ---------------------------------------------------------------------

// cardOf fetches and decodes the agent card as a generic map, so the
// assertions below are about the JSON that actually went out rather
// than about the Go struct that produced it.
func cardOf(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	req.Host = "jlp.example:28080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("card status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var card map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	return card
}

// TestAgentCardHasRequiredV1Fields pins the card against A2A v1.0's
// required AgentCard fields — camelCase, and in particular
// `supportedInterfaces` rather than v0.3's top-level url/
// preferredTransport/protocolVersion. `supportedInterfaces` is the one
// the official client's transport selection actually reads: without it,
// ClientFactory throws "No compatible transport found" no matter how
// correct the rest of the card is.
func TestAgentCardHasRequiredV1Fields(t *testing.T) {
	srv, _, _ := newTestServer(t)
	card := cardOf(t, srv.Routes())

	for _, k := range []string{"name", "description", "version", "supportedInterfaces", "capabilities", "defaultInputModes", "defaultOutputModes", "skills"} {
		if _, ok := card[k]; !ok {
			t.Errorf("card is missing required v1.0 field %q", k)
		}
	}
	// The v0.3 spellings must be gone, not merely joined by the new
	// ones: a card carrying both would be lying about one of them.
	for _, k := range []string{"tasks_endpoint", "input_schema", "output_schema", "push_notifications", "preferredTransport", "protocolVersion"} {
		if _, ok := card[k]; ok {
			t.Errorf("card still carries retired/non-v1.0 top-level field %q", k)
		}
	}

	ifaces, _ := card["supportedInterfaces"].([]any)
	if len(ifaces) != 1 {
		t.Fatalf("len(supportedInterfaces) = %d, want 1", len(ifaces))
	}
	iface, _ := ifaces[0].(map[string]any)
	if got := iface["protocolBinding"]; got != "JSONRPC" {
		t.Errorf("protocolBinding = %v, want JSONRPC", got)
	}
	if got := iface["protocolVersion"]; got != "1.0" {
		t.Errorf("protocolVersion = %v, want 1.0 (the version this adapter implements)", got)
	}
	// Absolute, and pointing at the endpoint that actually serves RPC —
	// the client feeds this straight to fetch().
	if got := iface["url"]; got != "http://jlp.example:28080/a2a/v1" {
		t.Errorf("interface url = %v, want the absolute JSON-RPC endpoint under the mount path", got)
	}
}

// TestAgentCardAdvertisesOnlyCapabilitiesWeServe is the "an agent card
// is a contract with strangers" pin: this adapter serves no
// SendStreamingMessage and no push-notification method (see
// TestUnknownMethodReturnsMethodNotFound, which drives exactly those),
// so both capabilities must read false — and must be PRESENT, not
// omitted, so the denial is stated rather than inferred.
func TestAgentCardAdvertisesOnlyCapabilitiesWeServe(t *testing.T) {
	srv, _, _ := newTestServer(t)
	card := cardOf(t, srv.Routes())

	caps, ok := card["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is not an object: %v", card["capabilities"])
	}
	for _, k := range []string{"streaming", "pushNotifications"} {
		v, present := caps[k]
		if !present {
			t.Errorf("capabilities.%s is absent; it must be stated false, not omitted", k)
			continue
		}
		if v != false {
			t.Errorf("capabilities.%s = %v, want false (this adapter does not serve it)", k, v)
		}
	}
}

// TestAgentCardSkillsAreSpecShaped pins the skills array: v1.0's
// AgentSkill fields, in the documented order, with the conversational
// default first — and, the Rule 13 transparency this adapter exists to
// demonstrate, each skill's tool allowlist read LIVE off the same
// Registry this test configured above and surfaced as `tool:` tags
// (v1.0's AgentSkill has no other free-form field): chat/review_writing
// ("teacher") see exactly the one tool Allow()ed, analyse_learner/
// plan_lesson ("summary"/"lesson", not Allow()ed anything here) see
// none.
func TestAgentCardSkillsAreSpecShaped(t *testing.T) {
	srv, _, _ := newTestServer(t)
	card := cardOf(t, srv.Routes())

	raw, err := json.Marshal(card["skills"])
	if err != nil {
		t.Fatalf("re-marshal skills: %v", err)
	}
	var skills []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Examples    []string `json:"examples"`
		InputModes  []string `json:"inputModes"`
		OutputModes []string `json:"outputModes"`
	}
	if err := json.Unmarshal(raw, &skills); err != nil {
		t.Fatalf("unmarshal skills: %v", err)
	}

	wantIDs := []string{"chat", "review_writing", "analyse_learner", "plan_lesson"}
	if len(skills) != len(wantIDs) {
		t.Fatalf("len(skills) = %d, want %d", len(skills), len(wantIDs))
	}
	byID := map[string][]string{}
	for i, sk := range skills {
		if sk.ID != wantIDs[i] {
			t.Errorf("skills[%d].id = %q, want %q", i, sk.ID, wantIDs[i])
		}
		if sk.Name == "" || sk.Description == "" {
			t.Errorf("skills[%d] (%s) has an empty name or description", i, sk.ID)
		}
		if len(sk.Tags) == 0 || len(sk.Examples) == 0 {
			t.Errorf("skills[%d] (%s) must carry tags and examples", i, sk.ID)
		}
		if len(sk.InputModes) == 0 || len(sk.OutputModes) == 0 {
			t.Errorf("skills[%d] (%s) must declare inputModes/outputModes", i, sk.ID)
		}
		byID[sk.ID] = sk.Tags
	}

	hasToolTag := func(tags []string, name string) bool {
		for _, tg := range tags {
			if tg == "tool:"+name {
				return true
			}
		}
		return false
	}
	countToolTags := func(tags []string) int {
		n := 0
		for _, tg := range tags {
			if strings.HasPrefix(tg, "tool:") {
				n++
			}
		}
		return n
	}
	for _, id := range []string{"chat", "review_writing"} {
		if !hasToolTag(byID[id], "get_learning_priorities") || countToolTags(byID[id]) != 1 {
			t.Errorf("%s tags = %v, want exactly one tool: tag for get_learning_priorities (this test's own Registry.Allow)", id, byID[id])
		}
	}
	for _, id := range []string{"analyse_learner", "plan_lesson"} {
		if n := countToolTags(byID[id]); n != 0 {
			t.Errorf("%s has %d tool: tags, want 0 (no Allow() configured for its agent)", id, n)
		}
	}
}

// TestAgentCardDescriptionMatchesRealToolAccess pins whole-branch
// review I-3: the card's prose must agree with its own `tool:` tags.
//
// The mechanism already degraded honestly — a skill whose agent has no
// allowlist gets no tool: tags and runs on SystemWithoutTools — but the
// DESCRIPTION was a static string promising a result produced "after
// investigating the learner's history through the … agent's permitted
// tools". A remote client reading "Lesson Planner" expected a
// history-informed plan and got prose derived from its own message.
//
// Asserted against the two groups this test server deliberately
// configures differently (see newTestServer), so it stays true when the
// summary/lesson allowlists are eventually granted: a tool-less skill
// must say so and must NOT promise investigation; a tool-bearing skill
// must not carry the "NO tool access" disclaimer.
func TestAgentCardDescriptionMatchesRealToolAccess(t *testing.T) {
	srv, _, _ := newTestServer(t)
	card := cardOf(t, srv.Routes())

	raw, err := json.Marshal(card["skills"])
	if err != nil {
		t.Fatalf("re-marshal skills: %v", err)
	}
	var skills []struct {
		ID          string   `json:"id"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	}
	if err := json.Unmarshal(raw, &skills); err != nil {
		t.Fatalf("unmarshal skills: %v", err)
	}

	for _, sk := range skills {
		hasTools := false
		for _, tg := range sk.Tags {
			if strings.HasPrefix(tg, "tool:") {
				hasTools = true
			}
		}
		promisesTools := strings.Contains(sk.Description, "permitted tools") ||
			strings.Contains(sk.Description, "investigating")
		disclaimsTools := strings.Contains(sk.Description, "NO tool access")

		switch {
		case hasTools && disclaimsTools:
			t.Errorf("%s has tool: tags but its description disclaims tool access: %q", sk.ID, sk.Description)
		case !hasTools && promisesTools:
			t.Errorf("%s has NO tool: tags but its description promises tool-backed investigation: %q", sk.ID, sk.Description)
		case !hasTools && !disclaimsTools:
			t.Errorf("%s has NO tool: tags and its description never says so — a caller cannot tell from the prose: %q", sk.ID, sk.Description)
		}
	}
}

// ---------------------------------------------------------------------
// SendMessage
// ---------------------------------------------------------------------

// TestSendMessageReturnsSpecShapedTask is the happy path, driven
// through the real fakeai two-turn script (tool call, then prose naming
// the tool's returned subject): a completed, spec-shaped Task whose
// artifact actually reflects the underlying agent-run's investigation,
// not a stub.
func TestSendMessageReturnsSpecShapedTask(t *testing.T) {
	srv, _, _ := newTestServer(t)
	task := sendAndDecode(t, srv.Routes(), "mikey", textMessageParams("友達と映画を見ました。とても面白いでした。"))

	if task.ID == "" {
		t.Error("task.id is empty")
	}
	// contextId is required on a Task and the server mints one when the
	// client didn't supply it.
	if task.ContextID == "" {
		t.Error("task.contextId is empty; the server must generate one for a new context")
	}
	if task.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("status.state = %q, want TASK_STATE_COMPLETED (the full enum NAME, not \"completed\")", task.Status.State)
	}
	if task.Status.Timestamp == "" {
		t.Error("status.timestamp is empty")
	} else if _, err := time.Parse(time.RFC3339, task.Status.Timestamp); err != nil {
		t.Errorf("status.timestamp %q is not RFC 3339: %v", task.Status.Timestamp, err)
	}
	// One artifact, whose FIRST part is the prose answer. Widgets may
	// follow it (see widgets.go); what is pinned here is that the prose
	// leads and is complete on its own, because any client may ignore
	// the parts after it.
	if len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) == 0 {
		t.Fatalf("want exactly one artifact with at least one part, got %+v", task.Artifacts)
	}
	if task.Artifacts[0].Parts[0].Text == nil {
		t.Fatalf("the artifact's first part is not the prose answer: %+v", task.Artifacts[0].Parts[0])
	}
	if task.Artifacts[0].ArtifactID == "" {
		t.Error("artifact.artifactId is empty; it must be unique within the task")
	}
	if got := task.Artifacts[0].Parts[0].text(); !strings.Contains(got, "i-adjective-past") {
		t.Errorf("artifact text = %q, want it to mention i-adjective-past (the fake tool's returned priority)", got)
	}
	// History carries the exchange: the client's message, then ours.
	if len(task.History) != 2 {
		t.Fatalf("len(history) = %d, want 2 (the user's message and the agent's reply)", len(task.History))
	}
	if task.History[0].Role != "ROLE_USER" || task.History[1].Role != "ROLE_AGENT" {
		t.Errorf("history roles = %q/%q, want ROLE_USER/ROLE_AGENT (full enum names)", task.History[0].Role, task.History[1].Role)
	}
	if task.History[0].MessageID != "client-msg-1" {
		t.Errorf("history[0].messageId = %q, want the client's own id echoed back", task.History[0].MessageID)
	}
	for i, m := range task.History {
		if m.TaskID != task.ID || m.ContextID != task.ContextID {
			t.Errorf("history[%d] is not threaded onto the task (taskId=%q contextId=%q)", i, m.TaskID, m.ContextID)
		}
	}
}

// TestPartsUseProtobufOneofShapeNotKindDiscriminator is the pin against
// the single easiest way to get v1.0 wrong. A2A v1.0's data model is
// protobuf-derived, so a Part is a `oneof` spelled by FIELD NAME —
// `{"text":"…"}` — with no discriminator. v0.3's JSON-Schema model used
// `{"kind":"text","text":"…"}`, and that is what a from-memory
// implementation writes. The official client reads the field name and
// ignores `kind` entirely, so a `kind`-tagged part isn't merely
// non-canonical, it is how you ship parts nothing can read.
func TestPartsUseProtobufOneofShapeNotKindDiscriminator(t *testing.T) {
	srv, _, _ := newTestServer(t)
	task := sendAndDecode(t, srv.Routes(), "mikey", textMessageParams("こんにちは"))

	var parts []a2aPart
	parts = append(parts, task.Artifacts[0].Parts...)
	for _, m := range task.History {
		parts = append(parts, m.Parts...)
	}
	if len(parts) == 0 {
		t.Fatal("no parts emitted at all")
	}
	for i, p := range parts {
		if p.Kind != nil {
			t.Errorf("part[%d] carries a v0.3 \"kind\" discriminator (%q); v1.0 parts are a protobuf oneof identified by field name alone", i, *p.Kind)
		}
		// Exactly one oneof member, not "text specifically": a data part
		// carrying a widget legitimately has no text, and that IS the
		// protobuf oneof shape. What must never happen is a part with
		// two members set, or none.
		if n := p.oneofMembers(); n != 1 {
			t.Errorf("part[%d] sets %d oneof members, want exactly 1: %+v", i, n, p)
		}
	}
}

// TestSendMessageResultIsEnveloped pins the asymmetry that is easy to
// get backwards: SendMessage's `result` is a `task`|`message` oneof and
// MUST be enveloped (the official client runs it through
// SendMessageResponse.fromJSON and throws "Invalid response: missing
// payload" on a bare Task), whereas GetTask's result is a BARE Task
// (parsed with Task.fromJSON directly).
func TestSendMessageResultIsEnveloped(t *testing.T) {
	srv, _, _ := newTestServer(t)

	_, env := rpc(t, srv.Routes(), "mikey", "SendMessage", textMessageParams("こんにちは"))
	var sendResult map[string]json.RawMessage
	if err := json.Unmarshal(env.Result, &sendResult); err != nil {
		t.Fatalf("unmarshal SendMessage result: %v", err)
	}
	if _, ok := sendResult["task"]; !ok {
		t.Errorf("SendMessage result must be enveloped as {\"task\":…}, got %s", env.Result)
	}
	if _, ok := sendResult["id"]; ok {
		t.Errorf("SendMessage result looks like a BARE Task (has a top-level \"id\"); it must be enveloped: %s", env.Result)
	}

	// And the other half: GetTask must NOT be enveloped.
	var task a2aTask
	if err := json.Unmarshal(sendResult["task"], &task); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	_, getEnv := rpc(t, srv.Routes(), "mikey", "GetTask", `{"id":"`+task.ID+`"}`)
	var getResult map[string]json.RawMessage
	if err := json.Unmarshal(getEnv.Result, &getResult); err != nil {
		t.Fatalf("unmarshal GetTask result: %v", err)
	}
	if _, ok := getResult["task"]; ok {
		t.Errorf("GetTask result must be a BARE Task, not enveloped: %s", getEnv.Result)
	}
	if _, ok := getResult["id"]; !ok {
		t.Errorf("GetTask result has no top-level \"id\"; it must be a bare Task: %s", getEnv.Result)
	}
}

// TestSendMessageWithoutSkillRoutesToConversationalDefault pins the
// brief's requirement that a plain chat message — which is all the
// protocol lets a client send, since v1.0 has no skill selector on the
// wire — lands somewhere sensible rather than being rejected. The proof
// is which agent/prompt actually reached the runner.
func TestSendMessageWithoutSkillRoutesToConversationalDefault(t *testing.T) {
	srv, _, repo := newTestServer(t)
	task := sendAndDecode(t, srv.Routes(), "mikey", textMessageParams("「は」と「が」の違いを教えてください。"))

	if got := task.Metadata["skill"]; got != "chat" {
		t.Errorf("task.metadata.skill = %v, want chat (the conversational default)", got)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.started) != 1 {
		t.Fatalf("started = %d rows, want 1", len(repo.started))
	}
	if repo.started[0].PromptName != "a2a.chat" {
		t.Errorf("PromptName = %q, want a2a.chat", repo.started[0].PromptName)
	}
}

// TestSendMessageSelectsSkillFromMetadata pins the opt-in escape hatch:
// a client that HAS read the card and wants a specific skill selects it
// through the protocol's own metadata map, since v1.0 gives it no
// dedicated field.
func TestSendMessageSelectsSkillFromMetadata(t *testing.T) {
	srv, _, repo := newTestServer(t)
	params := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}],"metadata":{"skill":"plan_lesson"}}}`
	task := sendAndDecode(t, srv.Routes(), "mikey", params)

	if got := task.Metadata["skill"]; got != "plan_lesson" {
		t.Errorf("task.metadata.skill = %v, want plan_lesson", got)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.started[0].Agent != "lesson" {
		t.Errorf("Agent = %q, want lesson", repo.started[0].Agent)
	}
}

// TestSendMessageUnknownSkillFallsBackToDefault pins that an
// unrecognised metadata skill is not an error: an A2A client is
// entitled to know nothing about JLP's skill ids, and its message is
// still a perfectly good message.
func TestSendMessageUnknownSkillFallsBackToDefault(t *testing.T) {
	srv, _, _ := newTestServer(t)
	params := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}],"metadata":{"skill":"not_a_real_skill"}}}`
	task := sendAndDecode(t, srv.Routes(), "mikey", params)
	if got := task.Metadata["skill"]; got != "chat" {
		t.Errorf("task.metadata.skill = %v, want chat", got)
	}
}

// TestSendMessageEchoesClientContextID pins conversation threading: a
// client that supplies a contextId (its own conversation identifier)
// gets it back, so successive turns group together.
func TestSendMessageEchoesClientContextID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	params := `{"message":{"messageId":"m1","contextId":"my-conversation","role":"ROLE_USER","parts":[{"text":"hello"}]}}`
	task := sendAndDecode(t, srv.Routes(), "mikey", params)
	if task.ContextID != "my-conversation" {
		t.Errorf("contextId = %q, want the client's own echoed back", task.ContextID)
	}
}

// TestSendMessageAcceptsNumericRoleEnum pins the leniency in types.go's
// Role.UnmarshalJSON: protobuf JSON permits an enum as either its name
// or its integer tag, and a conforming client from another ecosystem
// may send `1`. Rejecting the whole request over a field this adapter
// doesn't even branch on would be a gratuitous interop break.
func TestSendMessageAcceptsNumericRoleEnum(t *testing.T) {
	srv, _, _ := newTestServer(t)
	params := `{"message":{"messageId":"m1","role":1,"parts":[{"text":"hello"}]}}`
	task := sendAndDecode(t, srv.Routes(), "mikey", params)
	if task.Status.State != "TASK_STATE_COMPLETED" {
		t.Errorf("state = %q, want TASK_STATE_COMPLETED", task.Status.State)
	}
}

// TestSendMessageWithNoTextPartReturnsInvalidParams: the one input this
// adapter genuinely cannot act on.
func TestSendMessageWithNoTextPartReturnsInvalidParams(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for name, params := range map[string]string{
		"no parts":   `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[]}}`,
		"blank text": `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"   "}]}}`,
		"file part":  `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"url":"https://example.com/a.pdf"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, env := rpc(t, srv.Routes(), "mikey", "SendMessage", params)
			if env.Error == nil || env.Error.Code != -32602 {
				t.Fatalf("error = %+v, want -32602 INVALID_PARAMS", env.Error)
			}
		})
	}
}

// ---------------------------------------------------------------------
// JSON-RPC envelope
// ---------------------------------------------------------------------

// TestUnknownMethodReturnsMethodNotFound drives every v1.0 method this
// adapter does NOT serve and pins -32601 for each — the honest answer,
// and the one consistent with the card's capabilities both reading
// false. If any of these ever starts working, the card must change in
// the same commit.
func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, method := range []string{
		"SendStreamingMessage", "SubscribeToTask", "ListTasks", "GetExtendedAgentCard",
		"CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig",
		"ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig",
		// v0.3's slash-separated spellings are not aliases: a
		// default-configured v1.0 client never sends them, and answering
		// them would imply a compatibility this adapter doesn't have.
		"message/send", "tasks/get", "tasks/cancel",
		"totally_made_up",
	} {
		t.Run(method, func(t *testing.T) {
			_, env := rpc(t, srv.Routes(), "mikey", method, `{}`)
			if env.Error == nil || env.Error.Code != -32601 {
				t.Fatalf("error = %+v, want -32601 METHOD_NOT_FOUND", env.Error)
			}
		})
	}
}

// TestMalformedJSONReturnsParseError pins -32700, with the null id
// JSON-RPC 2.0 requires when the request's own id could not be read.
func TestMalformedJSONReturnsParseError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, env := rpcRaw(t, srv.Routes(), "mikey", `{not json`)
	if env.Error == nil || env.Error.Code != -32700 {
		t.Fatalf("error = %+v, want -32700 PARSE_ERROR", env.Error)
	}
	if string(env.ID) != "null" {
		t.Errorf("id = %s, want null (the request's id was unreadable)", env.ID)
	}
}

// TestInvalidEnvelopeReturnsInvalidRequest pins -32600 for JSON that
// parses but isn't a JSON-RPC 2.0 request object.
func TestInvalidEnvelopeReturnsInvalidRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for name, body := range map[string]string{
		"wrong version": `{"jsonrpc":"1.0","id":1,"method":"SendMessage","params":{}}`,
		"no version":    `{"id":1,"method":"SendMessage","params":{}}`,
		"no method":     `{"jsonrpc":"2.0","id":1,"params":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, env := rpcRaw(t, srv.Routes(), "mikey", body)
			if env.Error == nil || env.Error.Code != -32600 {
				t.Fatalf("error = %+v, want -32600 INVALID_REQUEST", env.Error)
			}
		})
	}
}

// TestResponseEchoesRequestIDVerbatim pins the round trip the official
// client enforces with a strict `!==` against the number it sent: a
// numeric id must come back as the same JSON number, not restyled.
func TestResponseEchoesRequestIDVerbatim(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, id := range []string{`7`, `"abc"`, `0`} {
		t.Run(id, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":` + id + `,"method":"GetTask","params":{"id":"nope"}}`
			_, env := rpcRaw(t, srv.Routes(), "mikey", body)
			if string(env.ID) != id {
				t.Errorf("response id = %s, want %s verbatim", env.ID, id)
			}
			if env.JSONRPC != "2.0" {
				t.Errorf("jsonrpc = %q, want 2.0", env.JSONRPC)
			}
		})
	}
}

// TestOversizedBodyIsRejected is the MaxBytesReader pin: the JSON-RPC
// endpoint must cap its body the same way every sibling body-reading
// handler in internal/adapters/http already does (see jsonrpc.go's
// maxRequestBodyBytes), rather than buffering an unbounded body whose
// text gets forwarded verbatim into an LLM call.
func TestOversizedBodyIsRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	huge := textMessageParams(strings.Repeat("a", 2<<20))
	_, env := rpc(t, srv.Routes(), "mikey", "SendMessage", huge)
	if env.Error == nil {
		t.Fatalf("oversized body was accepted: %+v", env)
	}
	if env.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "too large") {
		t.Errorf("error message = %q, want it to name the size problem", env.Error.Message)
	}
}

// ---------------------------------------------------------------------
// GetTask / CancelTask, and Rule 13's identity guarantees
// ---------------------------------------------------------------------

// TestGetTaskRoundTripsSentTask pins GetTask reading back exactly what
// SendMessage already computed (see handleSendMessage's doc comment on
// synchronous execution — GetTask never finds a working task, only ever
// the already-final result), when the call comes from the SAME identity
// that created it.
func TestGetTaskRoundTripsSentTask(t *testing.T) {
	srv, _, _ := newTestServer(t)
	created := sendAndDecode(t, srv.Routes(), "mikey", textMessageParams("hello"))

	_, env := rpc(t, srv.Routes(), "mikey", "GetTask", `{"id":"`+created.ID+`"}`)
	if env.Error != nil {
		t.Fatalf("GetTask returned error %+v", env.Error)
	}
	var got a2aTask
	if err := json.Unmarshal(env.Result, &got); err != nil {
		t.Fatalf("unmarshal GetTask result: %v", err)
	}
	if got.ID != created.ID || got.ContextID != created.ContextID || got.Status.State != created.Status.State {
		t.Errorf("GetTask %+v, want it to match the SendMessage task %+v", got.Status, created.Status)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Parts[0].text() != created.Artifacts[0].Parts[0].text() {
		t.Errorf("GetTask artifacts differ from the ones SendMessage returned")
	}
}

// TestGetTaskUnknownReturnsTaskNotFound pins -32001, the spec's own
// named code, rather than a generic internal error.
func TestGetTaskUnknownReturnsTaskNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, env := rpc(t, srv.Routes(), "mikey", "GetTask", `{"id":"does-not-exist"}`)
	if env.Error == nil || env.Error.Code != -32001 {
		t.Fatalf("error = %+v, want -32001 TASK_NOT_FOUND", env.Error)
	}
}

// TestGetTaskIsIdentityScopedWithNoExistenceOracle is the carried-over
// Rule 13 pin, in the new shape: identity B must not be able to read
// identity A's task output — this being the one read path in the
// codebase that isn't identity-scoped otherwise (every
// storage.*Repository method takes an IdentityID; agent_runs itself is
// indexed (identity_id, started_at) for exactly this reason).
//
// And the sharper half: "not found" and "not yours" must be
// INDISTINGUISHABLE — identical code AND identical message — so the
// response can never be used as an oracle to confirm that a task id
// belongs to someone else.
func TestGetTaskIsIdentityScopedWithNoExistenceOracle(t *testing.T) {
	srv, _, _ := newTestServer(t)
	created := sendAndDecode(t, srv.Routes(), "identity-a", textMessageParams("友達と映画を見ました。"))

	// Sanity: the OWNING identity can read it back (otherwise this test
	// would trivially pass for the wrong reason).
	_, ownerEnv := rpc(t, srv.Routes(), "identity-a", "GetTask", `{"id":"`+created.ID+`"}`)
	if ownerEnv.Error != nil {
		t.Fatalf("owner GetTask returned error %+v", ownerEnv.Error)
	}

	_, notYours := rpc(t, srv.Routes(), "identity-b", "GetTask", `{"id":"`+created.ID+`"}`)
	_, notFound := rpc(t, srv.Routes(), "identity-b", "GetTask", `{"id":"a-task-id-that-never-existed"}`)
	if notYours.Error == nil || notFound.Error == nil {
		t.Fatalf("expected errors for both, got %+v / %+v", notYours.Error, notFound.Error)
	}
	if notYours.Error.Code != notFound.Error.Code || notYours.Error.Message != notFound.Error.Message {
		t.Errorf("\"not yours\" (%d %q) is distinguishable from \"not found\" (%d %q) — that's an existence oracle",
			notYours.Error.Code, notYours.Error.Message, notFound.Error.Code, notFound.Error.Message)
	}
}

// TestCancelTaskOnTerminalTaskIsNotCancelable pins the honest answer:
// every task this adapter creates is already terminal before the caller
// could learn its id, so -32002 is correct — never a silently
// successful no-op that would tell a client it had stopped work which
// in fact already finished.
func TestCancelTaskOnTerminalTaskIsNotCancelable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	created := sendAndDecode(t, srv.Routes(), "mikey", textMessageParams("hello"))

	_, env := rpc(t, srv.Routes(), "mikey", "CancelTask", `{"id":"`+created.ID+`"}`)
	if env.Error == nil || env.Error.Code != -32002 {
		t.Fatalf("error = %+v, want -32002 TASK_NOT_CANCELABLE", env.Error)
	}
}

// TestCancelTaskIsIdentityScopedWithNoExistenceOracle: cancelling
// someone else's task must answer TASK_NOT_FOUND, identically to an
// unknown id — NOT "not cancelable", which would confirm the task
// exists to a caller who cannot read it.
func TestCancelTaskIsIdentityScopedWithNoExistenceOracle(t *testing.T) {
	srv, _, _ := newTestServer(t)
	created := sendAndDecode(t, srv.Routes(), "identity-a", textMessageParams("hello"))

	_, notYours := rpc(t, srv.Routes(), "identity-b", "CancelTask", `{"id":"`+created.ID+`"}`)
	_, notFound := rpc(t, srv.Routes(), "identity-b", "CancelTask", `{"id":"never-existed"}`)
	if notYours.Error == nil || notYours.Error.Code != -32001 {
		t.Fatalf("cancelling another identity's task = %+v, want -32001 TASK_NOT_FOUND", notYours.Error)
	}
	if notYours.Error.Message != notFound.Error.Message || notYours.Error.Code != notFound.Error.Code {
		t.Errorf("\"not yours\" (%d %q) is distinguishable from \"not found\" (%d %q) — that's an existence oracle",
			notYours.Error.Code, notYours.Error.Message, notFound.Error.Code, notFound.Error.Message)
	}
}

// TestRPCRequiresAuthenticatedIdentity pins that a request reaching
// this adapter with NO context identity at all (should never happen in
// production — Routes() is only ever mounted inside server.go's
// authenticated group — but this adapter must not assume its caller got
// that right) is rejected rather than silently running as some
// zero-value identity. 401, because this is a transport-level
// authentication failure rather than a fault in an accepted call.
func TestRPCRequiresAuthenticatedIdentity(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec, env := rpc(t, srv.Routes(), "", "SendMessage", textMessageParams("hello"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if env.Error == nil {
		t.Error("401 response carries no JSON-RPC error object")
	}
}

// TestGetTaskWithNoIdentityIsRejectedBeforeAnyLookup pins the defensive
// half of the same guarantee: a READ reaching this adapter with no
// context identity must not be treated as "allowed to read anything".
// It is refused at the envelope, before the task map is consulted at
// all, so the response says nothing whatsoever about whether that id
// exists.
func TestGetTaskWithNoIdentityIsRejectedBeforeAnyLookup(t *testing.T) {
	srv, _, _ := newTestServer(t)
	created := sendAndDecode(t, srv.Routes(), "mikey", textMessageParams("hello"))

	rec, real := rpc(t, srv.Routes(), "", "GetTask", `{"id":"`+created.ID+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if real.Error == nil {
		t.Fatal("no error object")
	}
	// An id that exists and one that never did must be answered
	// identically to an unauthenticated caller — the rejection happens
	// before the task map is read, so it cannot depend on its contents.
	_, bogus := rpc(t, srv.Routes(), "", "GetTask", `{"id":"never-existed"}`)
	if bogus.Error == nil || real.Error.Code != bogus.Error.Code || real.Error.Message != bogus.Error.Message {
		t.Errorf("an existing id (%+v) is distinguishable from an unknown one (%+v) to an unauthenticated caller", real.Error, bogus.Error)
	}
}

// TestTaskCacheIsBounded pins the eviction Server.remember does: every
// cached Task holds a full model response, and the conversational
// default skill means a chat client can create them indefinitely, so an
// uncapped map would be a slow leak in a long-running process. Evicting
// the oldest is safe because this is a cache, not the source of truth —
// the evicted task's answer is "not found", the same answer a process
// restart already gives, while its agent_runs trace is untouched.
func TestTaskCacheIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("drives maxCachedTasks+1 full agent runs")
	}
	srv, _, _ := newTestServer(t)
	h := srv.Routes()

	first := sendAndDecode(t, h, "mikey", textMessageParams("first"))
	// 512 more sends must push the first one out (maxCachedTasks = 512).
	var last a2aTask
	for i := 0; i < 512; i++ {
		last = sendAndDecode(t, h, "mikey", textMessageParams("filler"))
	}

	_, evicted := rpc(t, h, "mikey", "GetTask", `{"id":"`+first.ID+`"}`)
	if evicted.Error == nil || evicted.Error.Code != -32001 {
		t.Errorf("the oldest task was not evicted: %+v", evicted)
	}
	// ...and the most recent one is of course still there.
	_, kept := rpc(t, h, "mikey", "GetTask", `{"id":"`+last.ID+`"}`)
	if kept.Error != nil {
		t.Errorf("the newest task was evicted: %+v", kept.Error)
	}
}

// TestSendMessageIgnoresForgedIdentityInParams is this adapter's
// required pin (mirroring internal/tools's
// TestInvokeIgnoresForgedIdentityInArguments), carried over from the
// retired shape into the new one: params that carry an "identity" field
// naming someone else — at the top level, inside the message, and
// inside its metadata — must have ZERO effect on which identity the run
// (and every tool call inside it) actually executes as. The request's
// real, context-supplied identity ("real-caller", set the same way
// internal/adapters/http/server.go's withA2AIdentity sets it from
// RequireIdentity's own resolution) is the only one that reaches the
// recordingTool handler, proven by asserting on *seen — not by
// inspecting the response, which never echoes an identity back at all.
func TestSendMessageIgnoresForgedIdentityInParams(t *testing.T) {
	srv, seen, repo := newTestServer(t)
	params := `{"identity":"someone-else","message":{"messageId":"m1","role":"ROLE_USER",` +
		`"parts":[{"text":"友達と映画を見ました。"}],"identity":"someone-else",` +
		`"metadata":{"identity":"someone-else","identity_id":"someone-else"}}}`

	task := sendAndDecode(t, srv.Routes(), "real-caller", params)
	if task.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("state = %q, want TASK_STATE_COMPLETED", task.Status.State)
	}
	if len(*seen) != 1 {
		t.Fatalf("tool invoked %d times, want exactly 1", len(*seen))
	}
	if (*seen)[0] != "real-caller" {
		t.Fatalf("tool saw identity %q, want %q (the forged \"identity\" fields must be ignored entirely)", (*seen)[0], "real-caller")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.started[0].IdentityID != "real-caller" {
		t.Fatalf("agent_runs row recorded identity %q, want real-caller", repo.started[0].IdentityID)
	}
}

// TestSendMessageUsesHonestSystemPromptForToollessSkill: analyse_learner
// /plan_lesson (agents "summary"/"lesson", not Allow()ed any tool in
// this test's Registry, matching cmd/jlp/main.go's current wiring) must
// not be told to "investigate using your available tools" when they
// provably have none — the same live reg.DefsFor signal card.go's
// tool: tags already use. review_writing ("teacher", which IS Allow()ed
// a tool here) is the control: its System prompt still carries the
// investigate-first instruction.
func TestSendMessageUsesHonestSystemPromptForToollessSkill(t *testing.T) {
	srv, _, repo := newTestServer(t)

	sendAndDecode(t, srv.Routes(), "mikey", `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"how am I doing?"}],"metadata":{"skill":"analyse_learner"}}}`)
	sendAndDecode(t, srv.Routes(), "mikey", `{"message":{"messageId":"m2","role":"ROLE_USER","parts":[{"text":"hello"}],"metadata":{"skill":"review_writing"}}}`)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.started) != 2 {
		t.Fatalf("started = %d rows, want 2", len(repo.started))
	}
	if toolless := repo.started[0].System; strings.Contains(toolless, "using your available tools") {
		t.Errorf("analyse_learner System = %q, must not claim tool access it doesn't have", toolless)
	}
	if withTools := repo.started[1].System; !strings.Contains(withTools, "using your available tools") {
		t.Errorf("review_writing System = %q, want it to still instruct investigation (it DOES have a tool)", withTools)
	}
}

// TestSessionIDComesFromMetadataNotContextID pins the deliberate
// non-mapping documented on sessionFrom: agent_runs.session_id is a
// real foreign key into JLP's `sessions` table, while an A2A contextId
// is an identifier the CLIENT invents for its own conversation
// threading. Feeding one to the other would turn every ordinary chat
// into a failed insert — so a contextId must never reach the runner's
// SessionID, and an explicitly-supplied session_id must.
func TestSessionIDComesFromMetadataNotContextID(t *testing.T) {
	srv, _, repo := newTestServer(t)

	sendAndDecode(t, srv.Routes(), "mikey", `{"message":{"messageId":"m1","contextId":"a-client-invented-context","role":"ROLE_USER","parts":[{"text":"hello"}]}}`)
	sendAndDecode(t, srv.Routes(), "mikey", `{"message":{"messageId":"m2","role":"ROLE_USER","parts":[{"text":"hello"}],"metadata":{"session_id":"11111111-1111-1111-1111-111111111111"}}}`)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.started[0].SessionID != nil {
		t.Errorf("contextId leaked into agent_runs.session_id as %v; it is not a JLP session id", *repo.started[0].SessionID)
	}
	if repo.started[1].SessionID == nil || string(*repo.started[1].SessionID) != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("SessionID = %v, want the metadata-supplied session id", repo.started[1].SessionID)
	}
}

// TestRetiredBespokeRoutesAreGone: the previous, non-protocol shape
// (POST {path}/tasks with {skill,input}, GET {path}/tasks/{id}) is
// retired rather than maintained alongside the real protocol. It had no
// external consumers; keeping it would mean two contracts to keep
// honest, and one of them unspeakable by any A2A client.
func TestRetiredBespokeRoutesAreGone(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Routes()
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/tasks"},
		{http.MethodGet, "/tasks/some-id"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"skill":"review_writing","input":"hi"}`))
		req = req.WithContext(a2a.WithIdentity(req.Context(), "mikey"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want the retired route to be absent", tc.method, tc.path, rec.Code)
		}
	}
}
