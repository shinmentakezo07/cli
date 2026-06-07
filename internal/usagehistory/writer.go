package usagehistory

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

var (
	metricsRecordsWritten = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "cliproxy",
		Subsystem: "usage_history",
		Name:      "records_written_total",
		Help:      "Total usage records written to TimescaleDB.",
	})
	metricsFlushErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "cliproxy",
		Subsystem: "usage_history",
		Name:      "flush_errors_total",
		Help:      "Total batch flush errors to TimescaleDB.",
	})
	metricsRecordsDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "cliproxy",
		Subsystem: "usage_history",
		Name:      "records_dropped_total",
		Help:      "Total usage records dropped from the in-memory Postgres writer queue after JSONL persistence when the Postgres backlog cap is exceeded.",
	})
)

func init() {
	prometheus.MustRegister(metricsRecordsWritten, metricsFlushErrors, metricsRecordsDropped)
}

// BatchInserter is the minimal contract Writer needs from a backing store
// during a flush. Defined as an interface so tests can substitute an
// in-memory mock without pulling in a real Postgres. The concrete *PgStore
// satisfies this interface via its InsertBatch method.
type BatchInserter interface {
	InsertBatch(ctx context.Context, records []PgRecord) error
}

// Writer buffers usage records and flushes them to a PgStore in batches.
// It avoids producer-side drops under normal load: the buffer grows until the
// Postgres backlog cap is reached, and Write succeeds until the writer is
// closed. If Postgres remains unavailable past the cap, the writer drops the
// oldest Postgres backlog entries; the JSONL history sink has already persisted
// those records before they reach this writer.
type Writer struct {
	// store is the concrete *PgStore used by the management API for queries.
	// It is also the default BatchInserter; tests may override via SetInserter.
	store         *PgStore
	inserter      BatchInserter
	mu            sync.Mutex
	queue         []PgRecord
	closed        bool
	batchSize     int
	flushInterval time.Duration

	wakeCh chan struct{} // buffered(1); producers signal new records
	done   chan struct{}
	stopMu sync.Mutex
	wg     sync.WaitGroup
}

// NewWriter creates a Writer that buffers records and flushes to store.
// The buffer is logically unbounded — Write never blocks the caller and never
// reports failure. Memory grows only when the store cannot keep up; once the
// store drains, the buffer is compacted and the GC reclaims the slice.
//
// bufferSize is retained for backward compatibility with the previous
// bounded-channel implementation; it is no longer used as a hard cap. The
// flush batch size still governs when records are sent to the store.
func NewWriter(store *PgStore, bufferSize, batchSize int, flushInterval time.Duration) *Writer {
	w := &Writer{
		store:         store,
		inserter:      store,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		wakeCh:        make(chan struct{}, 1),
		done:          make(chan struct{}),
	}
	_ = bufferSize
	return w
}

// SetInserter overrides the BatchInserter used by flush. Intended for tests
// that want to substitute a mock without spinning up a real Postgres. Must
// be called before Start.
func (w *Writer) SetInserter(b BatchInserter) {
	if w == nil {
		return
	}
	if b == nil {
		w.inserter = w.store
		return
	}
	w.inserter = b
}

// Start launches the background flush goroutine.
func (w *Writer) Start(ctx context.Context) {
	w.wg.Add(1)
	go w.run(ctx)
}

// Write enqueues a record for async insertion. It never blocks the caller
// and never returns false before the writer is closed. The previous
// bool-returning contract permitted silent drops when the bounded channel
// filled; that behaviour is removed.
func (w *Writer) Write(r PgRecord) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	w.queue = append(w.queue, r)
	w.mu.Unlock()

	// Non-blocking wake-up signal. The run goroutine may already be busy
	// flushing, in which case the next tick or signal will catch this.
	select {
	case w.wakeCh <- struct{}{}:
	default:
	}
	return true
}

// Stop signals the writer to flush remaining records and shut down.
// Idempotent: subsequent calls are no-ops.
func (w *Writer) Stop() {
	if w == nil {
		return
	}
	w.stopMu.Lock()
	select {
	case <-w.done:
		// already stopped
		w.stopMu.Unlock()
		return
	default:
	}
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	close(w.done)
	w.stopMu.Unlock()
	w.wg.Wait()
}

// drainLocked moves up to batchSize records from queue into batch. The queue
// is compacted (set to nil) when fully drained so the underlying array can
// be GC'd.
func (w *Writer) drainLocked(batch []PgRecord) []PgRecord {
	for len(w.queue) > 0 && len(batch) < w.batchSize {
		batch = append(batch, w.queue[0])
		w.queue = w.queue[1:]
	}
	if len(w.queue) == 0 && cap(w.queue) > 64 {
		w.queue = nil
	}
	return batch
}

// drainAll empties the queue into the batch regardless of batch size, then
// returns. Used during shutdown to make sure every record is flushed.
func (w *Writer) drainAll(batch []PgRecord) []PgRecord {
	for len(w.queue) > 0 {
		batch = append(batch, w.queue[0])
		w.queue = w.queue[1:]
	}
	if len(w.queue) == 0 && cap(w.queue) > 64 {
		w.queue = nil
	}
	return batch
}

func (w *Writer) moveIntoBatch(batch []PgRecord) []PgRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.drainLocked(batch)
}

func (w *Writer) flush(ctx context.Context, batch []PgRecord) bool {
	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	inserter := w.inserter
	if inserter == nil {
		inserter = w.store
	}
	if err := inserter.InsertBatch(flushCtx, batch); err != nil {
		log.WithError(err).WithField("count", len(batch)).Error("usagehistory: batch flush failed")
		metricsFlushErrors.Inc()
		return false
	}
	metricsRecordsWritten.Add(float64(len(batch)))
	return true
}

func (w *Writer) flushFinal(batch []PgRecord, reason string) {
	if len(batch) == 0 {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		if w.flush(context.Background(), batch) {
			return
		}
		if attempt < 2 {
			delay := time.Duration(attempt+1) * 100 * time.Millisecond
			log.WithField("count", len(batch)).WithField("reason", reason).WithField("retry_in", delay).Warn("usagehistory: retrying final flush")
			time.Sleep(delay)
		}
	}
	MarkPgDegraded()
	log.WithField("count", len(batch)).WithField("reason", reason).Error("usagehistory: failed final postgres flush after retries; marked postgres history degraded")
}

func (w *Writer) run(ctx context.Context) {
	defer w.wg.Done()

	batch := make([]PgRecord, 0, w.batchSize)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			// Drain every remaining record and flush. Final flush retries until
			// success to preserve the writer's no-drop contract.
			w.mu.Lock()
			batch = w.drainAll(batch)
			w.mu.Unlock()
			w.flushFinal(batch, "shutdown")
			return

		case <-ticker.C:
			batch = w.moveIntoBatch(batch)
			if len(batch) > 0 && w.flush(ctx, batch) {
				batch = batch[:0]
			}

		case <-ctx.Done():
			w.mu.Lock()
			batch = w.drainAll(batch)
			w.mu.Unlock()
			w.flushFinal(batch, "context-cancel")
			return

		case <-w.wakeCh:
			// Move as many records as we can into the batch and flush
			// repeatedly until the queue is empty or the batch is full.
			for {
				batch = w.moveIntoBatch(batch)
				if len(batch) < w.batchSize {
					break
				}
				if !w.flush(ctx, batch) {
					break
				}
				batch = batch[:0]
			}
			// If we accumulated a partial batch, flush it on the next
			// ticker tick to avoid latency.
		}
	}
}
