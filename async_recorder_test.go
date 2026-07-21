package recorder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAsyncRecorderDeliversFIFOAndDrainsOnClose(t *testing.T) {
	t.Parallel()

	sink := &collectingRecorder{}
	async := mustAsyncRecorder(t, sink, WithAsyncQueueCapacity(4))

	for i := range 10 {
		async.Record(asyncTestEntry(i))
	}

	closeAsyncRecorder(t, async)

	got := sink.indices()
	if len(got) != 10 {
		t.Fatalf("processed %d entries, want 10: %v", len(got), got)
	}

	for i, index := range got {
		if index != i {
			t.Fatalf("entry %d = %d, want %d; all=%v", i, index, i, got)
		}
	}

	stats := async.Stats()
	if stats.Accepted != 10 || stats.Processed != 10 || stats.Pending != 0 || stats.InFlight != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderDeliversFullBatchesInFIFOOrder(t *testing.T) {
	t.Parallel()

	sink := newBatchCollectingRecorder()
	async := mustAsyncRecorder(t, sink,
		WithAsyncQueueCapacity(8),
		WithAsyncBatchSize(3),
		WithAsyncFlushInterval(time.Second),
	)

	for i := range 6 {
		async.Record(asyncTestEntry(i))
	}

	waitSignal(t, sink.delivered, "two full batches")
	waitSignal(t, sink.delivered, "two full batches")
	closeAsyncRecorder(t, async)

	if got := sink.indices(); fmt.Sprint(got) != "[0 1 2 3 4 5]" {
		t.Fatalf("entries = %v", got)
	}

	if got := sink.batchSizes(); fmt.Sprint(got) != "[3 3]" {
		t.Fatalf("batch sizes = %v", got)
	}

	stats := async.Stats()
	if stats.Processed != 6 || stats.BatchesProcessed != 2 || stats.MaxBatchSize != 3 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderFlushesPartialBatchAfterInterval(t *testing.T) {
	t.Parallel()

	sink := newBatchCollectingRecorder()
	async := mustAsyncRecorder(t, sink,
		WithAsyncBatchSize(4),
		WithAsyncFlushInterval(20*time.Millisecond),
	)
	async.Record(asyncTestEntry(1))
	async.Record(asyncTestEntry(2))

	waitSignal(t, sink.delivered, "partial batch interval")
	closeAsyncRecorder(t, async)

	if got := sink.batchSizes(); fmt.Sprint(got) != "[2]" {
		t.Fatalf("batch sizes = %v", got)
	}
}

func TestAsyncRecorderCloseFlushesPartialBatchImmediately(t *testing.T) {
	t.Parallel()

	sink := newBatchCollectingRecorder()
	async := mustAsyncRecorder(t, sink,
		WithAsyncBatchSize(4),
		WithAsyncFlushInterval(time.Hour),
	)
	async.Record(asyncTestEntry(1))
	async.Record(asyncTestEntry(2))
	closeAsyncRecorder(t, async)

	if got := sink.batchSizes(); fmt.Sprint(got) != "[2]" {
		t.Fatalf("batch sizes = %v", got)
	}
}

func TestAsyncRecorderDefaultBlocksInsteadOfDropping(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink, WithAsyncQueueCapacity(1))
	async.Record(asyncTestEntry(1))
	waitSignal(t, sink.started, "first sink call")
	async.Record(asyncTestEntry(2))

	recorded := make(chan struct{})

	go func() {
		async.Record(asyncTestEntry(3))
		close(recorded)
	}()

	select {
	case <-recorded:
		t.Fatal("Record returned while the default blocking queue was full")

	case <-time.After(20 * time.Millisecond):
	}

	stats := async.Stats()
	if stats.CurrentlyBlocked != 1 || stats.DroppedNewest != 0 || stats.DroppedOldest != 0 {
		t.Fatalf("unexpected blocked stats: %+v", stats)
	}

	close(sink.release)
	waitSignal(t, recorded, "blocked Record")
	closeAsyncRecorder(t, async)

	if got := sink.indices(); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("entries = %v, want [1 2 3]", got)
	}

	stats = async.Stats()
	if stats.BlockedRecords != 1 || stats.CurrentlyBlocked != 0 || stats.TotalBlockTime <= 0 {
		t.Fatalf("unexpected final blocked stats: %+v", stats)
	}
}

