// Package aiutil holds the schema-validation-with-bounded-repair logic
// every generation agent (teacher, drill) needs: Rule 4 says an
// ai.StructuredGenerator response gets one constrained repair attempt
// (extract the first balanced {...} block, e.g. to strip a markdown
// code fence) and, failing that, one full retry call before the caller
// must treat it as a hard failure. This package factors that loop out
// of internal/agent/teacher (where it first shipped) so
// internal/agent/drill can reuse it verbatim rather than
// re-implementing — or silently drifting from — the same
// bounded-self-healing contract.
//
// Rule 3 still applies here: this package imports ports/ai + schemas
// ONLY — no domain, no storage, no adapters. It is pure request/response
// plumbing shared by every agent, not itself an agent.
package aiutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

// ValidateWithRepairAndRetry implements Rule 4: schema validation with
// bounded self-healing. It tries, in order: resp as given; one
// constrained repair (extractBalancedObject re-validated against
// schemaName); one full retry call to gen using req. If none validates,
// it returns a wrapped error rather than looping further — an AI
// response that still won't validate after both is a hard failure, not
// something to keep retrying indefinitely.
func ValidateWithRepairAndRetry(ctx context.Context, gen ai.StructuredGenerator, schemaName string, req ai.StructuredRequest, resp ai.StructuredResponse) (ai.StructuredResponse, error) {
	if err := schemas.Validate(schemaName, resp.JSON); err == nil {
		return resp, nil
	}

	if repaired, ok := extractBalancedObject(resp.JSON); ok {
		if err := schemas.Validate(schemaName, repaired); err == nil {
			resp.JSON = repaired
			return resp, nil
		}
	}

	retried, err := gen.GenerateStructured(ctx, req)
	if err != nil {
		return resp, fmt.Errorf("aiutil: retry after invalid response: %w", err)
	}
	if err := schemas.Validate(schemaName, retried.JSON); err != nil {
		return retried, fmt.Errorf("aiutil: response failed schema validation after repair and retry: %w", err)
	}
	return retried, nil
}

// extractBalancedObject finds the first '{' in raw and returns the
// substring through its matching '}', tracking brace depth and JSON
// string literals (so braces inside quoted strings don't throw off the
// count). It reports ok=false if raw contains no balanced {...} block —
// e.g. a refusal message with no JSON in it at all, which repair can't
// fix and the caller must fall back to a retry for.
func extractBalancedObject(raw []byte) (json.RawMessage, bool) {
	start := bytes.IndexByte(raw, '{')
	if start == -1 {
		return nil, false
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return json.RawMessage(raw[start : i+1]), true
			}
		}
	}
	return nil, false
}
