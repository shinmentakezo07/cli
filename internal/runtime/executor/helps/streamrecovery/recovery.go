package streamrecovery

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// CollectTextResult is the parsed output of a recovery continuation request.
type CollectTextResult struct {
	Text     string
	Thinking string
	Terminal bool
}

// CollectText reuses the caller's createStream callback to issue a recovery
// request and consume the resulting stream synchronously. It mirrors
// OpenAIChatRecovery.collect_text in cc/providers/transports/openai_chat/recovery.py.
//
// createStream returns a StreamReader over the upstream stream. Each upstream
// SSE data payload (without the "data: " prefix) is passed to cb as raw bytes;
// cb is responsible for parsing the OpenAI chat completion chunk. We accumulate
// content/reasoning deltas out-of-band without forwarding them downstream,
// since recovery output is post-processed by the caller before being emitted.
//
// The function is aware of the MidstreamRecoveryAttempts global; it will retry
// the inner stream on retryable errors up to that limit before surfacing the
// last error.
func CollectText(
	ctx context.Context,
	createStream func(ctx context.Context, body map[string]any) (StreamReader, error),
	body map[string]any,
	stallTimeout time.Duration,
) (CollectTextResult, error) {
	var lastErr error
	for attempt := 0; attempt < MidstreamRecoveryAttempts; attempt++ {
		stream, err := createStream(ctx, body)
		if err != nil {
			lastErr = err
			if !IsRetryableStreamError(err) {
				break
			}
			continue
		}
		var textParts, thinkingParts []string
		terminal := false
		err = stream.ForEach(ctx, stallTimeout, func(data []byte) {
			if len(data) == 0 {
				return
			}
			if bytesStartsWith(data, "[DONE]") {
				return
			}
			var chunk openAIChunk
			if err := json.Unmarshal(data, &chunk); err != nil {
				return
			}
			if len(chunk.Choices) == 0 {
				return
			}
			choice := chunk.Choices[0]
			if choice.FinishReason != "" {
				terminal = true
			}
			if choice.Delta.ReasoningContent != "" {
				thinkingParts = append(thinkingParts, choice.Delta.ReasoningContent)
			}
			if choice.Delta.Content != "" {
				textParts = append(textParts, choice.Delta.Content)
			}
		})
		if err != nil {
			_ = stream.Close()
			lastErr = err
			if !IsRetryableStreamError(err) {
				break
			}
			continue
		}
		_ = stream.Close()
		if !terminal {
			lastErr = ErrTruncatedProviderStream
			continue
		}
		return CollectTextResult{
			Text:     strings.Join(textParts, ""),
			Thinking: strings.Join(thinkingParts, ""),
			Terminal: true,
		}, nil
	}
	if lastErr != nil {
		return CollectTextResult{}, lastErr
	}
	return CollectTextResult{}, nil
}

// openAIChunk is the subset of an OpenAI chat completion chunk used by the
// recovery collector. Defined here so the package doesn't depend on the SDK
// translator types.
type openAIChunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
}

// StreamReader is the minimal stream interface used by the recovery collector.
// Implementations read upstream OpenAI chat completion chunks and call cb for
// each parsed delta.
//
// cb receives the SSE data payload (without the "data: " prefix and without a
// trailing newline) for each chunk. Implementations must NOT call cb for
// keep-alive comments, event/id/retry metadata, or the terminal "[DONE]"
// marker (those should be silently skipped). On a non-2xx stream that opened
// successfully, ForEach returns *HTTPStatusError. On a stall or truncation,
// ForEach returns ErrStalledProviderStream / ErrTruncatedProviderStream.
type StreamReader interface {
	ForEach(ctx context.Context, stall time.Duration, cb func(data []byte)) error
	Close() error
}

// RecoveryOutcome is the final result of a recovery attempt for one broken
// stream. Events is the list of SSE strings to forward downstream in order.
// When nil, recovery was not possible and the caller should surface the
// original error.
type RecoveryOutcome struct {
	Events []string
}

// BuildTextRecoveryEvents issues a text-continuation request, computes the
// suffix vs. the partial text/thinking already emitted, and returns the
// SSE events to emit (in downstream format supplied by eventEmit). When the
// continuation yields no new content it returns a nil outcome.
//
// eventEmit is the caller-supplied function that produces downstream SSE
// strings (so we don't depend on the translator package here). The caller
// passes helpers bound to its own emitter/ledger.
func BuildTextRecoveryEvents(
	ctx context.Context,
	createStream func(ctx context.Context, body map[string]any) (StreamReader, error),
	body map[string]any,
	partialText, partialThinking string,
	stallTimeout time.Duration,
	eventEmit TextRecoveryEmitter,
) (*RecoveryOutcome, error) {
	recoveryBody := MakeTextRecoveryBody(body, partialText, partialThinking)
	collected, err := CollectText(ctx, createStream, recoveryBody, stallTimeout)
	if err != nil {
		return nil, err
	}
	textSuffix := ContinuationSuffix(partialText, collected.Text)
	thinkingSuffix := ContinuationSuffix(partialThinking, collected.Thinking)
	var events []string
	if thinkingSuffix != "" {
		events = append(events, eventEmit.EnsureThinkingBlock()...)
		events = append(events, eventEmit.EmitThinkingDelta(thinkingSuffix))
	}
	if textSuffix != "" {
		events = append(events, eventEmit.EnsureTextBlock()...)
		events = append(events, eventEmit.EmitTextDelta(textSuffix))
	}
	if len(events) == 0 {
		return nil, nil
	}
	events = append(events, eventEmit.CloseAllBlocks()...)
	events = append(events, eventEmit.MessageDelta("end_turn", 1))
	events = append(events, eventEmit.MessageStop())
	return &RecoveryOutcome{Events: events}, nil
}

