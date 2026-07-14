# NVIDIA NIM Executor & WebUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the Python `cc/providers/nvidia_nim/` provider into the Go proxy as a dedicated executor with its own WebUI section, while leaving `OpenAICompatExecutor` untouched.

**Architecture:** A new `NvidiaNimExecutor` in `internal/runtime/executor/` reuses the package's HTTP/translation/usage helpers but implements NIM-specific tool-schema sanitization, request-option injection, and 400 retry downgrades locally. The WebUI adds a dedicated `NvidiaSection` and edit routes that read/write the existing `openai-compatibility` config section. `sdk/cliproxy/service.go` binds the executor when the provider key is `nvidia` or `nvidia-nim`.

**Tech Stack:** Go 1.24, Gin, `tidwall/sjson`/`gjson`, React/TypeScript, Vite, SCSS modules, i18next.

---

## Task 1: Create the NVIDIA NIM executor scaffold

**Files:**
- Create: `internal/runtime/executor/nvidia_nim_executor.go`
- Test: `internal/runtime/executor/nvidia_nim_executor_test.go`

- [ ] **Step 1.1: Write the executor scaffold and credential resolution**

Create `internal/runtime/executor/nvidia_nim_executor.go` with the following content:

```go
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	nvidiaNimDefaultBaseURL       = "https://integrate.api.nvidia.com/v1"
	nimToolArgumentAliasesKey     = "_fcc_nim_tool_argument_aliases"
	nimToolParameterAliasPrefix   = "_fcc_arg_"
)

// NvidiaNimExecutor implements a dedicated executor for NVIDIA NIM.
type NvidiaNimExecutor struct {
	provider string
	cfg      *config.Config
}

// NewNvidiaNimExecutor creates an executor bound to a provider key ("nvidia" or "nvidia-nim").
func NewNvidiaNimExecutor(provider string, cfg *config.Config) *NvidiaNimExecutor {
	return &NvidiaNimExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *NvidiaNimExecutor) Identifier() string { return e.provider }

// PrepareRequest injects NVIDIA NIM credentials into the outgoing HTTP request.
func (e *NvidiaNimExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects NVIDIA NIM credentials and executes the request.
func (e *NvidiaNimExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nvidia nim executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *NvidiaNimExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	if baseURL == "" {
		baseURL = nvidiaNimDefaultBaseURL
	}
	return
}

// Refresh is a no-op for API-key based providers.
func (e *NvidiaNimExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("nvidia nim executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}
```

- [ ] **Step 1.2: Add the basic compile check**

Run:

```bash
go build ./internal/runtime/executor
```

Expected: no errors.

---

## Task 2: Port tool schema sanitization

**Files:**
- Modify: `internal/runtime/executor/nvidia_nim_executor.go`
- Test: `internal/runtime/executor/nvidia_nim_executor_test.go`

- [ ] **Step 2.1: Implement NIM schema helpers**

Append to `nvidia_nim_executor.go`:

```go
func sanitizeNimToolSchemas(body map[string]any) {
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}

	toolArgumentAliases := make(map[string]map[string]string)
	sanitizedTools := make([]any, 0, len(tools))

	for _, tool := range tools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			sanitizedTools = append(sanitizedTools, tool)
			continue
		}
		sanitizedTool := shallowCopyMap(toolMap)
		function, ok := toolMap["function"].(map[string]any)
		if ok {
			sanitizedFunction := shallowCopyMap(function)
			parameters, ok := function["parameters"].(map[string]any)
			if ok {
				sanitizedParameters := sanitizeNimSchemaNode(parameters)
				sanitizedParameters, aliases := aliasNimToolParameters(sanitizedParameters)
				sanitizedFunction["parameters"] = sanitizedParameters
				toolName, _ := function["name"].(string)
				if len(aliases) > 0 && toolName != "" {
					toolArgumentAliases[toolName] = aliases
				}
			}
			sanitizedTool["function"] = sanitizedFunction
		}
		sanitizedTools = append(sanitizedTools, sanitizedTool)
	}

	body["tools"] = sanitizedTools
	if len(toolArgumentAliases) > 0 {
		body[nimToolArgumentAliasesKey] = toolArgumentAliases
	} else {
		delete(body, nimToolArgumentAliasesKey)
	}
}

func shallowCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var nimSchemaValueKeys = map[string]struct{}{
	"additionalProperties": {}, "additionalItems": {}, "unevaluatedProperties": {},
	"unevaluatedItems": {}, "items": {}, "contains": {}, "propertyNames": {},
	"if": {}, "then": {}, "else": {}, "not": {},
}

var nimSchemaListKeys = map[string]struct{}{
	"allOf": {}, "anyOf": {}, "oneOf": {}, "prefixItems": {},
}

var nimSchemaMapKeys = map[string]struct{}{
	"properties": {}, "patternProperties": {}, "$defs": {}, "definitions": {}, "dependentSchemas": {},
}

func sanitizeNimSchemaNode(value any) any {
	switch v := value.(type) {
	case bool:
		return nil
	case map[string]any:
		sanitized := make(map[string]any, len(v))
		for key, item := range v {
			if _, isValueKey := nimSchemaValueKeys[key]; isValueKey {
				if cleaned := sanitizeNimSchemaNode(item); cleaned != nil {
					sanitized[key] = cleaned
				}
			} else if _, isListKey := nimSchemaListKeys[key]; isListKey {
				if list, ok := item.([]any); ok {
					cleanedList := make([]any, 0, len(list))
					for _, elem := range list {
						if cleaned := sanitizeNimSchemaNode(elem); cleaned != nil {
							cleanedList = append(cleanedList, cleaned)
						}
					}
					if len(cleanedList) > 0 {
						sanitized[key] = cleanedList
					}
				}
			} else if _, isMapKey := nimSchemaMapKeys[key]; isMapKey {
				if m, ok := item.(map[string]any); ok {
					cleanedMap := make(map[string]any, len(m))
					for mk, mv := range m {
						if cleaned := sanitizeNimSchemaNode(mv); cleaned != nil {
							cleanedMap[mk] = cleaned
						}
					}
					sanitized[key] = cleanedMap
				}
			} else {
				sanitized[key] = item
			}
		}
		return sanitized
	case []any:
		sanitized := make([]any, 0, len(v))
		for _, item := range v {
			if cleaned := sanitizeNimSchemaNode(item); cleaned != nil {
				sanitized = append(sanitized, cleaned)
			}
		}
		return sanitized
	default:
		return value
	}
}

func aliasNimToolParameters(parameters map[string]any) (map[string]any, map[string]string) {
	reserved := collectNimToolPropertyNames(parameters)
	aliasToOriginal := make(map[string]string)
	originalToAlias := make(map[string]string)
	aliased := aliasNimSchemaPropertyNames(parameters, reserved, aliasToOriginal, originalToAlias)
	if len(aliasToOriginal) == 0 {
		return parameters, nil
	}
	return aliased, aliasToOriginal
}

func collectNimToolPropertyNames(value any) map[string]struct{} {
	names := make(map[string]struct{})
	var walk func(any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if props, ok := node["properties"].(map[string]any); ok {
				for name := range props {
					names[name] = struct{}{}
				}
				for _, schema := range props {
					walk(schema)
				}
			}
			for key, item := range node {
				if key != "properties" {
					walk(item)
				}
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(value)
	return names
}

func aliasNimSchemaPropertyNames(value any, reserved map[string]struct{}, aliasToOriginal, originalToAlias map[string]string) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = aliasNimSchemaPropertyNames(item, reserved, aliasToOriginal, originalToAlias)
		}
		return out
	case map[string]any:
		aliased := make(map[string]any, len(v))
		localAliases := make(map[string]string)
		if props, ok := v["properties"].(map[string]any); ok {
			aliasedProps := make(map[string]any, len(props))
			for name, schema := range props {
				aliasedSchema := aliasNimSchemaPropertyNames(schema, reserved, aliasToOriginal, originalToAlias)
				if needsNimToolParameterAlias(name) {
					alias := originalToAlias[name]
					if alias == "" {
						alias = makeNimToolParameterAlias(name, reserved)
						aliasToOriginal[alias] = name
						originalToAlias[name] = alias
					}
					localAliases[name] = alias
					aliasedProps[alias] = aliasedSchema
				} else {
					aliasedProps[name] = aliasedSchema
				}
			}
			aliased["properties"] = aliasedProps
		}
		for key, item := range v {
			if key == "properties" {
				continue
			}
			if key == "required" {
				if reqList, ok := item.([]any); ok {
					newReq := make([]any, len(reqList))
					for i, r := range reqList {
						if s, ok := r.(string); ok {
							if alias, has := localAliases[s]; has {
								newReq[i] = alias
								continue
							}
						}
						newReq[i] = r
					}
					aliased[key] = newReq
					continue
				}
			}
			aliased[key] = aliasNimSchemaPropertyNames(item, reserved, aliasToOriginal, originalToAlias)
		}
		return aliased
	default:
		return value
	}
}

func needsNimToolParameterAlias(name string) bool {
	return name == "type"
}

func makeNimToolParameterAlias(name string, reserved map[string]struct{}) string {
	safe := strings.Builder{}
	for _, ch := range name {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
			safe.WriteRune(ch)
		} else {
			safe.WriteRune('_')
		}
	}
	tail := strings.Trim(safe.String(), "_")
	if tail == "" {
		tail = "arg"
	}
	candidate := nimToolParameterAliasPrefix + tail
	alias := candidate
	suffix := 2
	for {
		if _, exists := reserved[alias]; !exists {
			break
		}
		alias = fmt.Sprintf("%s_%d", candidate, suffix)
		suffix++
	}
	reserved[alias] = struct{}{}
	return alias
}

func bodyWithoutNimToolArgumentAliases(body map[string]any) map[string]any {
	if _, ok := body[nimToolArgumentAliasesKey]; !ok {
		return body
	}
	upstream := shallowCopyMap(body)
	delete(upstream, nimToolArgumentAliasesKey)
	return upstream
}
```

- [ ] **Step 2.2: Write tests for tool schema sanitization**

Append to `nvidia_nim_executor_test.go`:

```go
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
								"type": "object",
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
		"tools": []any{},
		nimToolArgumentAliasesKey: map[string]map[string]string{"fn": {"a": "b"}},
	}
	upstream := bodyWithoutNimToolArgumentAliases(body)
	_, ok := upstream[nimToolArgumentAliasesKey]
	assert.False(t, ok)
	_, ok = body[nimToolArgumentAliasesKey]
	assert.True(t, ok)
}
```

- [ ] **Step 2.3: Run tool-schema tests**

```bash
go test ./internal/runtime/executor -run TestSanitizeNimToolSchemas -v
go test ./internal/runtime/executor -run TestBodyWithoutNimToolArgumentAliases -v
```

Expected: PASS.

---

## Task 3: Port request options

**Files:**
- Modify: `internal/runtime/executor/nvidia_nim_executor.go`
- Test: `internal/runtime/executor/nvidia_nim_executor_test.go`

- [ ] **Step 3.1: Implement request option injection**

Append helpers to `nvidia_nim_executor.go`:

```go
func applyNimRequestOptions(body map[string]any, thinkingEnabled bool) {
	sanitizeNimToolSchemas(body)

	maxTokens := defaultNimMaxTokens
	if v, ok := body["max_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	} else if v, ok := body["max_tokens"].(int); ok && v > 0 {
		maxTokens = v
	}
	body["max_tokens"] = maxTokens

	if body["temperature"] == nil {
		body["temperature"] = 1.0
	}
	if body["top_p"] == nil {
		body["top_p"] = 1.0
	}

	extraBody := make(map[string]any)
	if eb, ok := body["extra_body"].(map[string]any); ok {
		for k, v := range eb {
			extraBody[k] = v
		}
	}

	if thinkingEnabled {
		ctk, ok := extraBody["chat_template_kwargs"].(map[string]any)
		if !ok {
			ctk = make(map[string]any)
			extraBody["chat_template_kwargs"] = ctk
		}
		ctk["thinking"] = true
		ctk["enable_thinking"] = true
		if _, exists := ctk["reasoning_budget"]; !exists {
			ctk["reasoning_budget"] = maxTokens
		}
	}

	setExtra(extraBody, "top_k", -1, -1)
	setExtra(extraBody, "min_p", 0.0, 0.0)
	setExtra(extraBody, "repetition_penalty", 1.0, 1.0)
	setExtra(extraBody, "min_tokens", 0, 0)
	setExtra(extraBody, "chat_template", nil, nil)
	setExtra(extraBody, "request_id", nil, nil)
	setExtra(extraBody, "ignore_eos", false, false)

	if len(extraBody) > 0 {
		body["extra_body"] = extraBody
	}

	body["parallel_tool_calls"] = true
}

func setExtra(extraBody map[string]any, key string, value, ignore any) {
	if _, exists := extraBody[key]; exists {
		return
	}
	if value == nil {
		return
	}
	if ignore != nil && value == ignore {
		return
	}
	extraBody[key] = value
}

const defaultNimMaxTokens = 8192
```

- [ ] **Step 3.2: Write request option tests**

Append to `nvidia_nim_executor_test.go`:

```go
func TestApplyNimRequestOptions_Defaults(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	applyNimRequestOptions(body, true)

	assert.Equal(t, 1.0, body["temperature"])
	assert.Equal(t, 1.0, body["top_p"])
	assert.Equal(t, defaultNimMaxTokens, body["max_tokens"])
	assert.Equal(t, true, body["parallel_tool_calls"])

	extra := body["extra_body"].(map[string]any)
	ctk := extra["chat_template_kwargs"].(map[string]any)
	assert.Equal(t, true, ctk["thinking"])
	assert.Equal(t, true, ctk["enable_thinking"])
	assert.Equal(t, defaultNimMaxTokens, ctk["reasoning_budget"])
}

func TestApplyNimRequestOptions_PreservesExistingMaxTokens(t *testing.T) {
	body := map[string]any{"max_tokens": 2048}
	applyNimRequestOptions(body, false)
	assert.Equal(t, 2048, body["max_tokens"])
}
```

- [ ] **Step 3.3: Run request option tests**

```bash
go test ./internal/runtime/executor -run TestApplyNimRequestOptions -v
```

Expected: PASS.

---

## Task 4: Port retry downgrade helpers

**Files:**
- Modify: `internal/runtime/executor/nvidia_nim_executor.go`
- Test: `internal/runtime/executor/nvidia_nim_executor_test.go`

- [ ] **Step 4.1: Implement retry downgrade cloning**

Append to `nvidia_nim_executor.go`:

