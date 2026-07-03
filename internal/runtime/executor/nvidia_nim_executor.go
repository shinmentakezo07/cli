package executor

import (
	"bufio"
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
	nvidiaNimDefaultBaseURL     = "https://integrate.api.nvidia.com/v1"
	nimToolArgumentAliasesKey   = "_fcc_nim_tool_argument_aliases"
	nimToolParameterAliasPrefix = "_fcc_arg_"
	defaultNimMaxTokens         = 8192
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

// ---------------------------------------------------------------------------
// Tool schema sanitization (Task 2)
// ---------------------------------------------------------------------------

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
				sanitizedParameters, ok := sanitizeNimSchemaNode(parameters).(map[string]any)
				if !ok {
					sanitizedParameters = make(map[string]any)
				}
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
	aliasedMap, ok := aliased.(map[string]any)
	if !ok {
		return parameters, nil
	}
	return aliasedMap, aliasToOriginal
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
	needsStrip := false
	if _, ok := body[nimToolArgumentAliasesKey]; ok {
		needsStrip = true
	}
	if _, ok := body["extra_body"].(map[string]any); ok {
		needsStrip = true
	}
	if !needsStrip {
		return body
	}
	upstream := shallowCopyMap(body)
	if extraBody, ok := upstream["extra_body"].(map[string]any); ok {
		// The OpenAI Python SDK merges extra_body into the top-level request
		// body before sending; it is not a literal JSON field. Flatten it
		// here so NVIDIA NIM receives the fields at the top level. Existing
		// top-level keys take precedence (matching SDK semantics).
		for k, v := range extraBody {
			if _, exists := upstream[k]; !exists {
				upstream[k] = v
			}
		}
		delete(upstream, "extra_body")
	}
	delete(upstream, nimToolArgumentAliasesKey)
	return upstream
}

// ---------------------------------------------------------------------------
// Request options (Task 3)
// ---------------------------------------------------------------------------

// sanitizeNimThinking coerces the top-level "thinking" field into the only two
// shapes NVIDIA NIM accepts: {"type":"enabled"} or {"type":"disabled"}. Any other
// thinking.type value (e.g. "adaptive", "auto", "budget") sent by clients would be
// rejected by NIM with "thinking.type must be enabled or disabled". NIM-specific
// reasoning budget lives in chat_template_kwargs.reasoning_budget, so budget_tokens
// is stripped from the thinking object.
func sanitizeNimThinking(body map[string]any, thinkingEnabled bool) {
	raw, hasThinking := body["thinking"]
	if !hasThinking {
		// No thinking field in the request; nothing to coerce. NIM-specific
		// enable/disable is driven entirely by chat_template_kwargs.
		return
	}

	desiredType := "disabled"
	if thinkingEnabled {
		desiredType = "enabled"
	}

	if existing, ok := raw.(map[string]any); ok {
		// Preserve the object shape but force the type + drop unsupported siblings.
		existing["type"] = desiredType
		delete(existing, "budget_tokens")
		delete(existing, "reasoning_budget")
		body["thinking"] = existing
		return
	}

	// Non-object thinking value: replace with the canonical NIM shape.
	body["thinking"] = map[string]any{"type": desiredType}
}

func applyNimRequestOptions(body map[string]any, thinkingEnabled bool) {
	sanitizeNimToolSchemas(body)
	sanitizeNimThinking(body, thinkingEnabled)

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

	// Always inject the NIM baseline chat_template_kwargs. By default NVIDIA NIM
	// providers run with enable_thinking=true and clear_thinking=false so reasoning
	// content is produced and retained for every request, regardless of whether the
	// caller passed an explicit thinking suffix. When thinking is additionally
	// enabled via a model suffix, the "thinking" flag and reasoning_budget are
	// layered on top.
	ctk, ok := extraBody["chat_template_kwargs"].(map[string]any)
	if !ok {
		ctk = make(map[string]any)
		extraBody["chat_template_kwargs"] = ctk
	}
	ctk["enable_thinking"] = true
	ctk["clear_thinking"] = false
	if thinkingEnabled {
		ctk["thinking"] = true
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

// ---------------------------------------------------------------------------
// Retry downgrade helpers (Task 4)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Execute (Task 5.1/5.2)
// ---------------------------------------------------------------------------

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

	thinkingEnabled := isThinkingEnabled(req.Model)
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

func isThinkingEnabled(model string) bool {
	suffix := thinking.ParseSuffix(model)
	if !suffix.HasSuffix {
		return false
	}
	raw := strings.ToLower(strings.TrimSpace(suffix.RawSuffix))
	if raw == "none" || raw == "0" || raw == "" {
		return false
	}
	return true
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
	if toolAliases := nimToolArgumentAliasesFromBody(bodyMap); toolAliases != nil {
		out = restoreNimToolAliasesNonStream(out, toolAliases)
	}
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

// ---------------------------------------------------------------------------
// ExecuteStream (Task 5.3)
// ---------------------------------------------------------------------------

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

	thinkingEnabled := isThinkingEnabled(req.Model)
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

// ---------------------------------------------------------------------------
// CountTokens (Task 5.4)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Image delegation wrappers (Task 5.5)
// ---------------------------------------------------------------------------

func (e *NvidiaNimExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	compat := &OpenAICompatExecutor{provider: e.provider, cfg: e.cfg}
	return compat.executeImages(ctx, auth, req, opts, endpointPath)
}

func (e *NvidiaNimExecutor) executeImagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	compat := &OpenAICompatExecutor{provider: e.provider, cfg: e.cfg}
	return compat.executeImagesStream(ctx, auth, req, opts, endpointPath)
}
