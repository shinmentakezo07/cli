// This file wires the streamrecovery package into NvidiaNimExecutor streaming.
// It is kept in a separate file from nvidia_nim_executor.go so the main
// executor file stays focused on the request/response translation pipeline
// while this file owns the recovery-augmented stream loop.
//
// The integration ports three of the Python cc recovery behaviors:
//  1. Holdback buffer (briefly delays downstream SSE so early cutoff is invisible)
//  2. Early transparent retry on stall / truncation / retryable transport error
//  3. Truncation recovery (synthetic [DONE] when the upstream closes without one)
//
// Mid-stream text/tool salvage (the third Python phase) requires the recovery
// package to own downstream Anthropic content-block state. The Go executor
// delegates block state to sdktranslator.TranslateStream via the opaque `param`
// pointer; reconstructing salvaged text/thinking deltas into the same blocks
// the translator already opened is out of scope here. The plumbing is in
// place: the StateTracker accumulates upstream state and the
// RecoveryController classifies failures, so a future ledger-integrated
// emitter can complete the loop.
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
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/streamrecovery"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// nimStallTimeout returns the per-chunk stall timeout for the NIM recovery
// loop. A non-positive cfg value disables the watchdog (mirrors the
// stream_stall_timeout=0 default in the Python config).
func (e *NvidiaNimExecutor) nimStallTimeout() time.Duration {
	if e.cfg == nil {
		return 0
	}
	if v := e.cfg.Streaming.KeepAliveSeconds; v > 0 {
		return time.Duration(v) * time.Second
	}
	// Default to 60s to match the Python default (60s) so a stalled NIM
	// upstream is rescued within a minute rather than hanging indefinitely.
	return 60 * time.Second
}

