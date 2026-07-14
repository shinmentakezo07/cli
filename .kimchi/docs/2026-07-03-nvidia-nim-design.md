# NVIDIA NIM Dedicated Executor Design

## Goal
Port the Python `cc/providers/nvidia_nim/` provider into the Go proxy as a dedicated executor and first-class WebUI provider section, without modifying `OpenAICompatExecutor` or reusing the old `nvi` compatibility entry.

## Scope
- Go executor: `internal/runtime/executor/nvidia_nim_executor.go`
- WebUI: dedicated section, edit page, routes, nav item
- Service binding: `sdk/cliproxy/service.go`
- Config examples: `config.example.yaml` and `config.yaml`
- Tests: unit tests for tool schema sanitization and retry downgrade helpers

## Design Decision: Dedicated Executor Reusing Package Helpers

### Options Considered
1. **Wrap `OpenAICompatExecutor`**: Delegate request/response handling to the compat executor and inject NIM transforms from the outside. Rejected because retry-downgrades and private alias stripping need access to the upstream request body and error body, which are internal to the compat executor.
2. **Refactor `OpenAICompatExecutor` with hooks**: Add pre-request and retry hooks to the compat executor. Rejected because the requirement explicitly forbids modifying `OpenAICompatExecutor`.
3. **Dedicated `NvidiaNimExecutor` reusing helpers** (selected): Create a new executor in the same package, reuse shared helpers for HTTP, usage, thinking, and translation, and implement all NIM-specific logic locally. This matches the existing per-provider executor pattern and keeps both providers isolated.

## Go Executor Behavior

### Identifier
Returns the bound provider key: `nvidia` or `nvidia-nim`.

### Default Base URL
`https://integrate.api.nvidia.com/v1`

### Tool Schema Sanitization
Ported from `cc/providers/nvidia_nim/tool_schema.py`:
- Walk each tool's `function.parameters` schema.
- Remove boolean subschemas under these keys:
  - single-value keys: `additionalProperties`, `additionalItems`, `unevaluatedProperties`, `unevaluatedItems`, `items`, `contains`, `propertyNames`, `if`, `then`, `else`, `not`
  - list keys: `allOf`, `anyOf`, `oneOf`, `prefixItems`
  - map keys: `properties`, `patternProperties`, `$defs`, `definitions`, `dependentSchemas`
- Alias reserved tool parameter names. The only reserved name to alias in the current Python code is `type` → `_fcc_arg_type`.
- Store the alias map in the private body key `_fcc_nim_tool_argument_aliases`.
- Strip `_fcc_nim_tool_argument_aliases` from the request body immediately before upstream I/O.

### Request Options
Ported from `cc/providers/nvidia_nim/request_options.py` and `config/nim.py` defaults:
- Request-level defaults:
  - `temperature`: 1.0
  - `top_p`: 1.0
  - `top_k`: -1
  - `max_tokens`: 8192 (matches `ANTHROPIC_DEFAULT_MAX_OUTPUT_TOKENS` constant)
  - `presence_penalty`: 0.0
  - `frequency_penalty`: 0.0
  - `min_p`: 0.0
  - `repetition_penalty`: 1.0
  - `seed`: nil
  - `stop`: nil
  - `parallel_tool_calls`: true
  - `ignore_eos`: false
  - `min_tokens`: 0
  - `chat_template`: nil
  - `request_id`: nil
- Apply the existing request `max_tokens` when present; otherwise fall back to the default.
- Build and inject `extra_body` with:
  - `chat_template_kwargs`: `{ "thinking": true, "enable_thinking": true }` when thinking is enabled, plus `reasoning_budget` set to `max_tokens`.
  - `top_k`, `min_p`, `repetition_penalty`, `min_tokens`, `chat_template`, `request_id`, `ignore_eos` when non-default.
- Set `parallel_tool_calls` on the body directly.

