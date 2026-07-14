package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeNimToolSchemas_RemovesBooleanSubschemas(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "test_fn",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"count": map[string]any{"type": "integer"},
							"nested": map[string]any{
								"type":                 "object",
								"additionalProperties": false,
							},
						},
						"additionalProperties": true,
						"allOf": []any{
							map[string]any{"type": "object"},
							false,
						},
					},
				},
			},
		},
	}

	sanitizeNimToolSchemas(body)

	tools := body["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)

	assert.Equal(t, "object", params["type"])
	assert.Nil(t, params["additionalProperties"])
	assert.Len(t, params["allOf"], 1)

	nested := params["properties"].(map[string]any)["nested"].(map[string]any)
	assert.Nil(t, nested["additionalProperties"])
}

func TestSanitizeNimToolSchemas_AliasesReservedName(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{
				"function": map[string]any{
					"name": "test_fn",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"type": map[string]any{"type": "string"},
						},
						"required": []any{"type"},
					},
				},
			},
		},
	}

	sanitizeNimToolSchemas(body)

	tools := body["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	assert.Nil(t, props["type"])
	assert.Equal(t, map[string]any{"type": "string"}, props["_fcc_arg_type"])
	assert.Equal(t, []any{"_fcc_arg_type"}, params["required"])

	aliases := body[nimToolArgumentAliasesKey].(map[string]map[string]string)
	assert.Equal(t, "type", aliases["test_fn"]["_fcc_arg_type"])
}

func TestBodyWithoutNimToolArgumentAliases(t *testing.T) {
	body := map[string]any{
		"tools":                   []any{},
		nimToolArgumentAliasesKey: map[string]map[string]string{"fn": {"a": "b"}},
	}
	upstream := bodyWithoutNimToolArgumentAliases(body)
	_, ok := upstream[nimToolArgumentAliasesKey]
	assert.False(t, ok)
	_, ok = body[nimToolArgumentAliasesKey]
	assert.True(t, ok)
}

func TestBodyWithoutNimToolArgumentAliases_FlattensExtraBody(t *testing.T) {
	body := map[string]any{
		"messages":    []any{},
		"temperature": 0.5,
		"extra_body": map[string]any{
			"top_k":                -1,
			"chat_template_kwargs": map[string]any{"thinking": true},
		},
	}
	upstream := bodyWithoutNimToolArgumentAliases(body)

	// extra_body should be gone from the upstream copy
	_, ok := upstream["extra_body"]
	assert.False(t, ok)

	// Its fields should be merged at the top level
	assert.Equal(t, -1, upstream["top_k"])
	ctk, ok := upstream["chat_template_kwargs"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, true, ctk["thinking"])

	// Existing top-level keys take precedence over extra_body entries
	assert.Equal(t, 0.5, upstream["temperature"])

	// Original body should still carry extra_body (copy-on-write)
	_, ok = body["extra_body"].(map[string]any)
	assert.True(t, ok)
}

func TestApplyNimRequestOptions_Defaults(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	applyNimRequestOptions(body)

	assert.Equal(t, 1.0, body["temperature"])
	assert.Equal(t, 1.0, body["top_p"])
	assert.Equal(t, defaultNimMaxTokens, body["max_tokens"])
	assert.Equal(t, true, body["parallel_tool_calls"])

	extra := body["extra_body"].(map[string]any)
	ctk := extra["chat_template_kwargs"].(map[string]any)
	assert.Equal(t, true, ctk["enable_thinking"])
	assert.Equal(t, false, ctk["clear_thinking"])
	assert.Nil(t, ctk["thinking"])
	assert.Nil(t, ctk["reasoning_budget"])
}

func TestApplyNimRequestOptions_StripsReasoningEffort(t *testing.T) {
	body := map[string]any{
		"messages":       []any{},
		"reasoning_effort": "high",
	}
	applyNimRequestOptions(body)

	assert.Nil(t, body["reasoning_effort"])
	extra := body["extra_body"].(map[string]any)
	ctk := extra["chat_template_kwargs"].(map[string]any)
	assert.Equal(t, true, ctk["enable_thinking"])
	assert.Equal(t, false, ctk["clear_thinking"])
}

