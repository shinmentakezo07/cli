package streamrecovery

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// StateTracker mirrors the subset of Python's AnthropicStreamLedger state used
// by stream recovery: accumulated text, accumulated thinking, and per-tool
// call argument buffers with their names and started-state.
//
// Unlike the Python ledger, the Go executor does not own the downstream SSE
// emission (sdktranslator.TranslateStream owns that). This tracker inspects
// upstream OpenAI-format chat completion chunks *before* translation so that
// the recovery layer can detect when the upstream produced partial output that
// would be lost if the stream restarted from scratch.
type StateTracker struct {
	mu               sync.Mutex
	textParts        []string
	thinkingParts    []string
	toolStateByIndex map[int]*ToolState
	toolOrder        []int
	sawFinishReason  bool
}

// ToolState tracks one in-flight tool call from the upstream stream.
type ToolState struct {
	Index    int
	ID       string
	Name     string
	ArgParts []string
	Started  bool
	Complete bool
}

// NewStateTracker constructs an empty tracker.
func NewStateTracker() *StateTracker {
	return &StateTracker{toolStateByIndex: make(map[int]*ToolState)}
}

// IngestOpenAIChunk parses one raw OpenAI chat completion SSE line (the data
// portion, without the "data: " prefix) and updates the tracking state.
// Returns true when the chunk is a chat completion chunk with choices, false
// when it should be ignored (usage-only, ping, done marker, or unparseable).
func (s *StateTracker) IngestOpenAIChunk(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var chunk struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(trimmed, &chunk); err != nil {
		return false
	}
	if len(chunk.Choices) == 0 {
		// Usage-only chunk or other non-choice payload.
		return true
	}
	choice := chunk.Choices[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	if choice.FinishReason != "" {
		s.sawFinishReason = true
	}
	if choice.Delta.ReasoningContent != "" {
		s.thinkingParts = append(s.thinkingParts, choice.Delta.ReasoningContent)
	}
	if choice.Delta.Content != "" {
		s.textParts = append(s.textParts, choice.Delta.Content)
	}
	for _, tc := range choice.Delta.ToolCalls {
		state, ok := s.toolStateByIndex[tc.Index]
		if !ok {
			state = &ToolState{Index: tc.Index}
			s.toolStateByIndex[tc.Index] = state
			s.toolOrder = append(s.toolOrder, tc.Index)
		}
		if tc.ID != "" && state.ID == "" {
			state.ID = tc.ID
		}
		if tc.Function.Name != "" && state.Name == "" {
			state.Name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			state.ArgParts = append(state.ArgParts, tc.Function.Arguments)
		}
		state.Started = true
	}
	return true
}

// SawFinishReason reports whether the upstream stream emitted a terminal
// finish_reason. Used by the stream loop to detect truncation (stream ended
// without finish_reason).
func (s *StateTracker) SawFinishReason() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawFinishReason
}

// AccumulatedText returns the concatenated text deltas received so far.
func (s *StateTracker) AccumulatedText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.textParts, "")
}

// AccumulatedReasoning returns the concatenated thinking / reasoning_content
// deltas received so far.
func (s *StateTracker) AccumulatedReasoning() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.thinkingParts, "")
}

// HasEmittedTool reports whether at least one tool call has been started.
func (s *StateTracker) HasEmittedTool() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.toolOrder) > 0
}

// ToolBlocks returns a snapshot of in-flight tool blocks in upstream order.
func (s *StateTracker) ToolBlocks() []ToolState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ToolState, 0, len(s.toolOrder))
	for _, idx := range s.toolOrder {
		if state, ok := s.toolStateByIndex[idx]; ok && state.Started {
			out = append(out, *state)
		}
	}
	return out
}

// ToolBlockContent returns the concatenated argument fragments for the tool at
// the given upstream tool index, or "" if not present.
func (s *StateTracker) ToolBlockContent(toolIndex int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.toolStateByIndex[toolIndex]
	if !ok {
		return ""
	}
	return strings.Join(state.ArgParts, "")
}

// AllToolsComplete reports whether every started tool call has accumulated a
// parseable JSON object that validates against the supplied schemas. Mirrors
// all_emitted_tools_complete in cc/providers/transports/openai_chat/tool_calls.py.
func (s *StateTracker) AllToolsComplete(schemas map[string]ToolSchema) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.toolOrder) == 0 {
		return false
	}
	for _, idx := range s.toolOrder {
		state, ok := s.toolStateByIndex[idx]
		if !ok || !state.Started {
			return false
		}
		rawArgs := strings.Join(state.ArgParts, "")
		if strings.TrimSpace(rawArgs) == "" {
			return false
		}
		if ParseCompleteToolInput(rawArgs, state.Name, schemas) == nil {
			return false
		}
	}
	return true
}

// MarkToolComplete marks the tool at the given upstream tool index as complete
// (used after a successful tool-repair emits the suffix).
func (s *StateTracker) MarkToolComplete(toolIndex int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.toolStateByIndex[toolIndex]; ok {
		state.Complete = true
	}
}

// Reset clears all accumulated state, used by early transparent retry before
// restarting the stream.
func (s *StateTracker) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.textParts = nil
	s.thinkingParts = nil
	s.toolStateByIndex = make(map[int]*ToolState)
	s.toolOrder = nil
	s.sawFinishReason = false
}

// GeneratedOutput reports whether any non-empty text, thinking, or tool delta
// has been accumulated. Mirrors has_committed_sse_output / generated_output
// hints in recovery.py.
func (s *StateTracker) GeneratedOutput() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.textParts) > 0 || len(s.thinkingParts) > 0 || len(s.toolOrder) > 0
}

// FlushToolArgBuffers returns (toolIndex, JSON-string) pairs for any started
// tool whose accumulated arguments parse as a valid JSON object - normalized
// and trimmed of any partial trailing garbage if parsing fails. Mirrors
// flush_task_arg_buffers in ledger.py (simplified: we only attempt the strict
// parse here since downstream tool JSON delta emission is owned by the
// translator, not by this tracker).
func (s *StateTracker) FlushToolArgBuffers() []ToolArgFlush {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ToolArgFlush, 0, len(s.toolOrder))
	for _, idx := range s.toolOrder {
		state, ok := s.toolStateByIndex[idx]
		if !ok || state == nil || len(state.ArgParts) == 0 {
			continue
		}
		raw := strings.Join(state.ArgParts, "")
		flush, _ := normalizeOrEmpty(raw)
		out = append(out, ToolArgFlush{ToolIndex: idx, ArgsJSON: flush})
		state.Complete = true
	}
	return out
}

// ToolArgFlush is one entry from FlushToolArgBuffers.
type ToolArgFlush struct {
	ToolIndex int
	ArgsJSON  string
}

func normalizeOrEmpty(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", false
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "{}", false
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return "{}", false
	}
	return string(out), true
}