// sendNimStreamWithRecovery is the recovery-augmented drop-in replacement for
// the legacy sendNimStream goroutine body. It opens an upstream stream, parses
// OpenAI chat completion chunks (via the StateTracker), translates them to
// downstream Anthropic SSE (via the SDK translator), and routes the SSE bytes
// through a holdback-buffered RecoveryController so an early stream cutoff can
// be retried transparently before any bytes reach the client.
//
// On a retryable failure before the holdback commits, the loop transparently
// restarts the stream with the same body. On a failure after the holdback
// commits, it flushes whatever downstream events have already been emitted,
// then surfacing the error as Anthropic SSE so the client gets a clean
// message_stop instead of a hung stream.
func (e *NvidiaNimExecutor) sendNimStreamWithRecovery(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	baseURL, apiKey string,
	bodyMap map[string]any,
	reporter *helps.UsageReporter,
	to, from sdktranslator.Format,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	originalBody := deepCopyMap(bodyMap)
	toolAliases := nimToolArgumentAliasesFromBody(bodyMap)
	out := make(chan cliproxyexecutor.StreamChunk)
	// Capture response headers from the first successful open for the
	// StreamResult, mirroring the legacy sendNimStream which returns
	// httpResp.Header.Clone().
	var respHeaders http.Header

	go func() {
		defer close(out)
		controller := streamrecovery.NewRecoveryController(e.Identifier(), streamrecovery.NewHoldbackBuffer(0, 0, nil))
		state := streamrecovery.NewStateTracker()
		restorer := newNimAliasRestorer(toolAliases)
		var param any
		translatedBody := mustMarshalOriginalBody(originalBody)

		for attempt := 0; ; attempt++ {
			if err := ctx.Err(); err != nil {
				return
			}
			stream, streamOpened, openErr := e.openRecoveryStream(ctx, auth, baseURL, apiKey, bodyMap)
			if openErr != nil {
				decision := controller.AdvanceFailure(openErr, streamOpened, state.GeneratedOutput(), false)
				if !e.handleRecoveryDecision(out, controller, state, decision, openErr, req, reporter, ctx, translatedBody, &param, originalBody) {
					return
				}
				continue
			}
			// Capture headers from the first successful open.
			if respHeaders == nil {
				if ha, ok := stream.(interface{ Headers() http.Header }); ok {
					respHeaders = ha.Headers()
				}
			}
			consumeErr := e.consumeRecoveryStream(ctx, stream, state, controller, restorer, out, &param, req, to, from, opts, translatedBody, reporter)
			_ = stream.Close()
			if consumeErr == nil && state.SawFinishReason() {
				// Happy path: upstream emitted a terminal finish_reason. Flush
				// the holdback buffer and synthesize the final data: [DONE].
				e.flushAndTerminate(out, controller, req, opts, to, from, &param, ctx, reporter)
				return
			}
			// Stream broke or ended without finish_reason. Classify and act.
			classifyErr := consumeErr
			if classifyErr == nil && !state.SawFinishReason() {
				classifyErr = streamrecovery.ErrTruncatedProviderStream
			}
			decision := controller.AdvanceFailure(classifyErr, true, state.GeneratedOutput(), false)
			if !e.handleRecoveryDecision(out, controller, state, decision, classifyErr, req, reporter, ctx, translatedBody, &param, originalBody) {
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: respHeaders, Chunks: out}, nil
}

// openRecoveryStream issues one upstream HTTP request and returns a
// streamrecovery.StreamReader wrapping the response body. streamOpened is true
// when the HTTP round-trip itself succeeded (so the controller can attribute
// DNS / connect failures separately from in-stream failures).
func (e *NvidiaNimExecutor) openRecoveryStream(ctx context.Context, auth *cliproxyauth.Auth, baseURL, apiKey string, bodyMap map[string]any) (stream streamrecovery.StreamReader, streamOpened bool, err error) {
	upstreamBodyMap := bodyWithoutNimToolArgumentAliases(bodyMap)
	translated, marshalErr := json.Marshal(upstreamBodyMap)
	if marshalErr != nil {
		return nil, false, fmt.Errorf("nvidia nim executor: marshal stream body: %w", marshalErr)
	}
	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if errReq != nil {
		return nil, false, errReq
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
		URL: url, Method: http.MethodPost, Headers: httpReq.Header.Clone(), Body: translated,
		Provider: e.Identifier(), AuthID: authID, AuthLabel: authLabel, AuthType: authType, AuthValue: authValue,
	})
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, false, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("nvidia nim stream request error, status: %d, message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		return nil, true, &streamrecovery.HTTPStatusError{Code: httpResp.StatusCode, Msg: string(b)}
	}
	return newStallReadingAdapter(ctx, httpResp.Body, e.nimStallTimeout(), httpResp.Header.Clone()), true, nil
}

// consumeRecoveryStream reads from one upstream stream. Each upstream SSE data
// line is:
//  1. Recorded via AppendAPIResponseChunk (for management API inspection)
//  2. Parsed for usage tokens via ParseOpenAIStreamUsage + reporter.Publish
//  3. Mirrored to the StateTracker as raw JSON (stripping "data: " prefix)
//     so the tracker can detect partial upstream output and finish_reason
//  4. Translated to downstream SSE via TranslateStream
//  5. Pushed through the holdback buffer IN REAL TIME (not batched after
//     ForEach returns) so the 750ms holdback window actually fires
//  6. Flushed events are emitted to out immediately
//
// Returns nil on a clean stream end (the caller checks for SawFinishReason()),
// or the error that broke the stream (stall / truncation / transport error).
//
// A sentinel *consumeAbort error is returned when the ctx is cancelled mid-stream;
// the caller treats it as a ctx.Err().
func (e *NvidiaNimExecutor) consumeRecoveryStream(
	ctx context.Context,
	stream streamrecovery.StreamReader,
	state *streamrecovery.StateTracker,
	controller *streamrecovery.RecoveryController,
	restorer *nimAliasRestorer,
	out chan<- cliproxyexecutor.StreamChunk,
	param *any,
	req cliproxyexecutor.Request,
	to, from sdktranslator.Format,
	opts cliproxyexecutor.Options,
	translatedBody []byte,
	reporter *helps.UsageReporter,
) error {
	var abortErr error
	err := stream.ForEach(ctx, e.nimStallTimeout(), func(line []byte) {
		if abortErr != nil {
			return
		}

		// 1) Record the raw upstream line for management API inspection.
		helps.AppendAPIResponseChunk(ctx, e.cfg, line)

		// 2) Parse usage tokens from the upstream line.
		if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
			reporter.Publish(ctx, detail)
		}

		// 3) Mirror raw JSON (without "data: " prefix) to the StateTracker
		//    so it can track text/thinking/tool-arg accumulation and
		//    finish_reason. The tracker expects raw JSON starting with '{'.
		rawJSON := stripDataPrefix(line)
		state.IngestOpenAIChunk(rawJSON)

		// 4) Translate the upstream chunk to downstream SSE.
		chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translatedBody, bytes.Clone(line), param)
		// 4a) Restore aliased tool argument keys (e.g. _fcc_arg_type -> type)
		//     in translated Anthropic SSE so downstream agents see the original
		//     tool parameter names.
		if restorer != nil {
			for j := range chunks {
				chunks[j] = restorer.restoreEventBytes(chunks[j])
			}
		}

		// 5) Push each translated chunk through the holdback IN REAL TIME
		//    so the 750ms holdback window fires correctly. Once the
		//    holdback is committed, events flow directly to out.
		for _, ev := range chunks {
			if abortErr != nil {
				return
			}
			if controller.IsCommitted() {
				if !emitBytes(out, ev, ctx) {
					abortErr = ctx.Err()
					return
				}
				continue
			}
			for _, flushed := range controller.Push(string(ev)) {
				if !emitString(out, flushed, ctx) {
					abortErr = ctx.Err()
					return
				}
			}
		}
	})
	if abortErr != nil {
		return abortErr
	}
	return err
}