```go
func cloneBodyWithoutReasoningBudget(body map[string]any) map[string]any {
	return cloneStripExtraBody(body, stripReasoningBudgetFields)
}

func cloneBodyWithoutChatTemplate(body map[string]any) map[string]any {
	return cloneStripExtraBody(body, stripChatTemplateField)
}

func cloneBodyWithoutReasoningContent(body map[string]any) map[string]any {
	cloned := deepCopyMap(body)
	if !stripMessageReasoningContent(cloned) {
		return nil
	}
	return cloned
}

func cloneStripExtraBody(body map[string]any, strip func(map[string]any) bool) map[string]any {
	cloned := deepCopyMap(body)
	extraBody, ok := cloned["extra_body"].(map[string]any)
	if !ok {
		return nil
	}
	if !strip(extraBody) {
		return nil
	}
	if len(extraBody) == 0 {
		delete(cloned, "extra_body")
	}
	return cloned
}

func stripReasoningBudgetFields(extraBody map[string]any) bool {
	removed := false
	if _, exists := extraBody["reasoning_budget"]; exists {
		delete(extraBody, "reasoning_budget")
		removed = true
	}
	if ctk, ok := extraBody["chat_template_kwargs"].(map[string]any); ok {
		if _, exists := ctk["reasoning_budget"]; exists {
			delete(ctk, "reasoning_budget")
			removed = true
		}
	}
	return removed
}

func stripChatTemplateField(extraBody map[string]any) bool {
	if _, exists := extraBody["chat_template"]; exists {
		delete(extraBody, "chat_template")
		return true
	}
	return false
}

func stripMessageReasoningContent(body map[string]any) bool {
	messages, ok := body["messages"].([]any)
	if !ok {
		return false
	}
	removed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := msg["reasoning_content"]; exists {
			delete(msg, "reasoning_content")
			removed = true
		}
	}
	return removed
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		return x
	}
}
```

- [ ] **Step 4.2: Write retry downgrade tests**

Append to `nvidia_nim_executor_test.go`:

```go
func TestCloneBodyWithoutReasoningBudget(t *testing.T) {
	body := map[string]any{
		"extra_body": map[string]any{
			"reasoning_budget": 100,
			"chat_template_kwargs": map[string]any{
				"reasoning_budget": 200,
				"thinking": true,
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
```

- [ ] **Step 4.3: Run retry downgrade tests**

```bash
go test ./internal/runtime/executor -run TestCloneBodyWithout -v
```

Expected: PASS.

---

## Task 5: Implement Execute, ExecuteStream, and CountTokens

**Files:**
- Modify: `internal/runtime/executor/nvidia_nim_executor.go`

- [ ] **Step 5.1: Implement non-streaming Execute**

Append to `nvidia_nim_executor.go`:

```go
// Execute runs a non-streaming NVIDIA NIM chat completion request.
func (e *NvidiaNimExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImages(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	endpoint := "/chat/completions"
	if opts.Alt == "responses/compact" {
		to = sdktranslator.FromString("openai-response")
		endpoint = "/responses/compact"
	}

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, opts.Stream)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, opts.Stream)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}

	thinkingEnabled := thinking.IsEnabled(req.Model)
	bodyMap, err := unmarshalNimBody(translated)
	if err != nil {
		return resp, fmt.Errorf("nvidia nim executor: unmarshal translated body: %w", err)
	}
	applyNimRequestOptions(bodyMap, thinkingEnabled)

	var upstreamBodyMap map[string]any
	upstreamBodyMap, err = e.sendNimRequest(ctx, auth, baseURL, apiKey, endpoint, bodyMap, reporter, &resp, to, from, req, opts)
	if err != nil {
		if retryBody := e.retryBodyForError(err, bodyMap); retryBody != nil {
			log.Warnf("nvidia nim executor: retrying after 400 downgrade")
			_, err = e.sendNimRequest(ctx, auth, baseURL, apiKey, endpoint, retryBody, reporter, &resp, to, from, req, opts)
		}
		if err != nil {
			return resp, err
		}
	}

	_ = upstreamBodyMap
	return resp, nil
}

func unmarshalNimBody(data []byte) (map[string]any, error) {
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	if body == nil {
		body = make(map[string]any)
	}
	return body, nil
}
```

- [ ] **Step 5.2: Implement the shared request sender**

Append to `nvidia_nim_executor.go`:

```go
func (e *NvidiaNimExecutor) sendNimRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	baseURL, apiKey, endpoint string,
	bodyMap map[string]any,
	reporter *helps.UsageReporter,
	resp *cliproxyexecutor.Response,
	to, from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (map[string]any, error) {
	upstreamBodyMap := bodyWithoutNimToolArgumentAliases(bodyMap)
	translated, err := json.Marshal(upstreamBodyMap)
	if err != nil {
		return nil, fmt.Errorf("nvidia nim executor: marshal body: %w", err)
	}

	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-nvidia-nim")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("nvidia nim executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("nvidia nim request error, status: %d, message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(body))
	reporter.EnsurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, body, &param)
	*resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return upstreamBodyMap, nil
}

func (e *NvidiaNimExecutor) retryBodyForError(err error, body map[string]any) map[string]any {
	se, ok := err.(statusErr)
	if !ok || se.code != http.StatusBadRequest {
		return nil
	}
	text := strings.ToLower(se.msg)
	if strings.Contains(text, "reasoning_budget") {
		return cloneBodyWithoutReasoningBudget(body)
	}
	if strings.Contains(text, "chat_template") {
		return cloneBodyWithoutChatTemplate(body)
	}
	if strings.Contains(text, "reasoning_content") {
		return cloneBodyWithoutReasoningContent(body)
	}
	return nil
}
```

- [ ] **Step 5.3: Implement ExecuteStream**

Append to `nvidia_nim_executor.go`:

