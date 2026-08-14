// Package anthropic implements ai.StructuredGenerator against the real
// Claude Messages API using forced tool use: every request declares a
// single tool, "emit_result", whose input_schema is the caller's
// req.Schema verbatim, and forces tool_choice to that tool. Claude's
// "arguments" for calling emit_result therefore ARE the structured,
// schema-conforming JSON the caller asked for — tool-call argument
// generation is far more reliable at honoring a schema than asking
// the model to freeform a JSON object in prose.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const (
	provider = "anthropic"

	// toolName is the single tool every request forces. Claude has no
	// choice but to call it, so its "input" (validated against
	// req.Schema) is the structured response we want.
	toolName = "emit_result"

	// defaultMaxTokens applies when the caller leaves
	// StructuredRequest.MaxTokens at its zero value.
	defaultMaxTokens = 2048
)

type generator struct {
	client anthropic.Client
	model  string
}

// New returns an ai.StructuredGenerator backed by the real Anthropic
// Messages API. cfg.BaseURL and cfg.APIKey are always passed
// explicitly to the SDK client, so this generator never falls back to
// ambient ANTHROPIC_* environment variables — the caller's config is
// the only source of truth for where requests go and what
// credentials they carry.
func New(cfg config.Anthropic) ai.StructuredGenerator {
	return &generator{
		client: anthropic.NewClient(
			option.WithAPIKey(cfg.APIKey),
			option.WithBaseURL(cfg.BaseURL),
		),
		model: cfg.Model,
	}
}

func (g *generator) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	// The tool's input_schema is req.Schema byte-for-byte: param.Override
	// replaces the field's normal typed marshaling with these raw bytes,
	// so what Anthropic sees under tools[0].input_schema is exactly the
	// schema the caller asked for — additionalProperties, enums, and all.
	inputSchema := param.Override[anthropic.ToolInputSchemaParam](json.RawMessage(req.Schema))
	tool := anthropic.ToolUnionParamOfTool(inputSchema, toolName)

	resp, err := g.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      anthropic.Model(g.model),
		MaxTokens:  int64(maxTokens),
		System:     []anthropic.TextBlockParam{{Text: req.System}},
		Messages:   []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(req.User))},
		Tools:      []anthropic.ToolUnionParam{tool},
		ToolChoice: anthropic.ToolChoiceParamOfTool(toolName),
	})
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: g.model},
			fmt.Errorf("anthropic: messages.new: %w", err)
	}

	for _, block := range resp.Content {
		if block.Type == "tool_use" && block.Name == toolName {
			return ai.StructuredResponse{
				JSON:         json.RawMessage(block.Input),
				Provider:     provider,
				Model:        string(resp.Model),
				InputTokens:  int(resp.Usage.InputTokens),
				OutputTokens: int(resp.Usage.OutputTokens),
				Latency:      time.Since(start),
			}, nil
		}
	}

	return ai.StructuredResponse{Provider: provider, Model: string(resp.Model)},
		fmt.Errorf("anthropic: response contained no tool_use block for tool %q (stop_reason %q)", toolName, resp.StopReason)
}