// handleRecoveryDecision dispatches one RecoveryDecision. Returns true to let
// the caller continue looping (early retry); false to terminate.
func (e *NvidiaNimExecutor) handleRecoveryDecision(
	out chan<- cliproxyexecutor.StreamChunk,
	controller *streamrecovery.RecoveryController,
	state *streamrecovery.StateTracker,
	decision streamrecovery.RecoveryDecision,
	cause error,
	req cliproxyexecutor.Request,
	reporter *helps.UsageReporter,
	ctx context.Context,
	translatedBody []byte,
	param *any,
	originalBody map[string]any,
) bool {
	switch decision.Action {
	case streamrecovery.ActionEarlyRetry:
		log.WithError(cause).WithField("attempt", decision.EarlyRetryAttempt).
			Warn("nvidia nim executor: early transparent retry after upstream stream failure")
		// Discard any pending downstream events buffered in the holdback since
		// we will replay this stream from scratch.
		controller.Discard()
		state.Reset()
		reporter.PublishFailure(ctx, cause)
		return true
	default: // ActionMidstreamRecovery / ActionFinalError (mid-stream salvage not wired yet)
		// Flush any buffered events so downstream sees them before the error.
		for _, ev := range controller.Flush() {
			if !emitString(out, ev, ctx) {
				return false
			}
		}
		// Emit a clean Anthropic error + message_stop so the client doesn't
		// hang waiting for a terminator that never comes.
		_ = emitString(out, anthropicErrorEvent(cause), ctx)
		_ = emitString(out, anthropicMessageStop(), ctx)
		reporter.PublishFailure(ctx, cause)
		return false
	}
}

// flushAndTerminate flushes the holdback buffer and emits the synthetic
// data: [DONE] synthesis that the SDK translator uses to emit the final
// message_stop / usage event.
func (e *NvidiaNimExecutor) flushAndTerminate(
	out chan<- cliproxyexecutor.StreamChunk,
	controller *streamrecovery.RecoveryController,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	to, from sdktranslator.Format,
	param *any,
	ctx context.Context,
	reporter *helps.UsageReporter,
) {
	for _, ev := range controller.Flush() {
		if !emitString(out, ev, ctx) {
			return
		}
	}
	terminal := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, nil, []byte("data: [DONE]"), param)
	for i := range terminal {
		if !emitBytes(out, terminal[i], ctx) {
			return
		}
	}
	reporter.EnsurePublished(ctx)
}

// ---- small helpers ----

// stallReadingAdapter implements streamrecovery.StreamReader by wrapping a
// bufio.Scanner over a StallReader-protected upstream body. It also carries
// the HTTP response headers so the caller can expose them in the StreamResult.
type stallReadingAdapter struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	headers http.Header
}

