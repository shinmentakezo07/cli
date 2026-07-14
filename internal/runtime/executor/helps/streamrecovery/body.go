package streamrecovery

import (
	"encoding/json"
	"strings"
)

const (
	recoveryUserPrefix = "The previous provider stream was interrupted. Continue the assistant response " +
		"exactly where it stopped. Do not repeat text already written."
	recoveryThinkingPrefix = "The assistant had already emitted this hidden thinking before the interruption:\n"
)

// MakeTextRecoveryBody builds a text-only continuation request for either
// transport family. Mirrors make_text_recovery_body in recovery.py.
//
// recovery is a deep copy of body with tools/tool_choice stripped, stream
// forced true, and messages extended with the partial assistant content plus
// a user prompt asking the model to continue.
func MakeTextRecoveryBody(body map[string]any, partialText, partialThinking string) map[string]any {
	recovery := deepCopyMapAny(body)
	delete(recovery, "tools")
	delete(recovery, "tool_choice")
	recovery["stream"] = true
	messages := copiedMessages(recovery)
	if partialText != "" {
		messages = append(messages, map[string]any{"role": "assistant", "content": partialText})
	}
	prompt := recoveryUserPrefix
	if partialThinking != "" {
		prompt = recoveryThinkingPrefix + partialThinking + "\n\n" + prompt
	}
	messages = append(messages, map[string]any{"role": "user", "content": prompt})
	recovery["messages"] = messages
	return recovery
}

// MakeToolRepairBody builds a text-only request asking upstream to complete a
// JSON suffix for an interrupted tool call. Mirrors make_tool_repair_body in
// recovery.py.
func MakeToolRepairBody(body map[string]any, toolName, prefix string, inputSchema map[string]any) map[string]any {
	recovery := deepCopyMapAny(body)
	delete(recovery, "tools")
	delete(recovery, "tool_choice")
	recovery["stream"] = true
	messages := copiedMessages(recovery)
	messages = append(messages, map[string]any{
		"role":    "user",
		"content": toolRepairPrompt(toolName, prefix, inputSchema),
	})
	recovery["messages"] = messages
	return recovery
}

func copiedMessages(body map[string]any) []any {
	messages, ok := body["messages"].([]any)
	if !ok {
		return []any{}
	}
	return deepCopyValueAny(messages).([]any)
}

func toolRepairPrompt(toolName, prefix string, inputSchema map[string]any) string {
	if inputSchema == nil {
		inputSchema = map[string]any{"type": "object"}
	}
	schemaText, _ := json.Marshal(inputSchema)
	return "A streamed tool call was interrupted while writing JSON arguments.\n" +
		"Tool name: " + toolName + "\n" +
		"JSON schema: " + string(schemaText) + "\n" +
		"Already emitted JSON prefix: " + prefix + "\n\n" +
		"Return only the exact missing JSON suffix needed to complete the same object. " +
		"Do not repeat the prefix. Do not include markdown or explanation."
}

// stripMarkdownFence removes a leading and trailing ``` fence from a block of
// text, used by toolRepairCandidate cleanup.
func stripMarkdownFence(raw string) string {
	if !strings.HasPrefix(raw, "```") {
		return raw
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "```") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
