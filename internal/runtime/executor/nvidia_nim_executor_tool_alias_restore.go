// This file implements the response-side tool argument alias restoration for
// the NVIDIA NIM executor. When sanitizeNimToolSchemas renames unsafe tool
// parameter names (e.g. "type" -> "_fcc_arg_type") on the request side, the
// model generates arguments with the alias key. This module swaps the alias
// keys back to their original names in the translated downstream (Anthropic)
// response, so coding agents receive the correct tool call arguments.
//
// The implementation is post-translation: it inspects the Anthropic SSE or
// JSON payload *after* the SDK translator has converted OpenAI responses to
// Anthropic format, so it does not require any translator API changes.
package executor

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// nimToolArgumentAliasesFromBody extracts the alias map from a built request
// body. Returns nil when no aliases were recorded.
func nimToolArgumentAliasesFromBody(body map[string]any) map[string]map[string]string {
	raw, ok := body[nimToolArgumentAliasesKey]
	if !ok {
		return nil
	}
	rawMap, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]map[string]string, len(rawMap))
	for toolName, toolAliases := range rawMap {
		aliasesMap, ok := toolAliases.(map[string]any)
		if !ok {
			continue
		}
		aliases := make(map[string]string, len(aliasesMap))
		for alias, original := range aliasesMap {
			origStr, ok := original.(string)
			if !ok || origStr == "" {
				continue
			}
			aliases[alias] = origStr
		}
		if len(aliases) > 0 {
			result[toolName] = aliases
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// restoreNimToolAliasesNonStream restores aliased tool argument keys in a
// non-streaming Anthropic JSON response. The input is the full Anthropic
// message JSON (with a "content" array containing "tool_use" blocks).
func restoreNimToolAliasesNonStream(payload []byte, aliases map[string]map[string]string) []byte {
	if len(aliases) == 0 || len(payload) == 0 {
		return payload
	}
	contentRoot := gjson.GetBytes(payload, "content")
	if !contentRoot.Exists() || !contentRoot.IsArray() {
		return payload
	}
	changed := false
	var newContent []any
	contentRoot.ForEach(func(_, block gjson.Result) bool {
		var blockVal any
		if err := json.Unmarshal([]byte(block.Raw), &blockVal); err != nil {
			newContent = append(newContent, json.RawMessage(block.Raw))
			return true
		}
		blockMap, ok := blockVal.(map[string]any)
		if !ok {
			newContent = append(newContent, blockVal)
			return true
		}
		if blockMap["type"] != "tool_use" {
			newContent = append(newContent, blockVal)
			return true
		}
		toolName, _ := blockMap["name"].(string)
		toolAliases, ok := aliases[toolName]
		if !ok {
			newContent = append(newContent, blockVal)
			return true
		}
		input, ok := blockMap["input"].(map[string]any)
		if !ok {
			newContent = append(newContent, blockVal)
			return true
		}
		blockMap["input"] = restoreAliasedToolArguments(input, toolAliases)
		newContent = append(newContent, blockMap)
		changed = true
		return true
	})
	if !changed {
		return payload
	}
	contentBytes, err := json.Marshal(newContent)
	if err != nil {
		return payload
	}
	out, err := sjson.SetRawBytes(payload, "content", contentBytes)
	if err != nil {
		return payload
	}
	return out
}

// nimAliasRestorer is a stateful streaming alias restorer. It tracks which
// tool name corresponds to each Anthropic content block index (learned from
// content_block_start events) and when an input_json_delta arrives for a
// block index that has an alias map, it restores the alias keys in the
// partial_json before the SSE event is forwarded downstream.
type nimAliasRestorer struct {
	aliases   map[string]map[string]string
	blockTool map[int]string // content_block index -> tool_name
}

func newNimAliasRestorer(aliases map[string]map[string]string) *nimAliasRestorer {
	if len(aliases) == 0 {
		return nil
	}
	return &nimAliasRestorer{
		aliases:   aliases,
		blockTool: make(map[int]string),
	}
}

// restoreEventBytes processes translated SSE bytes (may contain one or more
// concatenated events). It learns tool names from content_block_start events
// and restores alias keys in input_json_delta events.
func (r *nimAliasRestorer) restoreEventBytes(event []byte) []byte {
	if r == nil {
		return event
	}
	parts := splitSSEEvents(event)
	for i := range parts {
		parts[i] = r.restoreSingleEvent(parts[i])
	}
	return bytes.Join(parts, nil)
}

func (r *nimAliasRestorer) restoreSingleEvent(event []byte) []byte {
	if r == nil {
		return event
	}
	if !bytes.Contains(event, []byte("content_block_start")) &&
		!bytes.Contains(event, []byte("input_json_delta")) {
		return event
	}
	eventType, dataJSON := splitSSEEvent(event)
	if eventType == "" || len(dataJSON) == 0 {
		return event
	}
	switch eventType {
	case "content_block_start":
		blockType := gjson.GetBytes(dataJSON, "content_block.type")
		if !blockType.Exists() || blockType.String() != "tool_use" {
			return event
		}
		blockIndex := int(gjson.GetBytes(dataJSON, "index").Int())
		toolName := gjson.GetBytes(dataJSON, "content_block.name").String()
		if toolName == "" {
			return event
		}
		r.blockTool[blockIndex] = toolName
		return event

	case "content_block_delta":
		deltaType := gjson.GetBytes(dataJSON, "delta.type")
		if !deltaType.Exists() || deltaType.String() != "input_json_delta" {
			return event
		}
		blockIndex := int(gjson.GetBytes(dataJSON, "index").Int())
		toolName, ok := r.blockTool[blockIndex]
		if !ok {
			return event
		}
		toolAliases, ok := r.aliases[toolName]
		if !ok || len(toolAliases) == 0 {
			return event
		}
		partialJSON := gjson.GetBytes(dataJSON, "delta.partial_json").String()
		if partialJSON == "" || !gjson.Valid(partialJSON) {
			return event
		}
		parsed := gjson.Parse(partialJSON)
		if !parsed.IsObject() {
			return event
		}
		restored := restoreAliasedToolArguments(parsed.Value(), toolAliases)
		if restored == nil {
			return event
		}
		restoredBytes, err := json.Marshal(restored)
		if err != nil {
			return event
		}
		// partial_json is a JSON string field in the Anthropic SSE format.
		// Use SetBytes (not SetRawBytes) so the restored object is properly
		// string-quoted, matching how the SDK translator emits it.
		newData, err := sjson.SetBytes(dataJSON, "delta.partial_json", string(restoredBytes))
		if err != nil {
			return event
		}
		return assembleSSEEvent(eventType, newData)

	default:
		return event
	}
}

// restoreAliasedToolArguments recursively walks a parsed JSON value and
// replaces alias keys with original keys in any object. Mirrors
// _restore_aliased_tool_argument_value in the Python tool_calls.py.
func restoreAliasedToolArguments(value any, aliases map[string]string) any {
	switch v := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			if original, ok := aliases[key]; ok {
				result[original] = restoreAliasedToolArguments(item, aliases)
			} else {
				result[key] = restoreAliasedToolArguments(item, aliases)
			}
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = restoreAliasedToolArguments(item, aliases)
		}
		return result
	default:
		return value
	}
}