func TestAsyncRecorderDropNewestPreservesAcceptedPrefix(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink,
		WithAsyncQueueCapacity(1),
		WithAsyncBackpressurePolicy(AsyncDropNewest),
	)
	async.Record(asyncTestEntry(1))
	waitSignal(t, sink.started, "first sink call")
	async.Record(asyncTestEntry(2))
	async.Record(asyncTestEntry(3))
	close(sink.release)
	closeAsyncRecorder(t, async)

	if got := sink.indices(); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("entries = %v, want [1 2]", got)
	}

	stats := async.Stats()
	if stats.Accepted != 2 || stats.Processed != 2 || stats.DroppedNewest != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderDropOldestKeepsNewest(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink,
		WithAsyncQueueCapacity(1),
		WithAsyncBackpressurePolicy(AsyncDropOldest),
	)
	async.Record(asyncTestEntry(1))
	waitSignal(t, sink.started, "first sink call")
	async.Record(asyncTestEntry(2))
	async.Record(asyncTestEntry(3))
	close(sink.release)
	closeAsyncRecorder(t, async)

	if got := sink.indices(); fmt.Sprint(got) != "[1 3]" {
		t.Fatalf("entries = %v, want [1 3]", got)
	}

	stats := async.Stats()
	if stats.Accepted != 3 || stats.Processed != 2 || stats.DroppedOldest != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderCloseUnblocksBlockedRecords(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink, WithAsyncQueueCapacity(1))
	async.Record(asyncTestEntry(1))
	waitSignal(t, sink.started, "first sink call")
	async.Record(asyncTestEntry(2))

	recorded := make(chan struct{})

	go func() {
		async.Record(asyncTestEntry(3))
		close(recorded)
	}()

	waitFor(t, func() bool { return async.Stats().CurrentlyBlocked == 1 }, "blocked producer")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := async.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", err)
	}

	waitSignal(t, recorded, "Record released by Close")

	stats := async.Stats()
	if stats.DroppedClosed != 1 || stats.CurrentlyBlocked != 0 {
		t.Fatalf("unexpected stats after Close: %+v", stats)
	}

	close(sink.release)
	closeAsyncRecorder(t, async)
}

func TestAsyncRecorderCloseTimeoutContinuesDraining(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink, WithAsyncQueueCapacity(1))
	async.Record(asyncTestEntry(1))
	waitSignal(t, sink.started, "first sink call")
	async.Record(asyncTestEntry(2))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := async.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", err)
	}

	close(sink.release)
	closeAsyncRecorder(t, async)

	if got := sink.indices(); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("entries = %v, want [1 2]", got)
	}
}

func TestAsyncRecorderConcurrentCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	sink := &closingRecorder{}
	async := mustAsyncRecorder(t, sink, WithAsyncCloseSink(true))
	async.Record(asyncTestEntry(1))

	const closers = 8

	errs := make(chan error, closers)

	var wg sync.WaitGroup
	for range closers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			errs <- async.Close(context.Background())
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	if sink.closed.Load() != 1 {
		t.Fatalf("sink Close calls = %d, want 1", sink.closed.Load())
	}
}

func TestAsyncRecorderContainsSinkPanicAndContinues(t *testing.T) {
	t.Parallel()

	var (
		calls    atomic.Int64
		reported atomic.Int64
	)

	sink := RecorderFunc(func(*Entry) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
	})
	async := mustAsyncRecorder(t, sink, WithAsyncErrorHandler(func(error) {
		reported.Add(1)
		panic("contained callback panic")
	}))
	async.Record(asyncTestEntry(1))
	async.Record(asyncTestEntry(2))

	err := closeAsyncRecorderError(t, async)
	if err == nil || err.Error() != "recorder: async sink panic: boom" {
		t.Fatalf("Close error = %v", err)
	}

	if calls.Load() != 2 || reported.Load() != 1 {
		t.Fatalf("calls=%d reported=%d", calls.Load(), reported.Load())
	}

	stats := async.Stats()
	if stats.Processed != 2 || stats.SinkPanics != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderContainsBatchSinkPanicAndContinues(t *testing.T) {
	t.Parallel()

	sink := &panicBatchRecorder{}
	async := mustAsyncRecorder(t, sink,
		WithAsyncQueueCapacity(4),
		WithAsyncBatchSize(2),
		WithAsyncFlushInterval(time.Second),
	)

	for i := range 4 {
		async.Record(asyncTestEntry(i))
	}

	err := closeAsyncRecorderError(t, async)
	if err == nil || err.Error() != "recorder: async sink panic: batch boom" {
		t.Fatalf("Close error = %v", err)
	}

	stats := async.Stats()
	if sink.calls.Load() != 2 || stats.Processed != 4 || stats.BatchesProcessed != 2 || stats.SinkPanics != 1 {
		t.Fatalf("calls=%d stats=%+v", sink.calls.Load(), stats)
	}
}