```go
// ExecuteStream runs a streaming NVIDIA NIM chat completion request.
func (e *NvidiaNimExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImagesStream(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, true)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	translated, _ = sjson.SetBytes(translated, "stream_options.include_usage", true)

	thinkingEnabled := thinking.IsEnabled(req.Model)
	bodyMap, err := unmarshalNimBody(translated)
	if err != nil {
		return nil, fmt.Errorf("nvidia nim executor: unmarshal translated body: %w", err)
	}
	applyNimRequestOptions(bodyMap, thinkingEnabled)

	result, err := e.sendNimStream(ctx, auth, baseURL, apiKey, bodyMap, reporter, to, from, req, opts)
	if err != nil {
		if retryBody := e.retryBodyForError(err, bodyMap); retryBody != nil {
			log.Warnf("nvidia nim executor: retrying stream after 400 downgrade")
			return e.sendNimStream(ctx, auth, baseURL, apiKey, retryBody, reporter, to, from, req, opts)
		}
		return nil, err
	}
	return result, nil
}

func (e *NvidiaNimExecutor) sendNimStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	baseURL, apiKey string,
	bodyMap map[string]any,
	reporter *helps.UsageReporter,
	to, from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	upstreamBodyMap := bodyWithoutNimToolArgumentAliases(bodyMap)
	translated, err := json.Marshal(upstreamBodyMap)
	if err != nil {
		return nil, fmt.Errorf("nvidia nim executor: marshal stream body: %w", err)
	}

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-nvidia-nim")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("nvidia nim executor: close stream error body error: %v", errClose)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("nvidia nim stream request error, status: %d, message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("nvidia nim executor: close stream body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			trimmedLine := bytes.TrimSpace(line)
			if len(trimmedLine) == 0 {
				continue
			}
			if !bytes.HasPrefix(trimmedLine, []byte("data:")) {
				if bytes.HasPrefix(trimmedLine, []byte(":")) || bytes.HasPrefix(trimmedLine, []byte("event:")) ||
					bytes.HasPrefix(trimmedLine, []byte("id:")) || bytes.HasPrefix(trimmedLine, []byte("retry:")) {
					continue
				}
				if bytes.HasPrefix(trimmedLine, []byte("{")) || bytes.HasPrefix(trimmedLine, []byte("[")) {
					streamErr := statusErr{code: http.StatusBadGateway, msg: string(trimmedLine)}
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
					case <-ctx.Done():
					}
					return
				}
				continue
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(trimmedLine), &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		} else {
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]"), &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}
```

Add the missing `bufio` import at the top of the file:

```go
import (
	"bufio"
	"bytes"
	...
)
```

- [ ] **Step 5.4: Implement CountTokens**

Append to `nvidia_nim_executor.go`:

```go
// CountTokens returns an approximate token count for the request.
func (e *NvidiaNimExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelForCounting := baseModel

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("nvidia nim executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("nvidia nim executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}
```

- [ ] **Step 5.5: Add image delegation wrappers**

Append to `nvidia_nim_executor.go`:

```go
func (e *NvidiaNimExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	compat := &OpenAICompatExecutor{provider: e.provider, cfg: e.cfg}
	return compat.executeImages(ctx, auth, req, opts, endpointPath)
}

func (e *NvidiaNimExecutor) executeImagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	compat := &OpenAICompatExecutor{provider: e.provider, cfg: e.cfg}
	return compat.executeImagesStream(ctx, auth, req, opts, endpointPath)
}
```

- [ ] **Step 5.6: Build the executor package**

```bash
go build ./internal/runtime/executor
```

Expected: no errors.

---

## Task 6: Bind the executor in the service

**Files:**
- Modify: `sdk/cliproxy/service.go`

- [ ] **Step 6.1: Register NvidiaNimExecutor for NIM provider keys**

In `sdk/cliproxy/service.go`, locate `ensureExecutorsForAuthWithMode`. Inside the openai-compatibility branch, add the NIM check before registering the generic compat executor:

```go
if compatProviderKey, _, isCompat := openAICompatInfoFromAuth(a); isCompat {
	if compatProviderKey == "" {
		compatProviderKey = strings.ToLower(strings.TrimSpace(a.Provider))
	}
	if compatProviderKey == "" {
		compatProviderKey = "openai-compatibility"
	}
	lower := strings.ToLower(compatProviderKey)
	if lower == "nvidia" || lower == "nvidia-nim" {
		s.coreManager.RegisterExecutor(executor.NewNvidiaNimExecutor(compatProviderKey, s.cfg))
		return
	}
	s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(compatProviderKey, s.cfg))
	return
}
```

Also add a fallback in the `default` branch of the provider switch:

```go
default:
	providerKey := strings.ToLower(strings.TrimSpace(a.Provider))
	if providerKey == "" {
		providerKey = "openai-compatibility"
	}
	if providerKey == "nvidia" || providerKey == "nvidia-nim" {
		s.coreManager.RegisterExecutor(executor.NewNvidiaNimExecutor(providerKey, s.cfg))
		return
	}
	s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(providerKey, s.cfg))
```

- [ ] **Step 6.2: Register baseline NIM executor in home mode**

In `registerHomeExecutors`, after the OpenAICompat baseline:

```go
s.coreManager.RegisterExecutor(executor.NewNvidiaNimExecutor("nvidia", s.cfg))
```

- [ ] **Step 6.3: Build the service**

```bash
go build ./sdk/cliproxy
```

Expected: no errors.

---

## Task 7: Add NVIDIA icon asset