### Retry Downgrades
Ported from `cc/providers/nvidia_nim/retry.py`:
- After a non-streaming or streaming 400 error, inspect the error text/body (lowercased).
- If it contains `reasoning_budget`, `chat_template`, or `reasoning_content`, retry exactly once with a cloned body that strips the offending fields:
  - `reasoning_budget`: remove from `extra_body` and from `extra_body.chat_template_kwargs`.
  - `chat_template`: remove from `extra_body`.
  - `reasoning_content`: remove from all messages in the `messages` array.
- If the strip function finds nothing to remove, do not retry.

### Streaming and Non-Streaming Execution
- Support both `/chat/completions` and `/responses/compact` endpoints, same as `OpenAICompatExecutor`.
- Apply NIM transforms after `sdktranslator.TranslateRequest` and `thinking.ApplyThinking`.
- Usage reporting follows the same `helps.NewUsageReporter` pattern.
- Image endpoints are delegated through the existing openai-compat image helpers unchanged.

## WebUI Design

### Provider Navigation
Add `nvidia` to `ProviderNav` with a dedicated icon.

### Routes
- `/ai-providers/nvidia/new`
- `/ai-providers/nvidia/:index`

### Section Component
Create `NvidiaSection` (`webui/src/components/providers/NvidiaSection/`):
- Fetch the full `openai-compatibility` list from the existing `/openai-compatibility` endpoint.
- Filter entries whose `name` equals `nvidia` or `nvidia-nim`.
- Render them separately from `OpenAISection`.
- Provide add/edit/delete actions that navigate to the NVIDIA routes.

### Edit Pages
Create `AiProvidersNvidiaEditLayout` and `AiProvidersNvidiaEditPage` modeled on the OpenAI edit flow:
- Reuse the same form state shape (`OpenAIFormState`) and draft store pattern.
- Default base URL is `https://integrate.api.nvidia.com/v1`.
- Labels and page titles are NVIDIA-specific.
- Persist through the existing `/openai-compatibility` endpoints.

### Main Providers Page
Update `AiProvidersPage` to load/filter NIM entries and render `NvidiaSection`.

## Service Binding
In `sdk/cliproxy/service.go`, update `ensureExecutorsForAuthWithMode`:
- After `openAICompatInfoFromAuth`, check whether the resolved provider key is `nvidia` or `nvidia-nim`.
- If so, register `executor.NewNvidiaNimExecutor(s.cfg)` and return early.
- Otherwise continue with the existing openai-compatibility or provider-switch logic.

Also register a baseline `NvidiaNimExecutor` in `registerHomeExecutors` so home-dispatched NIM auth entries work without local credentials.

## Config Examples
Add commented-out NVIDIA NIM example blocks to:
- `config.example.yaml`
- `config.yaml`

Example block:
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
```

## Testing
- `go build ./...` must pass.
- `go test ./...` must pass.
- New unit tests for:
  - Tool schema boolean subschema removal.
  - Reserved parameter aliasing and alias stripping.
  - Retry downgrade body cloning for `reasoning_budget`, `chat_template`, and `reasoning_content`.

## Files to Create or Modify
### New files
- `internal/runtime/executor/nvidia_nim_executor.go`
- `internal/runtime/executor/nvidia_nim_executor_test.go`
- `webui/src/components/providers/NvidiaSection/NvidiaSection.tsx`
- `webui/src/components/providers/NvidiaSection/index.ts`
- `webui/src/pages/AiProvidersNvidiaEditLayout.tsx`
- `webui/src/pages/AiProvidersNvidiaEditPage.tsx`

### Modified files
- `sdk/cliproxy/service.go`
- `webui/src/router/MainRoutes.tsx`
- `webui/src/components/providers/ProviderNav/ProviderNav.tsx`
- `webui/src/components/providers/index.ts`
- `webui/src/pages/AiProvidersPage.tsx`
- `config.example.yaml`
- `config.yaml`

## Out of Scope
- NVIDIA NIM / Riva voice transcription (`voice.py`) is not ported; it depends on the `riva` Python gRPC client.
- No new backend management endpoints; storage remains `openai-compatibility`.
- No changes to `OpenAICompatExecutor`.