func TestAsyncRecorderObservesSinkErrorOnce(t *testing.T) {
	t.Parallel()

	sinkErr := errors.New("write failed")
	sink := &errorRecorder{err: sinkErr}
	async := mustAsyncRecorder(t, sink)
	async.Record(asyncTestEntry(1))
	async.Record(asyncTestEntry(2))

	err := closeAsyncRecorderError(t, async)
	if !errors.Is(err, sinkErr) {
		t.Fatalf("Close error = %v, want %v", err, sinkErr)
	}

	if stats := async.Stats(); stats.SinkErrors != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderOptionallyClosesSink(t *testing.T) {
	t.Parallel()

	sink := &closingRecorder{}
	async := mustAsyncRecorder(t, sink, WithAsyncCloseSink(true))
	async.Record(asyncTestEntry(1))
	closeAsyncRecorder(t, async)

	if sink.closed.Load() != 1 {
		t.Fatalf("sink Close calls = %d, want 1", sink.closed.Load())
	}
}

func TestAsyncRecorderReportsDownstreamCloseError(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("flush failed")
	sink := &closingRecorder{err: closeErr}
	async := mustAsyncRecorder(t, sink, WithAsyncCloseSink(true))

	err := closeAsyncRecorderError(t, async)
	if !errors.Is(err, closeErr) {
		t.Fatalf("Close error = %v, want %v", err, closeErr)
	}

	if stats := async.Stats(); stats.SinkErrors != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sink Recorder
		opts []AsyncRecorderOption
	}{
		{name: "nil sink"},
		{name: "zero capacity", sink: RecorderFunc(func(*Entry) {}), opts: []AsyncRecorderOption{WithAsyncQueueCapacity(0)}},
		{name: "negative capacity", sink: RecorderFunc(func(*Entry) {}), opts: []AsyncRecorderOption{WithAsyncQueueCapacity(-1)}},
		{name: "unknown policy", sink: RecorderFunc(func(*Entry) {}), opts: []AsyncRecorderOption{WithAsyncBackpressurePolicy(99)}},
		{name: "zero batch size", sink: RecorderFunc(func(*Entry) {}), opts: []AsyncRecorderOption{WithAsyncBatchSize(0)}},
		{name: "batch exceeds queue", sink: newBatchCollectingRecorder(), opts: []AsyncRecorderOption{WithAsyncQueueCapacity(1), WithAsyncBatchSize(2)}},
		{name: "negative flush interval", sink: RecorderFunc(func(*Entry) {}), opts: []AsyncRecorderOption{WithAsyncFlushInterval(-1)}},
		{name: "flush interval without batch", sink: RecorderFunc(func(*Entry) {}), opts: []AsyncRecorderOption{WithAsyncFlushInterval(time.Second)}},
		{name: "batch sink unsupported", sink: RecorderFunc(func(*Entry) {}), opts: []AsyncRecorderOption{WithAsyncBatchSize(2)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewAsyncRecorder(tt.sink, tt.opts...); err == nil {
				t.Fatal("NewAsyncRecorder succeeded")
			}
		})
	}
}