// TextRecoveryEmitter is the abstract Anthropic SSE emitter callbacks the
// recovery layer uses to construct continuation events. The caller binds these
// to the actual translator-backed emitter.
type TextRecoveryEmitter interface {
	EnsureThinkingBlock() []string
	EnsureTextBlock() []string
	EmitThinkingDelta(s string) string
	EmitTextDelta(s string) string
	CloseAllBlocks() []string
	MessageDelta(stopReason string, outputTokens int) string
	MessageStop() string
}

// BuildToolRepairEvents repairs in-flight tool calls that were started but not
// completed before the stream broke. For each started tool whose prefix is not
// yet a parseable/valid complete input, we issue a tool-repair continuation and
// accept the suffix; if any tool fails to repair we return nil (no salvage).
//
// eventEmit supplies tool-delta and tool-block-close callbacks bound to the
// caller's ledger, in downstream format.
func BuildToolRepairEvents(
	ctx context.Context,
	createStream func(ctx context.Context, body map[string]any) (StreamReader, error),
	body map[string]any,
	toolBlocks []ToolState,
	schemas map[string]ToolSchema,
	stallTimeout time.Duration,
	eventEmit ToolRepairEmitter,
) (*RecoveryOutcome, error) {
	var events []string
	for _, tb := range toolBlocks {
		rawArgs := strings.Join(tb.ArgParts, "")
		if ParseCompleteToolInput(rawArgs, tb.Name, schemas) != nil {
			// Already complete; emit any buffered args if not previously emitted.
			if eventEmit.ShouldEmitExisting(tb.Index) {
				events = append(events, eventEmit.EmitToolDelta(tb.Index, rawArgs))
			}
			continue
		}
		schema := schemas[tb.Name]
		repairBody := MakeToolRepairBody(body, tb.Name, rawArgs, schema.InputSchema)
		collected, err := CollectText(ctx, createStream, repairBody, stallTimeout)
		if err != nil {
			return nil, err
		}
		suffix, _, ok := AcceptToolJSONRepair(rawArgs, collected.Text, tb.Name, schemas)
		if !ok {
			return nil, nil
		}
		toEmit := suffix
		if eventEmit.ShouldEmitExisting(tb.Index) {
			toEmit = rawArgs + suffix
		}
		if toEmit != "" {
			events = append(events, eventEmit.EmitToolDelta(tb.Index, toEmit))
		}
	}
	events = append(events, eventEmit.CloseAllBlocks()...)
	events = append(events, eventEmit.MessageDelta("tool_use", 1))
	events = append(events, eventEmit.MessageStop())
	return &RecoveryOutcome{Events: events}, nil
}

// ToolRepairEmitter is the abstract emitter for tool-repair SSE events.
type ToolRepairEmitter interface {
	// ShouldEmitExisting reports whether the caller's ledger has *not* yet
	// emitted the existing prefix for this tool. When true, the recovered
	// suffix must include the existing prefix so the downstream consumer
	// reconstructs the full JSON.
	ShouldEmitExisting(toolIndex int) bool
	EmitToolDelta(toolIndex int, s string) string
	CloseAllBlocks() []string
	MessageDelta(stopReason string, outputTokens int) string
	MessageStop() string
}

// NoopEmitter is a TextRecoveryEmitter / ToolRepairEmitter that returns empty
// output. Useful as a placeholder when the caller doesn't wire an emitter.
type NoopEmitter struct{}

func (NoopEmitter) EnsureThinkingBlock() []string    { return nil }
func (NoopEmitter) EnsureTextBlock() []string        { return nil }
func (NoopEmitter) EmitThinkingDelta(string) string  { return "" }
func (NoopEmitter) EmitTextDelta(string) string      { return "" }
func (NoopEmitter) CloseAllBlocks() []string         { return nil }
func (NoopEmitter) MessageDelta(string, int) string  { return "" }
func (NoopEmitter) MessageStop() string              { return "" }
func (NoopEmitter) ShouldEmitExisting(int) bool      { return true }
func (NoopEmitter) EmitToolDelta(int, string) string { return "" }

// bytesStartsWith is a small helper to avoid importing bytes here.
func bytesStartsWith(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}
