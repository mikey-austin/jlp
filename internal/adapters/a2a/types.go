package a2a

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// This file is the A2A **v1.0** data model, transcribed field-for-field
// from the two primary sources (checked 2026-08-16, both agreeing):
//
//   - the specification at https://a2a-protocol.org/latest/specification/
//     ("The latest released version is 1.0.0"), and
//   - the official client's own generated types, `@a2a-js/sdk@1.0.1`
//     (dist/a2a-*.d.ts + the `fromJSON`/`toJSON` pair compiled into
//     dist/client/index.js), which is the thing that actually has to
//     parse what we emit.
//
// A2A v1.0's data model is derived from protobuf (`lf.a2a.v1`), so its
// JSON is **protobuf JSON**, not the hand-rolled JSON Schema shape v0.3
// used. Three consequences drive every type below, and all three are
// places a from-memory implementation gets it wrong:
//
//  1. A `Part` is a protobuf `oneof`. It is spelled by FIELD NAME —
//     `{"text": "…"}` — with NO `kind`/`type` discriminator anywhere.
//  2. Enums are spelled by their full protobuf enum-value name:
//     `"ROLE_USER"`, `"TASK_STATE_COMPLETED"` — not `"user"`/`"completed"`.
//  3. The card declares its endpoints as a LIST, `supportedInterfaces`,
//     each entry carrying its own url/protocolBinding/protocolVersion —
//     not a top-level `url` + `preferredTransport` + `protocolVersion`.
//
// Every shape here was verified by round-tripping it through the SDK's
// own `fromJSON`/`toJSON` rather than read off prose; see
// docs/api/a2a.md for the human-readable contract.

// Protocol version this adapter speaks, as the `Major.Minor` string the
// spec (§"Protocol Version") and the SDK's own A2A_PROTOCOL_VERSION use
// — advertised per-interface on the agent card, since v1.0 scopes the
// version to an interface rather than to the card as a whole.
const protocolVersion = "1.0"

// Protocol binding name for the JSON-RPC 2.0 transport. The client
// matches this case-insensitively against its registered transport
// factories (`JSONRPC`, `HTTP+JSON`, `GRPC`); `JSONRPC` is the one this
// adapter implements and the SDK's own first preference.
const protocolBindingJSONRPC = "JSONRPC"

// TaskState values, verbatim from the spec's TaskState enum. Only the
// three terminal ones this adapter can ever reach are named: task
// execution here is synchronous (see task.go's handleSendMessage), so a
// Task is already in a terminal state by the time any client can
// observe it — there is deliberately no constant for SUBMITTED/WORKING/
// INPUT_REQUIRED/AUTH_REQUIRED/REJECTED, because emitting one would be
// a claim this adapter cannot back up.
const (
	taskStateCompleted = "TASK_STATE_COMPLETED"
	taskStateFailed    = "TASK_STATE_FAILED"
)

// Role values, verbatim from the spec's Role enum.
const (
	roleUser  = "ROLE_USER"
	roleAgent = "ROLE_AGENT"
)

// Role is a Message's sender. It marshals as the canonical protobuf
// enum-value NAME (`"ROLE_USER"`), which is what the official client
// emits and what its `Role.fromJSON` reads back. Unmarshalling is
// deliberately more lenient than marshalling: protobuf JSON permits an
// enum to arrive as either its name or its integer tag, and a
// conforming client from another ecosystem may well send `1`. Decoding
// straight into a Go `string` would reject that whole request with a
// parse error over a field this adapter doesn't even branch on, so
// Role accepts both forms and normalises to the name.
type Role string

// UnmarshalJSON accepts either the enum name (`"ROLE_USER"`) or its
// integer tag (`1`), per protobuf JSON.
func (r *Role) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*r = Role(s)
		return nil
	}
	n, err := strconv.Atoi(string(b))
	if err != nil {
		return fmt.Errorf("a2a: role must be an enum name or integer tag, got %s", b)
	}
	switch n {
	case 1:
		*r = roleUser
	case 2:
		*r = roleAgent
	default:
		*r = "ROLE_UNSPECIFIED"
	}
	return nil
}

// Part is one section of a Message's or an Artifact's content — a
// protobuf `oneof`, so AT MOST ONE of Text/Raw/URL/Data is ever set,
// and the set one is identified purely by which field is present. There
// is no discriminator field to write or read; `{"text":"hi"}` IS a text
// part. Pointers/slices with omitempty are what keep that true on the
// way out: a zero-value Part must marshal to `{}`, never to
// `{"text":""}`, which would assert an empty text part.
//
// This adapter only ever PRODUCES text parts (the model's answer is
// prose) and only ever CONSUMES text parts (see textOf) — the other
// three are declared so an inbound file/data part round-trips into the
// task history intact rather than being silently dropped.
type Part struct {
	Text *string `json:"text,omitempty"`
	// Raw is a file's bytes; encoding/json renders []byte as base64,
	// which is exactly protobuf JSON's `bytes` encoding.
	Raw       []byte         `json:"raw,omitempty"`
	URL       *string        `json:"url,omitempty"`
	Data      any            `json:"data,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Filename  string         `json:"filename,omitempty"`
	MediaType string         `json:"mediaType,omitempty"`
}

// textPart builds the one Part shape this adapter emits.
func textPart(s string) Part { return Part{Text: &s} }

// Message is one unit of communication between client and server.
// MessageID is generated by whoever creates the message — this adapter
// generates one for every message IT authors, and echoes the client's
// own for the message the client sent.
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             Role           `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// text returns the message's text parts concatenated — this adapter's
// only reading of an inbound Message's content. Non-text parts are
// ignored rather than erroring: a client is free to attach a file part
// this adapter has no use for, and refusing the whole message over it
// would be less useful than answering the text it did send. A message
// with no text at all is caught by the caller (handleSendMessage), which
// is where an actionable INVALID_PARAMS can still be returned.
func (m Message) text() string {
	var b bytes.Buffer
	for _, p := range m.Parts {
		if p.Text != nil {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(*p.Text)
		}
	}
	return b.String()
}

// Artifact is a task output. Parts must contain at least one part.
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
}

