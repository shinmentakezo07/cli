package streamrecovery

import (
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrTruncatedProviderStream is returned when an upstream stream ends without a
// terminal marker (finish_reason or [DONE]).
var ErrTruncatedProviderStream = errors.New("provider stream ended without a terminal marker")

// ErrStalledProviderStream is returned when an upstream stream goes silent for
// longer than the configured stall timeout. It is a retryable error.
var ErrStalledProviderStream = errors.New("provider stream stalled: no chunk received within stall timeout")

// IsTruncatedStreamError reports whether err is a truncation or stall error.
func IsTruncatedStreamError(err error) bool {
	return errors.Is(err, ErrTruncatedProviderStream) || errors.Is(err, ErrStalledProviderStream)
}

// HTTPStatusError is an error that carries an HTTP status code, used to classify
// retryability of non-2xx upstream responses.
type HTTPStatusError struct {
	Code int
	Msg  string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.Msg == "" {
		return http.StatusText(e.Code)
	}
	return e.Msg
}

// StatusCode returns the HTTP status code.
func (e *HTTPStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Code
}

// IsRetryableStreamError reports whether a provider stream error can be retried
// or recovered. Mirrors cc/core/anthropic/streaming/recovery.py:is_retryable_stream_error.
func IsRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	if IsTruncatedStreamError(err) {
		return true
	}
	var se *HTTPStatusError
	if errors.As(err, &se) {
		code := se.Code
		return code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
	}
	// Network / IO errors are retryable.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// Treat generic connection / timeout errors as retryable. We avoid importing
	// net packages to keep the dependency surface small; the error message is a
	// best-effort heuristic for unwrapped transport errors.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") || strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "no such host") || strings.Contains(msg, "transport") {
		return true
	}
	return false
}

// IsNonRetryableStreamError reports whether an error is a definitive non-retryable
// client error (auth or bad request). Used by the early-retry path to bail out
// immediately instead of looping.
func IsNonRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var se *HTTPStatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest,
			http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusPaymentRequired:
			return true
		}
	}
	return false
}

// AsRetryableErrorOrSelf returns err unchanged. It exists to give callers a
// single place to swap in a different classification strategy later without
// rewriting every error path.
func AsRetryableErrorOrSelf(err error) error { return err }
