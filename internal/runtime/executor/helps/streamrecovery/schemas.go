package streamrecovery

import (
	"encoding/json"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ToolSchema captures a tool's input schema, used for tool-JSON repair.
// Mirrors ToolSchema in recovery.py.
type ToolSchema struct {
	Name        string
	InputSchema map[string]any
}

// ToolSchemasByName returns tool input schemas keyed by tool name, extracted
// from the original Anthropic request payload. Mirrors
// tool_schemas_by_name in recovery.py.
//
// The request payload is the original (pre-translation) Anthropic request. Tool
// entries may be either client-side dict-shaped or have an OpenAI-style
// "function.name" / "function.parameters" shape. We tolerate both because the
// Go translator may pass either representation through the opts.OriginalRequest.
func ToolSchemasByName(originalRequest []byte) map[string]ToolSchema {
	schemas := make(map[string]ToolSchema)
	if len(originalRequest) == 0 {
		return schemas
	}
	var req struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
			Function    struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(originalRequest, &req); err != nil {
		return schemas
	}
	for _, t := range req.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			name = strings.TrimSpace(t.Function.Name)
		}
		if name == "" {
			continue
		}
		raw := t.InputSchema
		if len(raw) == 0 {
			raw = t.Function.Parameters
		}
		var schema map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &schema); err != nil {
				schema = nil
			}
		}
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		schemas[name] = ToolSchema{Name: name, InputSchema: deepCopyMapAny(schema)}
	}
	return schemas
}

// ParseCompleteToolInput attempts to parse a JSON string as a tool input dict
// and validate it against the schema. Returns nil if parsing or validation
// fails. Mirrors parse_complete_tool_input in recovery.py.
func ParseCompleteToolInput(rawJSON, toolName string, schemas map[string]ToolSchema) map[string]any {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		return nil
	}
	if !validateToolInput(toolName, parsed, schemas) {
		return nil
	}
	return parsed
}

func validateToolInput(toolName string, parsed map[string]any, schemas map[string]ToolSchema) bool {
	schema, ok := schemas[toolName]
	if !ok || len(schema.InputSchema) == 0 {
		return true
	}
	compiler := jsonschema.NewCompiler()
	// Register the schema under a synthetic URL.
	loader := &rawLoader{schema.InputSchema}
	if err := compiler.AddResource("schema.json", loader); err != nil {
		return true // invalid meta schema: permissive (matches Python behavior)
	}
	validator, err := compiler.Compile("schema.json")
	if err != nil {
		return true // schema itself invalid: permissive
	}
	if err := validator.Validate(parsed); err != nil {
		return false
	}
	return true
}

// AcceptToolJSONRepair returns the suffix to append to prefix so that
// prefix+suffix is a valid complete tool input, or nil when no candidate suffix
// works. Mirrors accept_tool_json_repair in recovery.py.
func AcceptToolJSONRepair(prefix, candidate, toolName string, schemas map[string]ToolSchema) (suffix string, parsed map[string]any, ok bool) {
	for _, s := range repairSuffixCandidates(prefix, candidate) {
		combined := prefix + s
		if p := ParseCompleteToolInput(combined, toolName, schemas); p != nil {
			return s, p, true
		}
	}
	return "", nil, false
}

func repairSuffixCandidates(prefix, candidate string) []string {
	raw := strings.TrimSpace(candidate)
	if raw == "" {
		return nil
	}
	var out []string
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) > 0 && strings.HasPrefix(lines[0], "```") {
			lines = lines[1:]
		}
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			lines = lines[:len(lines)-1]
		}
		raw = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	out = append(out, raw)
	if strings.HasPrefix(raw, prefix) {
		out = append(out, raw[len(prefix):])
	}
	return dedupStrings(out)
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func deepCopyMapAny(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValueAny(v)
	}
	return out
}

func deepCopyValueAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopyMapAny(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopyValueAny(item)
		}
		return out
	default:
		return v
	}
}

// rawLoader is a jsonschema.Loader backed by a Go map, registered with the
// jsonschema compiler via AddResource.
type rawLoader struct{ m map[string]any }

func (l *rawLoader) LoadURL(_ string) (any, error) { return deepCopyMapAny(l.m), nil }