func TestApplyNimRequestOptions_StripsChatTemplateReasoningKeys(t *testing.T) {
	body := map[string]any{
		"messages": []any{},
		"extra_body": map[string]any{
			"chat_template_kwargs": map[string]any{
				"thinking":         true,
				"reasoning_budget": 4096,
			},
		},
	}
	applyNimRequestOptions(body)

	extra := body["extra_body"].(map[string]any)
	ctk := extra["chat_template_kwargs"].(map[string]any)
	assert.Nil(t, ctk["thinking"])
	assert.Nil(t, ctk["reasoning_budget"])
	assert.Equal(t, true, ctk["enable_thinking"])
	assert.Equal(t, false, ctk["clear_thinking"])
}

func TestApplyNimRequestOptions_PreservesExistingMaxTokens(t *testing.T) {
	body := map[string]any{"max_tokens": 2048}
	applyNimRequestOptions(body)
	assert.Equal(t, 2048, body["max_tokens"])
}

func TestCloneBodyWithoutReasoningBudget(t *testing.T) {
	body := map[string]any{
		"extra_body": map[string]any{
			"reasoning_budget": 100,
			"chat_template_kwargs": map[string]any{
				"reasoning_budget": 200,
				"thinking":         true,
			},
		},
	}
	cloned := cloneBodyWithoutReasoningBudget(body)
	extra := cloned["extra_body"].(map[string]any)
	assert.Nil(t, extra["reasoning_budget"])
	ctk := extra["chat_template_kwargs"].(map[string]any)
	assert.Nil(t, ctk["reasoning_budget"])
	assert.Equal(t, true, ctk["thinking"])

	_, ok := body["extra_body"].(map[string]any)["reasoning_budget"]
	assert.True(t, ok, "original should be unchanged")
}

func TestCloneBodyWithoutChatTemplate(t *testing.T) {
	body := map[string]any{
		"extra_body": map[string]any{"chat_template": "tpl", "top_k": 10},
	}
	cloned := cloneBodyWithoutChatTemplate(body)
	extra := cloned["extra_body"].(map[string]any)
	assert.Nil(t, extra["chat_template"])
	assert.Equal(t, 10, extra["top_k"])
}

func TestCloneBodyWithoutReasoningContent(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "reasoning_content": "r1"},
			map[string]any{"role": "user"},
		},
	}
	cloned := cloneBodyWithoutReasoningContent(body)
	msgs := cloned["messages"].([]any)
	assert.Nil(t, msgs[0].(map[string]any)["reasoning_content"])
	assert.Nil(t, msgs[1].(map[string]any)["reasoning_content"])
}

func TestApplyNimRequestOptions_StripsTopLevelThinking(t *testing.T) {
	body := map[string]any{
		"messages": []any{},
		"thinking": map[string]any{
			"type":          "adaptive",
			"budget_tokens": 4096,
		},
	}
	applyNimRequestOptions(body)
	_, ok := body["thinking"].(map[string]any)
	assert.False(t, ok)
	extra := body["extra_body"].(map[string]any)
	ctk := extra["chat_template_kwargs"].(map[string]any)
	assert.Equal(t, true, ctk["enable_thinking"])
	assert.Equal(t, false, ctk["clear_thinking"])
}

func TestSetExtra_SkipsExistingAndDefaultValues(t *testing.T) {
	eb := map[string]any{"top_k": 5}
	setExtra(eb, "top_k", -1, -1) // already exists, should skip
	assert.Equal(t, 5, eb["top_k"])

	setExtra(eb, "min_p", 0.0, 0.0) // equal to default, should skip
	assert.Nil(t, eb["min_p"])

	setExtra(eb, "new_key", 42, 0) // new key, value differs from default
	assert.Equal(t, 42, eb["new_key"])
}
