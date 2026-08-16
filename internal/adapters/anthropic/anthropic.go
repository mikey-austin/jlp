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
	// resolver, when non-nil, is consulted on every call (see
	// resolveModel below) so a /settings override reaches the very
	// next request without reconstructing this generator (Phase 4 Task
	// S). nil means "no resolver wired up" — model is always cfg.Model.
	resolver ai.ModelResolver
}

// New returns a value implementing BOTH ai.StructuredGenerator (forced
// tool use, single turn) and ai.ToolCaller (real multi-turn tool
// calling) against the real Anthropic Messages API — the concrete
// *generator return type (rather than either interface alone) is what
// lets a single call site hand the same value to both a
// StructuredGenerator chain and a ToolCaller chain (see
// cmd/jlp/ai.go's buildAIGenerator/buildToolCaller). cfg.BaseURL and
// cfg.APIKey are always passed explicitly to the SDK client, so this
// generator never falls back to ambient ANTHROPIC_* environment
// variables — the caller's config is the only source of truth for
// where requests go and what credentials they carry. resolver (may be
// nil) is checked at CALL time (resolveModel), not just here at
// construction — see that method's doc comment.
func New(cfg config.Anthropic, resolver ai.ModelResolver) *generator {
	return &generator{
		client: anthropic.NewClient(
			option.WithAPIKey(cfg.APIKey),
			option.WithBaseURL(cfg.BaseURL),
		),
		model:    cfg.Model,
		resolver: resolver,
	}
}

// resolveModel returns the model this call should use: resolver's
// current override for "anthropic" when one is set, else g.model (the
// APP_AI_ANTHROPIC_MODEL config value g was constructed with). Called
// fresh on every GenerateStructured/CallWithTools invocation — never
// cached — so a settings change takes effect on the very next call.
func (g *generator) resolveModel() string {
	if g.resolver != nil {
		if m, _ := g.resolver.Model(provider); m != "" {
			return m
		}
	}
	return g.model
}

func (g *generator) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()
	reqModel := g.resolveModel()

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
		Model:      anthropic.Model(reqModel),
		MaxTokens:  int64(maxTokens),
		System:     []anthropic.TextBlockParam{{Text: req.System}},
		Messages:   []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(req.User))},
		Tools:      []anthropic.ToolUnionParam{tool},
		ToolChoice: anthropic.ToolChoiceParamOfTool(toolName),
	})
	if err != nil {
		return ai.StructuredResponse{Provider: provider, Model: reqModel},
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

// CallWithTools implements ai.ToolCaller against the same Messages API
// GenerateStructured uses, but WITHOUT forcing tool_choice: req.Tools
// are offered as real tools (Anthropic's tools + tool_use/tool_result
// blocks), and the model decides, turn by turn, whether to call one,
// several, or none — the multi-turn agentic shape ToolCaller exists
// for, as opposed to StructuredGenerator's single forced call.
func (g *generator) CallWithTools(ctx context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	start := time.Now()
	reqModel := g.resolveModel()

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	resp, err := g.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(reqModel),
		MaxTokens: int64(maxTokens),
		System:    []anthropic.TextBlockParam{{Text: req.System}},
		Messages:  toAnthropicMessages(req.Messages),
		Tools:     toAnthropicTools(req.Tools),
	})
	if err != nil {
		return ai.ToolResponse{Provider: provider, Model: reqModel},
			fmt.Errorf("anthropic: messages.new: %w", err)
	}

	turn := ai.ToolTurn{}
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			turn.Text += block.Text
		case "tool_use":
			turn.Invocations = append(turn.Invocations, ai.ToolInvocation{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: json.RawMessage(block.Input),
			})
		}
	}

	return ai.ToolResponse{
		Turn:         turn,
		Provider:     provider,
		Model:        string(resp.Model),
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
		Latency:      time.Since(start),
	}, nil
}

// toAnthropicTools translates every ai.ToolDef into an Anthropic
// custom tool, its InputSchema carried byte-for-byte via
// param.Override — the same raw-bytes pass-through
// GenerateStructured's forced emit_result tool uses for req.Schema.
func toAnthropicTools(defs []ai.ToolDef) []anthropic.ToolUnionParam {
	tools := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        d.Name,
			Description: anthropic.String(d.Description),
			InputSchema: param.Override[anthropic.ToolInputSchemaParam](json.RawMessage(d.Schema)),
		}})
	}
	return tools
}

// toAnthropicMessages replays a ToolRequest's conversation so far into
// the Messages API's own turn shape: a "user" ToolMessage becomes a
// plain text user turn; an "assistant" ToolMessage becomes an
// assistant turn carrying its Text (if any) plus one tool_use block
// per Invocation; a "tool" ToolMessage becomes a user turn carrying
// one tool_result block per Result — Anthropic's wire contract puts
// tool results in a user-role message, never their own role.
func toAnthropicMessages(msgs []ai.ToolMessage) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(m.Invocations))
			if m.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Text))
			}
			for _, inv := range m.Invocations {
				blocks = append(blocks, anthropic.NewToolUseBlock(inv.ID, json.RawMessage(inv.Arguments), inv.Name))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		case "tool":
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Results))
			for _, r := range m.Results {
				blocks = append(blocks, anthropic.NewToolResultBlock(r.ID, r.Content, r.IsError))
			}
			out = append(out, anthropic.NewUserMessage(blocks...))
		default: // "user"
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
		}
	}
	return out
}