**Files:**
- Create: `webui/src/assets/icons/nvidia.svg`

- [ ] **Step 7.1: Create the icon**

```svg
<svg fill="none" height="1em" style="flex:none;line-height:1" viewBox="0 0 24 24" width="1em" xmlns="http://www.w3.org/2000/svg">
  <title>NVIDIA</title>
  <rect fill="#76B900" height="24" rx="4" width="24"/>
  <path d="M7.5 7.5h3v9h-3v-9zm6 0h3v9h-3v-9zm-6 4.5h6v3h-6v-3z" fill="#fff"/>
</svg>
```

---

## Task 8: Add NVIDIA translation keys

**Files:**
- Modify: `webui/src/i18n/locales/en.json`, `zh-CN.json`, `zh-TW.json`, `ru.json`

- [ ] **Step 8.1: Add English keys**

After the `openai_...` keys in `ai_providers`, add:

```json
"nvidia_title": "NVIDIA NIM Providers",
"nvidia_add_button": "Add NIM Provider",
"nvidia_empty_title": "No NVIDIA NIM Providers",
"nvidia_empty_desc": "Click the button above to add the first NVIDIA NIM provider",
"nvidia_filtered_empty_title": "No matching NIM providers",
"nvidia_filtered_empty_desc": "No NIM providers match the current model filter. Clear the filter and try again.",
"nvidia_add_modal_title": "Add NVIDIA NIM Provider",
"nvidia_edit_modal_title": "Edit NVIDIA NIM Provider",
"nvidia_add_modal_name_label": "Provider Name:",
"nvidia_add_modal_name_placeholder": "e.g.: nvidia",
"nvidia_add_modal_url_label": "Base URL:",
"nvidia_add_modal_url_placeholder": "e.g.: https://integrate.api.nvidia.com/v1",
"nvidia_add_modal_keys_label": "API Keys",
"nvidia_edit_modal_keys_label": "API Keys",
"nvidia_keys_hint": "Add each NVIDIA API key separately with an optional proxy URL.",
"nvidia_key_placeholder": "nvapi-... key",
"nvidia_add_modal_models_label": "Model List (name[, alias] one per line):",
"nvidia_edit_modal_models_label": "Model List (name[, alias] one per line):",
"nvidia_models_hint": "Example: nvidia/llama-3.3-nemotron-super-49b-v1 or deepseek-ai/deepseek-r1, deepseek-r1-nim",
"nvidia_model_name_placeholder": "Model name, e.g. nvidia/llama-3.3-nemotron-super-49b-v1",
"nvidia_delete_confirm": "Are you sure you want to delete this NVIDIA NIM provider?",
"nvidia_test_title": "Connection Test",
"nvidia_test_hint": "Send a /chat/completions request with the current settings to verify availability.",
"nvidia_test_model": "Test Model",
"nvidia_test_url_required": "Please provide a valid Base URL before testing",
"nvidia_test_key_required": "Please add at least one API key before testing",
"nvidia_test_model_required": "Please select a model to test",
"nvidia_provider_required": "Please fill in provider name and Base URL",
"nvidia_provider_added": "NVIDIA NIM provider added successfully",
"nvidia_provider_updated": "NVIDIA NIM provider updated successfully",
"nvidia_provider_deleted": "NVIDIA NIM provider deleted successfully",
"nvidia_models_fetch_button": "Fetch via /models",
"nvidia_models_fetch_title": "Pick Models from /models",
"nvidia_models_fetch_hint": "Call the /models endpoint using the Base URL above, sending the first API key as Bearer plus custom headers.",
"nvidia_models_fetch_added": "{{count}} new models added"
```

- [ ] **Step 8.2: Add translated keys**

Apply the same keys to `zh-CN.json`, `zh-TW.json`, and `ru.json` with appropriate translations (or copy English as fallback).

---

## Task 9: Create the NVIDIA section component

**Files:**
- Create: `webui/src/components/providers/NvidiaSection/NvidiaSection.tsx`
- Create: `webui/src/components/providers/NvidiaSection/index.ts`

- [ ] **Step 9.1: Create the component**

Copy `webui/src/components/providers/OpenAISection/OpenAISection.tsx` to `NvidiaSection/NvidiaSection.tsx` and apply these replacements:

1. Rename the component to `NvidiaSection`.
2. Replace `OpenAISectionProps` with `NvidiaSectionProps` (same fields).
3. In `getOpenAIProviderKey` usage, keep the helper but import it from `../utils`.
4. Replace all `openai_` i18n keys with `nvidia_` keys:
   - `ai_providers.openai_title` → `ai_providers.nvidia_title`
   - `ai_providers.openai_add_button` → `ai_providers.nvidia_add_button`
   - `ai_providers.openai_empty_title` → `ai_providers.nvidia_empty_title`
   - `ai_providers.openai_empty_desc` → `ai_providers.nvidia_empty_desc`
   - `ai_providers.openai_filtered_empty_title` → `ai_providers.nvidia_filtered_empty_title`
   - `ai_providers.openai_filtered_empty_desc` → `ai_providers.nvidia_filtered_empty_desc`
5. Replace the OpenAI light/dark icons with the NVIDIA icon:
   ```ts
   import iconNvidia from '@/assets/icons/nvidia.svg';
   ```
   Use `iconNvidia` wherever the OpenAI icon is used.