// splitSSEEvent extracts "event" type and "data" JSON from a single SSE event
// byte slice of the form "event: <type>\ndata: <json>\n\n".
func splitSSEEvent(event []byte) (eventType string, dataJSON []byte) {
	lines := bytes.Split(event, []byte("\n"))
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			eventType = string(bytes.TrimSpace(trimmed[len("event:"):]))
		} else if bytes.HasPrefix(trimmed, []byte("data:")) {
			dataJSON = bytes.TrimSpace(trimmed[len("data:"):])
		}
	}
	return
}

// assembleSSEEvent reassembles an SSE event from its type and data JSON.
func assembleSSEEvent(eventType string, dataJSON []byte) []byte {
	out := make([]byte, 0, len(eventType)+len(dataJSON)+14)
	out = append(out, "event: "...)
	out = append(out, eventType...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, dataJSON...)
	out = append(out, '\n', '\n')
	return out
}

// splitSSEEvents splits a byte slice containing one or more SSE events into
// individual event byte slices. Each event starts with "event:" at a line
// boundary.
func splitSSEEvents(data []byte) [][]byte {
	if len(data) == 0 {
		return [][]byte{data}
	}
	var events [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if i > 0 && data[i] == 'e' && i+6 <= len(data) && bytes.Equal(data[i:i+6], []byte("event:")) {
			if data[i-1] == '\n' {
				if i > start {
					events = append(events, data[start:i])
				}
				start = i
			}
		}
	}
	if start < len(data) {
		events = append(events, data[start:])
	}
	if len(events) == 0 {
		return [][]byte{data}
	}
	return events
}