func newStallReadingAdapter(ctx context.Context, body io.ReadCloser, stall time.Duration, headers http.Header) *stallReadingAdapter {
	reader := io.ReadCloser(body)
	if stall > 0 {
		reader = streamrecovery.NewStallReader(ctx, body, stall)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(nil, 52_428_800)
	return &stallReadingAdapter{body: body, scanner: scanner, headers: headers}
}

// Headers returns the HTTP response headers captured at stream-open time.
func (a *stallReadingAdapter) Headers() http.Header {
	return a.headers
}

// ForEach reads upstream SSE lines, extracting the data: payload for each
// chunk and invoking cb with the raw data line in canonical "data: <payload>\n"
// form so the SDK translator / ParseOpenAIStreamUsage / StateTracker all see
// a normal SSE frame. Returns nil on a clean [DONE] marker, or the stream
// error on failure.
func (a *stallReadingAdapter) ForEach(ctx context.Context, stall time.Duration, cb func(line []byte)) error {
	for a.scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := a.scanner.Bytes()
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			if bytes.HasPrefix(trimmed, []byte(":")) || bytes.HasPrefix(trimmed, []byte("event:")) ||
				bytes.HasPrefix(trimmed, []byte("id:")) || bytes.HasPrefix(trimmed, []byte("retry:")) {
				continue
			}
			if bytes.HasPrefix(trimmed, []byte("{")) || bytes.HasPrefix(trimmed, []byte("[")) {
				return &streamrecovery.HTTPStatusError{Code: http.StatusBadGateway, Msg: string(trimmed)}
			}
			continue
		}
		// Trim leading spaces after "data:" so the SDK translator sees a
		// clean frame. Keep one trailing newline so callers (the state
		// tracker + translator) both accept the line.
		data := bytes.TrimPrefix(trimmed, []byte("data:"))
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			// Caller is responsible for synthesizing the terminal frame via
			// TranslateStream("data: [DONE]"). Returning nil signals EOF.
			return nil
		}
		// Re-emit the line in the canonical "data: <payload>\n" form that
		// the SDK translator / ParseOpenAIStreamUsage expect.
		frame := make([]byte, 0, len(data)+8)
		frame = append(frame, []byte("data: ")...)
		frame = append(frame, data...)
		frame = append(frame, '\n')
		cb(frame)
	}
	if err := a.scanner.Err(); err != nil {
		return err
	}
	// Scanner ended without seeing [DONE]; surface truncation so the
	// controller's truncation path kicks in.
	return streamrecovery.ErrTruncatedProviderStream
}

func (a *stallReadingAdapter) Close() error {
	if a.body == nil {
		return nil
	}
	return a.body.Close()
}

// stripDataPrefix removes the "data: " SSE prefix and trailing whitespace,
// returning the raw JSON payload. If the line doesn't have the prefix, it's
// returned trimmed. This is what StateTracker.IngestOpenAIChunk expects.
func stripDataPrefix(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	return trimmed
}

// emitString sends one SSE string as a StreamChunk payload.
func emitString(out chan<- cliproxyexecutor.StreamChunk, ev string, ctx context.Context) bool {
	select {
	case out <- cliproxyexecutor.StreamChunk{Payload: []byte(ev)}:
		return true
	case <-ctx.Done():
		return false
	}
}

// emitBytes sends one SSE byte slice as a StreamChunk payload.
func emitBytes(out chan<- cliproxyexecutor.StreamChunk, ev []byte, ctx context.Context) bool {
	select {
	case out <- cliproxyexecutor.StreamChunk{Payload: ev}:
		return true
	case <-ctx.Done():
		return false
	}
}

// anthropicErrorEvent renders an Anthropic-format SSE error event for the
// final-error tail. Keeps the client from hanging when the upstream stream
// breaks irrecoverably.
func anthropicErrorEvent(err error) string {
	msg, _ := json.Marshal(err.Error())
	return "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":" + string(msg) + "}}\n\n"
}

// anthropicMessageStop renders the canonical Anthropic message_stop event so
// the client knows the message has ended.
func anthropicMessageStop() string {
	return "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// mustMarshalOriginalBody marshals the original request body for the recovery
// loop. On error (which indicates programmer error, since bodyMap was already
// JSON-marshalled once by the caller) we return nil so the loop degrades to no
// translation rather than crashing.
func mustMarshalOriginalBody(originalBody map[string]any) []byte {
	if originalBody == nil {
		return nil
	}
	out, err := json.Marshal(bodyWithoutNimToolArgumentAliases(originalBody))
	if err != nil {
		return nil
	}
	return out
}