// TaskStatus is a Task's current state plus an optional message
// explaining it — which is where this adapter puts a failed run's error
// text, since a Task has no error field of its own.
type TaskStatus struct {
	State     string   `json:"state"`
	Message   *Message `json:"message,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

// Task is the core unit of action in A2A: the thing SendMessage returns
// and GetTask reads back.
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// AgentCapabilities advertises optional protocol capabilities. Both of
// the ones this adapter could plausibly claim are false, and both are
// written out explicitly rather than omitted: an agent card is a
// contract with strangers, so "we do not stream" should be stated, not
// inferred from an absent key. Streaming would mean serving
// `SendStreamingMessage` over SSE, which this adapter does not (see
// jsonrpc.go's method table); push notifications would mean a callback
// sender it likewise has none of.
type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

// AgentInterface declares one (url, protocolBinding, protocolVersion)
// triple the agent can be called at. This is v1.0's replacement for
// v0.3's top-level `url`/`preferredTransport`/`protocolVersion`, and it
// is what the official client's transport selection actually reads:
// ClientFactory walks `supportedInterfaces`, keys them by
// protocolBinding, and instantiates the matching transport factory
// against that entry's `url`. A card without this array yields "No
// compatible transport found" no matter how correct the rest of it is.
type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

// AgentSkill is one advertised capability. Note what v1.0's AgentSkill
// does NOT have: no input/output JSON Schema (v0.3's `input_schema`/
// `output_schema` are gone) and no metadata map. Tags are the only
// free-form channel on a skill, which is why toolTags puts this
// adapter's Rule 13 allowlist disclosure there — see card.go.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples"`
	InputModes  []string `json:"inputModes"`
	OutputModes []string `json:"outputModes"`
}

// AgentCard is the self-description served at
// {path}/.well-known/agent-card.json.
type AgentCard struct {
	Name                string            `json:"name"`
	Description         string            `json:"description"`
	SupportedInterfaces []AgentInterface  `json:"supportedInterfaces"`
	Version             string            `json:"version"`
	Capabilities        AgentCapabilities `json:"capabilities"`
	DefaultInputModes   []string          `json:"defaultInputModes"`
	DefaultOutputModes  []string          `json:"defaultOutputModes"`
	Skills              []AgentSkill      `json:"skills"`
}

// sendMessageParams is the `params` of a SendMessage / GetTask-adjacent
// request — the JSON the official client's
// `SendMessageRequest.toJSON()` produces, which omits every empty field
// (verified by round-tripping it: a plain text send serialises to just
// `{"message":{"messageId":…,"role":"ROLE_USER","parts":[{"text":…}]}}`).
//
// Deliberately has NO identity field, exactly like the shape it
// replaces: identity always comes from request context (see
// WithIdentity/IdentityFrom in server.go), never from the payload. A
// caller that puts an "identity" key in here anyway — at any nesting
// level — is harmless, because encoding/json has nowhere to put it and
// nothing in this package ever looks. See
// TestSendMessageIgnoresForgedIdentityInParams.
type sendMessageParams struct {
	Message  Message        `json:"message"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Configuration is accepted and ignored: its fields
	// (acceptedOutputModes, historyLength, returnImmediately,
	// taskPushNotificationConfig) all describe behaviours this adapter
	// doesn't vary — it is always blocking, always returns the full
	// (two-message) history, always answers text/plain, and pushes
	// nothing. Declaring the field keeps a client that sends one from
	// looking like it was misunderstood.
	Configuration json.RawMessage `json:"configuration,omitempty"`
}

// taskIDParams is the `params` of GetTask and CancelTask: the spec
// names the task's identifier `id` in both (NOT `taskId` — that
// spelling belongs to the push-notification-config requests).
type taskIDParams struct {
	ID string `json:"id"`
}

// sendMessageResult is SendMessage's `result`. It is a protobuf oneof
// (`task` | `message`) and MUST be enveloped: the client runs the
// result through `SendMessageResponse.fromJSON` and throws "Invalid
// response: missing payload" on a bare Task. GetTask/CancelTask are the
// opposite — their results are a BARE Task, since the client parses
// those with `Task.fromJSON` directly. That asymmetry is real, is easy
// to get backwards, and is pinned by TestSendMessageResultIsEnveloped.
type sendMessageResult struct {
	Task *Task `json:"task,omitempty"`
}
