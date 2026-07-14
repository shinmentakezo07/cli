// Package streamrecovery provides mid-stream recovery for OpenAI-compatible
// upstream providers, ported from the Python cc/core/anthropic/streaming/recovery.py
// and cc/providers/transports/openai_chat/recovery.py modules.
//
// Recovery operates in three stages, mirroring the Python implementation:
//
//  1. Holdback buffer: briefly delays downstream SSE delivery after the first
//     chunk so an early stream cutoff can be retried transparently.
//  2. Early retry: when the stream fails before any output is committed, reset
//     state and restart the stream with the same body.
//  3. Mid-stream recovery: when the stream fails after partial output has been
//     emitted, salvage the partial text/thinking via a continuation request or
//     repair partial tool-call JSON by asking upstream to complete the suffix.
//
// A stall-detecting reader (StallReader) wraps the upstream HTTP response body
// and raises ErrStalledProviderStream when no chunk arrives within the configured
// stall timeout. A truncated stream (no terminal marker) raises
// ErrTruncatedProviderStream. Both are classified as retryable by
// IsRetryableStreamError.
//
// The package is transport-agnostic; callers feed raw upstream bytes through a
// StateTracker that mirrors Python's AnthropicStreamLedger for the subset of
// state needed by recovery (accumulated text, thinking, and per-tool JSON
// arguments), then ask the RecoveryController to classify failures and the
// BodyBuilder to construct recovery request bodies.
package streamrecovery
