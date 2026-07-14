package streamrecovery

import (
	"sync"
	"time"
)

const (
	// EarlyHoldbackSeconds is how long the first chunk is held back before
	// being flushed downstream, to allow transparent early retry.
	// Mirrors
	// EARLY_HOLDBACK_SECONDS in recovery.py.
	earlyHoldbackSeconds = 750 * time.Millisecond
	// RecoveryBufferMaxBytes is the maximum total byte size of buffered SSE
	// events before the holdback auto-flushes. Mirrors
	// RECOVERY_BUFFER_MAX_BYTES in recovery.py.
	RecoveryBufferMaxBytes = 65_536
)

// HoldbackBuffer briefly holds downstream SSE events so an early stream cutoff
// can be retried invisibly. Mirrors RecoveryHoldbackBuffer in recovery.py.
//
// The first event starts a holdback window. Events accumulate until either the
// window expires (EarlyHoldbackSeconds) or the byte budget (RecoveryBufferMaxBytes)
// is exceeded, at which point Flush() returns all buffered events and the buffer
// becomes committed: subsequent Push() calls return events immediately.
type HoldbackBuffer struct {
	mu        sync.Mutex
	holdback  time.Duration
	maxBytes  int
	now       func() time.Time
	events    []string
	bytes     int
	startedAt time.Time
	Commit    bool
}

// NewHoldbackBuffer constructs a buffer with custom holdback duration and byte
// budget. now is injectable for tests; nil falls back to time.Now.
func NewHoldbackBuffer(holdback time.Duration, maxBytes int, now func() time.Time) *HoldbackBuffer {
	if holdback <= 0 {
		holdback = earlyHoldbackSeconds
	}
	if maxBytes <= 0 {
		maxBytes = RecoveryBufferMaxBytes
	}
	if now == nil {
		now = time.Now
	}
	return &HoldbackBuffer{holdback: holdback, maxBytes: maxBytes, now: now}
}

// Push appends an SSE event. Returns events to emit now:
//   - when committed, returns [event]
//   - otherwise buffers and returns events to flush when the window expires or
//     the byte budget is exceeded.
func (h *HoldbackBuffer) Push(event string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Commit {
		return []string{event}
	}
	if h.startedAt.IsZero() {
		h.startedAt = h.now()
	}
	h.events = append(h.events, event)
	h.bytes += len(event)
	if h.bytes >= h.maxBytes || h.now().Sub(h.startedAt) >= h.holdback {
		return h.flushLocked()
	}
	return nil
}

// Flush returns all buffered events and commits the buffer.
func (h *HoldbackBuffer) Flush() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flushLocked()
}

func (h *HoldbackBuffer) flushLocked() []string {
	if h.Commit {
		return nil
	}
	h.Commit = true
	out := h.events
	h.events = nil
	h.bytes = 0
	h.startedAt = time.Time{}
	return out
}

// Discard drops all buffered events without emitting them. Used by early retry
// before resetting the buffer.
func (h *HoldbackBuffer) Discard() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = nil
	h.bytes = 0
	h.startedAt = time.Time{}
}

// HasBuffered reports whether any events are currently buffered (not committed).
func (h *HoldbackBuffer) HasBuffered() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events) > 0
}

// IsCommitted reports whether the holdback window has already passed.
func (h *HoldbackBuffer) IsCommitted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Commit
}