6. In the `EmptyState` rendering, update the title/description keys.

- [ ] **Step 9.2: Export the component**

Create `webui/src/components/providers/NvidiaSection/index.ts`:

```ts
export { NvidiaSection } from './NvidiaSection';
```

- [ ] **Step 9.3: Register the export**

Modify `webui/src/components/providers/index.ts` to add:

```ts
export { NvidiaSection } from './NvidiaSection';
```

---

## Task 10: Create the NVIDIA edit layout

**Files:**
- Create: `webui/src/pages/AiProvidersNvidiaEditLayout.tsx`

- [ ] **Step 10.1: Copy and adapt the OpenAI edit layout**

Copy `webui/src/pages/AiProvidersOpenAIEditLayout.tsx` to `AiProvidersNvidiaEditLayout.tsx` and apply these replacements:

1. Rename component to `AiProvidersNvidiaEditLayout`.
2. Change `OpenAIEditOutletContext` to `NvidiaEditOutletContext`.
3. Update draft keys from `openai:...` to `nvidia:...`:
   - `openai:invalid:...` → `nvidia:invalid:...`
   - `openai:new` → `nvidia:new`
   - `openai:${editIndex}` → `nvidia:${editIndex}`
4. Change `editorRootPath` to `/ai-providers/nvidia/...`.
5. Change the `handleBack` navigation to `/ai-providers` (same as OpenAI).
6. Keep the same form shape and store usage (`useOpenAIEditDraftStore`).
7. In the save payload, keep `name`, `baseUrl`, etc.
8. After successful save, call `navigate('/ai-providers', { replace: true })`.

Key excerpt from the adapted layout (representative):

```ts
const draftKey = useMemo(() => {
  if (invalidIndexParam) return `nvidia:invalid:${params.index ?? 'unknown'}`;
  if (editIndex === null) return 'nvidia:new';
  return `nvidia:${editIndex}`;
}, [editIndex, invalidIndexParam, params.index]);
```

---

## Task 11: Create the NVIDIA edit page

**Files:**
- Create: `webui/src/pages/AiProvidersNvidiaEditPage.tsx`

- [ ] **Step 11.1: Copy and adapt the OpenAI edit page**

Copy `webui/src/pages/AiProvidersOpenAIEditPage.tsx` to `AiProvidersNvidiaEditPage.tsx` and apply these replacements:

1. Rename component to `AiProvidersNvidiaEditPage`.
2. Update `OpenAIEditOutletContext` import to `NvidiaEditOutletContext`.
3. Change title keys:
   - `ai_providers.openai_add_modal_title` → `ai_providers.nvidia_add_modal_title`
   - `ai_providers.openai_edit_modal_title` → `ai_providers.nvidia_edit_modal_title`
4. Change all label/placeholder/hint keys from `openai_` to `nvidia_`.
5. Change `notification.openai_provider_required` etc. to `notification.nvidia_provider_required` (add those notification keys too).
6. In the empty form builder used in the layout, ensure the default `baseUrl` is `https://integrate.api.nvidia.com/v1` and default `name` is `nvidia` when creating a new entry. This is done in the layout's new-entry branch.

- [ ] **Step 11.2: Add notification translation keys**

Add to `notification` section of each locale:

```json
"nvidia_provider_required": "Please fill in provider name and Base URL",
"nvidia_provider_added": "NVIDIA NIM provider added successfully",
"nvidia_provider_updated": "NVIDIA NIM provider updated successfully",
"nvidia_provider_deleted": "NVIDIA NIM provider deleted successfully"
```

---

## Task 12: Add NVIDIA routes

**Files:**
- Modify: `webui/src/router/MainRoutes.tsx`

- [ ] **Step 12.1: Import and add routes**

Add imports:

```ts
import { AiProvidersNvidiaEditLayout } from '@/pages/AiProvidersNvidiaEditLayout';
import { AiProvidersNvidiaEditPage } from '@/pages/AiProvidersNvidiaEditPage';
```

Add routes after the openai routes block:

```ts
{
  path: '/ai-providers/nvidia/new',
  element: <AiProvidersNvidiaEditLayout />,
  children: [
    { index: true, element: <AiProvidersNvidiaEditPage /> },
  ],
},
{
  path: '/ai-providers/nvidia/:index',
  element: <AiProvidersNvidiaEditLayout />,
  children: [
    { index: true, element: <AiProvidersNvidiaEditPage /> },
  ],
},
```

---

## Task 13: Add NVIDIA to the provider nav

**Files:**
- Modify: `webui/src/components/providers/ProviderNav/ProviderNav.tsx`

- [ ] **Step 13.1: Update ProviderId type and provider list**

Change:

```ts
export type ProviderId = 'gemini' | 'codex' | 'claude' | 'vertex' | 'ampcode' | 'openai' | 'nvidia';
```

Add import:

```ts
import iconNvidia from '@/assets/icons/nvidia.svg';
```

Add to `PROVIDERS` array after `openai`:

```ts
{ id: 'nvidia', label: 'NVIDIA', getIcon: () => iconNvidia },
```

Update `itemRefs.current` initializer to include `nvidia: null`.

---

