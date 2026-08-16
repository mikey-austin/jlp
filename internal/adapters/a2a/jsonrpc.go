package a2a

import (
	"encoding/json"
	"errors"
	"net/http"
)

// JSON-RPC 2.0 error codes reserved for A2A, transcribed from the
// official client's own `A2A_ERROR_CODE` table (@a2a-js/sdk@1.0.1,
// dist/errors/index.d.ts) — the values it decodes an error response
// against. The first five are plain JSON-RPC 2.0; the -320xx range is
// A2A's own. Only the ones this adapter can actually emit are named.
const (
	errParse          = -32700 // malformed JSON
	errInvalidRequest = -32600 // well-formed JSON, but not a valid request object
	errMethodNotFound = -32601 // unknown method
	errInvalidParams  = -32602 // method known, params unusable

	errTaskNotFound      = -32001 // A2A: no such task (see handleGetTask on why "not yours" is this too)
	errTaskNotCancelable = -32002 // A2A: task is in a terminal state
)

// Note what is deliberately absent: -32603 INTERNAL_ERROR. A run that
// fails is not an RPC fault — the call succeeded, and its answer is a
// Task in TASK_STATE_FAILED carrying the reason in status.message (see
// handleSendMessage). Reporting it as a JSON-RPC error instead would
// throw away the task id, and with it the /ai/agents trace row that is
// the whole point of having failed runs recorded.

// maxRequestBodyBytes caps the JSON-RPC request body — the same 1 MiB
// cap internal/adapters/http applies to every JSON body it reads (see
// that package's api.go maxRequestBodyBytes doc comment, and
// documents.go/words.go, which apply it the same way this handler
// does). A message's text is forwarded verbatim into an LLM call, so an
// unbounded body is both a memory problem and a token-spend problem —
// same reasoning as every sibling handler, same number, not a new one
// invented for this package.
const maxRequestBodyBytes = 1 << 20

// rpcRequest is a JSON-RPC 2.0 request object. ID is kept as raw JSON
// and echoed back BYTE FOR BYTE: the spec requires the response id to
// equal the request id, and the official client enforces that with a
// strict `!==` against the number it sent, so re-encoding a numeric id
// through any Go numeric type would be a needless way to break the
// round trip.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      json.RawMessage `json:"id"`
}

// rpcResponse is a JSON-RPC 2.0 response object. Exactly one of
// Result/Error is ever set (both are omitempty and the two write paths
// below each set only one).
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func rpcErrf(code int, msg string) *rpcError { return &rpcError{Code: code, Message: msg} }

// nullID is the id a response carries when the request's own id could
// not be determined (a parse error, principally) — JSON-RPC 2.0
// requires null in exactly that case.
var nullID = json.RawMessage("null")

// writeRPCResult / writeRPCError are the only two ways this package
// answers a JSON-RPC call.
//
// Both respond with HTTP 200 even when carrying a JSON-RPC error. That
// is the JSON-RPC-over-HTTP convention (the error belongs to the
// envelope, not the transport) and it is what the official client is
// happiest with — it reads `error` off a 200 body directly, whereas a
// non-2xx status sends it down a fallback path that only recovers the
// structured error if the body happens to parse. The ONE deliberate
// exception is the missing-identity case in handleRPC, which is a
// transport-level authentication failure and answers 401.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, e *rpcError) {
	if id == nil {
		id = nullID
	}
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: e})
}

// handleRPC is the single POST endpoint the agent card advertises as
// this adapter's JSONRPC interface. It decodes the envelope, resolves
// the request's identity ONCE (from context — never from the payload;
// see the package doc comment), and dispatches on method.
//
// The method names are v1.0's, which are NOT v0.3's: v1.0 uses the
// PascalCase RPC names from the protobuf service definition
// (`SendMessage`, `GetTask`, `CancelTask`), where v0.3 used
// slash-separated ones (`message/send`, `tasks/get`, `tasks/cancel`).
// Both the spec's §9.4 method table and the official client's own
// JsonRpcTransport agree on the former; the latter appear in that same
// client only under its LegacyJsonRpcTransport, behind an opt-in
// `legacyCompat` flag that is off by default. Serving the v0.3 names
// would therefore be unreachable for a default-configured client.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeRPCError(w, http.StatusOK, nullID, rpcErrf(errInvalidRequest, "request body too large"))
			return
		}
		writeRPCError(w, http.StatusOK, nullID, rpcErrf(errParse, "malformed JSON request body"))
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPCError(w, http.StatusOK, req.ID, rpcErrf(errInvalidRequest, `"jsonrpc" must be "2.0"`))
		return
	}
	if req.Method == "" {
		writeRPCError(w, http.StatusOK, req.ID, rpcErrf(errInvalidRequest, `"method" is required`))
		return
	}

	// See the package doc comment's Identity section and
	// WithIdentity/IdentityFrom in server.go: the ONLY source of
	// identity here is request context, set by whoever mounted Routes()
	// inside their own authenticated group — never req, which has no
	// field to have read one from in the first place. 401 rather than a
	// 200-with-error because this is an authentication failure of the
	// transport, not a fault in an otherwise-accepted call.
	identity, ok := IdentityFrom(r.Context())
	if !ok || identity == "" {
		writeRPCError(w, http.StatusUnauthorized, req.ID, rpcErrf(errInvalidRequest, "no authenticated identity"))
		return
	}

	var (
		result any
		rpcErr *rpcError
	)
	switch req.Method {
	case "SendMessage":
		result, rpcErr = s.handleSendMessage(r.Context(), identity, req.Params)
	case "GetTask":
		result, rpcErr = s.handleGetTask(identity, req.Params)
	case "CancelTask":
		result, rpcErr = s.handleCancelTask(identity, req.Params)
	default:
		// Everything else in the v1.0 method table —
		// SendStreamingMessage, SubscribeToTask, ListTasks,
		// GetExtendedAgentCard, and the four push-notification-config
		// methods — is genuinely not served, and the agent card says so
		// (capabilities.streaming/pushNotifications are both false).
		// -32601 is the honest answer, not a stub.
		rpcErr = rpcErrf(errMethodNotFound, "unknown method "+req.Method)
	}
	if rpcErr != nil {
		writeRPCError(w, http.StatusOK, req.ID, rpcErr)
		return
	}
	writeRPCResult(w, req.ID, result)
}
