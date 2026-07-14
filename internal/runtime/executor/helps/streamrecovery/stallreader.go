package streamrecovery

import (
	"bytes"
	"context"
	"io"
	"time"
)

// StallReader wraps an io.ReadCloser and raises ErrStalledProviderStream when
// a Read call stalls longer than the per-chunk timeout after the first byte.
//
// Mirrors with_stall_timeout in recovery.py. The first Read has no timeout so
// slow model pre-fill does not trigger a false positive. After at least one
// byte has been seen, each subsequent Read must progress within the timeout,
// otherwise ErrStalledProviderStream is returned.
//
// Scanner-based callers should wrap this reader with bufio.Scanner directly.
type StallReader struct {
	rc      io.ReadCloser
	timeout time.Duration
	seenAny bool
	ctx     context.Context
}

// NewStallReader wraps rc with a stall watchdog. timeout <= 0 disables the
// watchdog (Read passes through). When ctx is cancelled, the read returns
// ctx.Err(); this lets upstream-aware callers (e.g. ExecuteStream) cancel
// early.
func NewStallReader(ctx context.Context, rc io.ReadCloser, timeout time.Duration) *StallReader {
	if ctx == nil {
		ctx = context.Background()
	}
	return &StallReader{rc: rc, timeout: timeout, ctx: ctx}
}

// Read implements io.Reader.
func (r *StallReader) Read(p []byte) (int, error) {
	if r.rc == nil {
		return 0, io.EOF
	}
	if r.timeout <= 0 || !r.seenAny {
		n, err := r.rc.Read(p)
		if n > 0 {
			r.seenAny = true
		}
		return n, err
	}
	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		n, err := r.rc.Read(p)
		ch <- readResult{n, err}
	}()
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-timer.C:
		// Best-effort close of the upstream body to unblock the goroutine.
		_ = r.rc.Close()
		return 0, ErrStalledProviderStream
	case <-r.ctx.Done():
		_ = r.rc.Close()
		return 0, r.ctx.Err()
	}
}

// Close implements io.Closer.
func (r *StallReader) Close() error {
	if r.rc == nil {
		return nil
	}
	return r.rc.Close()
}

// AcquireFirstChunk blocks until the reader returns either its first non-empty
// chunk or an error. Used after the initial Read to confirm the stream opened
// successfully before entering the per-chunk watchdog loop. Returns the first
// bytes collected (which may be empty when the upstream returned a chunk of
// length 0 followed by EOF / stall) and the error, if any.
//
// This is a convenience for scanner-style readers that want to skip the
// stall-free first Read and then loop with bufio.Scanner on a wrapped body.
func AcquireFirstChunk(r io.Reader, max int) ([]byte, error) {
	if max <= 0 {
		max = 64 * 1024
	}
	buf := make([]byte, max)
	n, err := r.Read(buf)
	if n > 0 {
		return bytes.Clone(buf[:n]), nil
	}
	return nil, err
}