## Task 14: Update the main providers page

**Files:**
- Modify: `webui/src/pages/AiProvidersPage.tsx`

- [ ] **Step 14.1: Import and filter NIM providers**

Add import:

```ts
import { NvidiaSection } from '@/components/providers/NvidiaSection';
```

Add state after `openaiProviders`:

```ts
const [nvidiaProviders, setNvidiaProviders] = useState<OpenAIProviderConfig[]>(
  () => (config?.openaiCompatibility || []).filter(isNimProvider)
);
```

Add helper:

```ts
const isNimProvider = (provider: OpenAIProviderConfig) => {
  const name = provider.name?.trim().toLowerCase();
  return name === 'nvidia' || name === 'nvidia-nim';
};
```

- [ ] **Step 14.2: Update loadConfigs**

In `loadConfigs`, after `openaiResult` handling:

```ts
const allOpenAI = openaiResult.status === 'fulfilled' ? openaiResult.value || [] : data?.openaiCompatibility || [];
setOpenaiProviders(allOpenAI.filter((p) => !isNimProvider(p)));
setNvidiaProviders(allOpenAI.filter(isNimProvider));
```

- [ ] **Step 14.3: Add config effect**

Add `nvidiaProviders` update effect:

```ts
useEffect(() => {
  const all = config?.openaiCompatibility || [];
  setOpenaiProviders(all.filter((p) => !isNimProvider(p)));
  setNvidiaProviders(all.filter(isNimProvider));
}, [config?.openaiCompatibility]);
```

- [ ] **Step 14.4: Add delete handler and section render**

Add `deleteNvidia` handler mirroring `deleteOpenai` but using `nvidia` i18n keys.

Render the section before the OpenAI section:

```tsx
<div id="provider-nvidia">
  <NvidiaSection
    configs={nvidiaProviders}
    keyStats={keyStats}
    usageDetailsBySource={usageDetailsBySource}
    usageDetailsByAuthIndex={usageDetailsByAuthIndex}
    loading={loading}
    disableControls={disableControls}
    isSwitching={isSwitching}
    resolvedTheme={resolvedTheme}
    onAdd={() => openEditor('/ai-providers/nvidia/new')}
    onEdit={(index) => openEditor(`/ai-providers/nvidia/${index}`)}
    onDelete={deleteNvidia}
  />
</div>
```

---

## Task 15: Add commented config examples

**Files:**
- Modify: `config.example.yaml`
- Modify: `config.yaml`

- [ ] **Step 15.1: Insert NIM example block**

After the existing `openai-compatibility` example block in both files, add:

```yaml
# NVIDIA NIM providers (OpenAI-compatible entries with name "nvidia" or "nvidia-nim")
# Uses a dedicated NVIDIA NIM executor with tool-schema sanitization, retry downgrades, and NIM-specific request shaping.
# The default base URL is https://integrate.api.nvidia.com/v1 if omitted.
# openai-compatibility:
#   - name: "nvidia"
#     disabled: false
#     prefix: "test"
#     base-url: "https://integrate.api.nvidia.com/v1"
#     disable-cooling: false
#     headers:
#       X-Custom-Header: "custom-value"
#     api-key-entries:
#       - api-key: "nvapi-****************"
#         proxy-url: "socks5://proxy.example.com:1080"
#     models:
#       - name: "nvidia/llama-3.3-nemotron-super-49b-v1"
#         alias: "nemotron-super-49b"
#       - name: "nvidia/llama-3.1-nemotron-nano-8b-v1"
#         alias: "nemotron-nano-8b"
#       - name: "deepseek-ai/deepseek-r1"
#         alias: "deepseek-r1-nim"
```

---

## Task 16: Final verification

- [ ] **Step 16.1: Format Go**

```bash
gofmt -w .
```

- [ ] **Step 16.2: Build and test Go**

```bash
go build -o cli-proxy-api ./cmd/server
rm -f cli-proxy-api
go test ./...
```

Expected: build succeeds, tests pass.

- [ ] **Step 16.3: Type-check WebUI**

```bash
cd webui && npm run type-check
```

Expected: no TypeScript errors.

- [ ] **Step 16.4: Lint WebUI**

```bash
cd webui && npm run lint
```

Expected: no lint errors (or only pre-existing ones).

---

## Spec Coverage Checklist

| Spec Requirement | Task |
|---|---|
| Dedicated executor, not OpenAICompatExecutor | Tasks 1, 5 |
| Tool schema sanitization + aliases | Task 2 |
| Request options + extra_body | Task 3 |
| Retry downgrades on 400 | Task 4 |
| Default base URL | Task 1 (`nvidiaNimDefaultBaseURL`) |
| WebUI own section | Tasks 7, 8, 9, 14 |
| WebUI own edit page/routes | Tasks 10, 11, 12 |
| WebUI nav item | Task 13 |
| Service binding for `nvidia`/`nvidia-nim` | Task 6 |
| Config examples (commented) | Task 15 |
| No modification of OpenAICompatExecutor | All tasks (verified by not editing `openai_compat_executor.go`) |

## Placeholder Scan

- No `TBD`, `TODO`, or `implement later`.
- No vague "add error handling" steps; code includes explicit error paths.
- No undefined types/functions; all names are taken from the existing codebase.
