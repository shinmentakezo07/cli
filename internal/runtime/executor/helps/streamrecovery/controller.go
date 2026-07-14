package streamrecovery

import (
	"errors"
	"time"
)

const (
	// EarlyTransparentTotalAttempts is the total number of stream attempts in
	// the early-transparent phase (1 initial + retries). Mirrors
	// EARLY_TRANSPARENT_TOTAL_ATTEMPTS in recovery.py.
	EarlyTransparentTotalAttempts = 5
	// EarlyTransparentMaxRetries is the number of transparent retries allowed
	// before the early phase gives up.
	EarlyTransparentMaxRetries = EarlyTransparentTotalAttempts - 1
	// MidstreamRecoveryAttempts is the number of mid-stream recovery attempts
	// allowed for continuation/tool-repair. Mirrors
	// MIDSTREAM_RECOVERY_ATTEMPTS in recovery.py.
	MidstreamRecoveryAttempts = 5
)

// RecoveryAction is how the stream lifecycle should respond to an upstream
// failure. Mirrors RecoveryFailureAction in recovery.py.
type RecoveryAction int

const (
	ActionEarlyRetry RecoveryAction = iota
	ActionMidstreamRecovery
	ActionFinalError
)

// String returns a human-readable name for the action.
func (a RecoveryAction) String() string {
	switch a {
	case ActionEarlyRetry:
		return "early_retry"
	case ActionMidstreamRecovery:
		return "midstream_recovery"
	default:
		return "final_error"
	}
}

// RecoveryDecision is the classification result for one stream exception. Mirrors
// RecoveryDecision in recovery.py.
type RecoveryDecision struct {
	Action                   RecoveryAction
	Retryable                bool
	Committed                bool
	HasBuffered              bool
	EarlyRetryAttempt        int
	MidstreamRecoveryAttempt int
}

// RecoveryController owns the holdback buffer and failure classification for one
// stream lifecycle. Mirrors RecoveryController in recovery.py.
type RecoveryController struct {
	providerName           string
	holdback               *HoldbackBuffer
	earlyRetryCount        int
	midstreamRecoveryCount int
}

// NewRecoveryController creates a controller with a fresh holdback buffer.
func NewRecoveryController(providerName string, holdback *HoldbackBuffer) *RecoveryController {
	if holdback == nil {
		holdback = NewHoldbackBuffer(0, 0, nil)
	}
	return &RecoveryController{providerName: providerName, holdback: holdback}
}

// Push feeds an SSE event through the holdback buffer, returning events to
// emit now.
func (c *RecoveryController) Push(event string) []string {
	return c.holdback.Push(event)
}

// Flush returns all buffered events and commits the buffer.
func (c *RecoveryController) Flush() []string {
	return c.holdback.Flush()
}

// FlushUncommitted returns buffered events when the decision is uncommitted and
// has buffered output, matching flush_uncommitted in recovery.py.
func (c *RecoveryController) FlushUncommitted(decision RecoveryDecision) []string {
	if !decision.Committed && decision.HasBuffered {
		return c.holdback.Flush()
	}
	return nil
}

// Discard drops buffered events without emitting. Used by early retry.
func (c *RecoveryController) Discard() {
	c.holdback.Discard()
}

// IsCommitted reports whether the holdback window has passed.
func (c *RecoveryController) IsCommitted() bool { return c.holdback.IsCommitted() }

// HasBuffered reports whether events are currently held.
func (c *RecoveryController) HasBuffered() bool { return c.holdback.HasBuffered() }

// EarlyRetries returns the number of transparent early retries taken so far.
func (c *RecoveryController) EarlyRetries() int { return c.earlyRetryCount }

// MidstreamRecoveries returns the number of mid-stream recovery attempts taken.
func (c *RecoveryController) MidstreamRecoveries() int { return c.midstreamRecoveryCount }

// ResetBuffer replaces the holdback buffer with a fresh one, preserving retry
// counters. Used by early retry before restarting the stream.
func (c *RecoveryController) ResetBuffer(now func() time.Time) {
	c.holdback.Discard()
	c.holdback = NewHoldbackBuffer(0, 0, now)
}

// AdvanceFailure classifies a stream exception into an action. Mirrors
// advance_failure in recovery.py.
//
//   - retryable + not committed + not complete-tool-salvageable + within early
//     retry budget  -> ActionEarlyRetry (transparent restart)
//   - retryable + generated output + within midstream budget -> ActionMidstreamRecovery
//   - otherwise -> ActionFinalError
func (c *RecoveryController) AdvanceFailure(err error, streamOpened, generatedOutput, completeToolSalvageable bool) RecoveryDecision {
	retryable := IsRetryableStreamError(err)
	committed := c.holdback.IsCommitted()
	hasBuffered := c.holdback.HasBuffered()

	if retryable && streamOpened && !committed && !completeToolSalvageable &&
		c.earlyRetryCount < EarlyTransparentMaxRetries {
		c.earlyRetryCount++
		c.holdback.Discard()
		c.holdback = NewHoldbackBuffer(0, 0, nil)
		return RecoveryDecision{
			Action:            ActionEarlyRetry,
			Retryable:         true,
			Committed:         false,
			HasBuffered:       hasBuffered,
			EarlyRetryAttempt: c.earlyRetryCount,
		}
	}

	if retryable && generatedOutput && c.midstreamRecoveryCount < MidstreamRecoveryAttempts {
		c.midstreamRecoveryCount++
		return RecoveryDecision{
			Action:                   ActionMidstreamRecovery,
			Retryable:                true,
			Committed:                committed,
			HasBuffered:              hasBuffered,
			MidstreamRecoveryAttempt: c.midstreamRecoveryCount,
		}
	}

	return RecoveryDecision{
		Action:      ActionFinalError,
		Retryable:   retryable,
		Committed:   committed,
		HasBuffered: hasBuffered,
	}
}

// IsRetryableNonStreamError is a helper used by callers before entering the
// stream loop to decide whether a non-stream request exception should retry.
func IsRetryableNonStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTruncatedProviderStream) || errors.Is(err, ErrStalledProviderStream) {
		return true
	}
	var se *HTTPStatusError
	if errors.As(err, &se) {
		code := se.Code
		return code == 429 || (code >= 500 && code <= 599)
	}
	return IsRetryableStreamError(err)
}