func TestAsyncRecorderConcurrentProducers(t *testing.T) {
	t.Parallel()

	sink := &collectingRecorder{}
	async := mustAsyncRecorder(t, sink, WithAsyncQueueCapacity(8))

	const (
		producers          = 16
		entriesPerProducer = 50
	)

	var wg sync.WaitGroup
	for producer := range producers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for sequence := range entriesPerProducer {
				async.Record(asyncTestEntry(producer*entriesPerProducer + sequence))
			}
		}()
	}

	wg.Wait()
	closeAsyncRecorder(t, async)

	stats := async.Stats()

	want := uint64(producers * entriesPerProducer)
	if stats.Accepted != want || stats.Processed != want || uint64(len(sink.indices())) != want {
		t.Fatalf("unexpected stats: %+v entries=%d", stats, len(sink.indices()))
	}
}

func TestAsyncRecorderRecordAfterCloseIsCounted(t *testing.T) {
	t.Parallel()

	async := mustAsyncRecorder(t, RecorderFunc(func(*Entry) {}))
	closeAsyncRecorder(t, async)
	async.Record(asyncTestEntry(1))

	if stats := async.Stats(); stats.DroppedClosed != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderCloseRejectsNilContext(t *testing.T) {
	t.Parallel()

	async := mustAsyncRecorder(t, RecorderFunc(func(*Entry) {}))
	if err := async.Close(nil); err == nil { //nolint:staticcheck // Explicitly test the defensive nil-context contract.
		t.Fatal("Close(nil) succeeded")
	}

	closeAsyncRecorder(t, async)
}

type collectingRecorder struct {
	mu      sync.Mutex
	entries []*Entry
}

type batchCollectingRecorder struct {
	collectingRecorder
	muBatch   sync.Mutex
	batches   []int
	delivered chan struct{}
}

func newBatchCollectingRecorder() *batchCollectingRecorder {
	return &batchCollectingRecorder{delivered: make(chan struct{}, 16)}
}

func (r *batchCollectingRecorder) RecordBatch(entries []*Entry) {
	r.muBatch.Lock()
	r.batches = append(r.batches, len(entries))
	r.muBatch.Unlock()

	for _, entry := range entries {
		r.Record(entry)
	}

	r.delivered <- struct{}{}
}

func (r *batchCollectingRecorder) batchSizes() []int {
	r.muBatch.Lock()
	defer r.muBatch.Unlock()

	return append([]int(nil), r.batches...)
}

func (r *collectingRecorder) Record(entry *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = append(r.entries, entry)
}

func (r *collectingRecorder) indices() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	indices := make([]int, len(r.entries))
	for i, entry := range r.entries {
		indices[i] = int(entry.Time)
	}

	return indices
}

type gatedRecorder struct {
	collectingRecorder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedRecorder() *gatedRecorder {
	return &gatedRecorder{started: make(chan struct{}), release: make(chan struct{})}
}

func (r *gatedRecorder) Record(entry *Entry) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	r.collectingRecorder.Record(entry)
}

type errorRecorder struct {
	err error
}

func (*errorRecorder) Record(*Entry) {}
func (r *errorRecorder) Err() error  { return r.err }

type panicBatchRecorder struct {
	calls atomic.Int64
}

func (*panicBatchRecorder) Record(*Entry) {}
func (r *panicBatchRecorder) RecordBatch([]*Entry) {
	if r.calls.Add(1) == 1 {
		panic("batch boom")
	}
}

type closingRecorder struct {
	closed atomic.Int64
	err    error
}

func (*closingRecorder) Record(*Entry) {}
func (r *closingRecorder) Close() error {
	r.closed.Add(1)

	return r.err
}

func asyncTestEntry(index int) *Entry {
	return &Entry{Time: float64(index)}
}

func mustAsyncRecorder(t *testing.T, sink Recorder, opts ...AsyncRecorderOption) *AsyncRecorder {
	t.Helper()

	async, err := NewAsyncRecorder(sink, opts...)
	if err != nil {
		t.Fatalf("NewAsyncRecorder: %v", err)
	}

	return async
}

func closeAsyncRecorder(t *testing.T, async *AsyncRecorder) {
	t.Helper()

	if err := closeAsyncRecorderError(t, async); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func closeAsyncRecorderError(t *testing.T, async *AsyncRecorder) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return async.Close(ctx)
}

func waitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitFor(t *testing.T, condition func() bool, name string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", name)
		}

		time.Sleep(time.Millisecond)
	}
}
