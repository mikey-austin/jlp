package tools

import (
	"encoding/json"
	"fmt"
)

// decodeArgs unmarshals a tool call's raw JSON arguments into T. Empty
// arguments (a model calling a tool that takes no required fields
// without bothering to send "{}") decode to T's zero value rather than
// erroring — every args struct in this package is designed so its zero
// value is a sensible default. A malformed-JSON args string is the one
// case this DOES report as an error, becoming an ai.ToolResult with
// IsError true via Registry.Invoke, so the model sees a clear message
// and can retry with valid JSON instead of the handler panicking on a
// zero-value/garbage mix.
func decodeArgs[T any](args json.RawMessage) (T, error) {
	var v T
	if len(args) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(args, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

// clampLimit returns want when it's in (0, max], falls back to def
// when want is <= 0 (the model omitted it or asked for a nonsensical
// value), and caps at max otherwise — every list-returning tool in
// this package uses this so a model can never ask a handler to dump
// an unbounded (context-wasting, per this task's design guidance)
// result set.
func clampLimit(want, def, max int) int {
	switch {
	case want <= 0:
		return def
	case want > max:
		return max
	default:
		return want
	}
}
