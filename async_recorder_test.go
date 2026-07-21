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
	async := mustAsyncRecorder(t, sink, withAsyncQueueCapacity(4))

	for i := range 10 {
		_ = async.Record(asyncTestEntry(i))
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
		withAsyncQueueCapacity(8),
		withAsyncBatchSize(3),
		withAsyncFlushInterval(time.Second),
	)

	for i := range 6 {
		_ = async.Record(asyncTestEntry(i))
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
		withAsyncBatchSize(4),
		withAsyncFlushInterval(20*time.Millisecond),
	)
	_ = async.Record(asyncTestEntry(1))
	_ = async.Record(asyncTestEntry(2))

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
		withAsyncBatchSize(4),
		withAsyncFlushInterval(time.Hour),
	)
	_ = async.Record(asyncTestEntry(1))
	_ = async.Record(asyncTestEntry(2))
	closeAsyncRecorder(t, async)

	if got := sink.batchSizes(); fmt.Sprint(got) != "[2]" {
		t.Fatalf("batch sizes = %v", got)
	}
}

func TestAsyncRecorderDefaultBlocksInsteadOfDropping(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink, withAsyncQueueCapacity(1))
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))

	recorded := make(chan struct{})

	go func() {
		_ = async.Record(asyncTestEntry(3))

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

	if stats.OldestBlockAge <= 0 {
		t.Fatalf("oldest block age = %v", stats.OldestBlockAge)
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

func TestAsyncRecorderBlockTimeoutDropsNewest(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	dropped := make(chan AsyncDropReason, 1)
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncBlockTimeout(20*time.Millisecond, AsyncDropNewest),
		withAsyncDropHandler(func(entry *Entry, reason AsyncDropReason) error {
			if entry.Time != 3 {
				t.Errorf("dropped entry = %v", entry.Time)
			}

			dropped <- reason

			return nil
		}),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))
	_ = async.Record(asyncTestEntry(3))

	if reason := <-dropped; reason != AsyncDropTimeoutNewest {
		t.Fatalf("drop reason = %q", reason)
	}

	stats := async.Stats()
	if stats.DroppedTimeoutNewest != 1 || stats.DroppedNewest != 0 || stats.CurrentlyBlocked != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	close(sink.release)
	closeAsyncRecorder(t, async)
}

func TestAsyncRecorderBlockTimeoutDropsOldest(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	dropped := make(chan *Entry, 1)
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncBlockTimeout(20*time.Millisecond, AsyncDropOldest),
		withAsyncDropHandler(func(entry *Entry, reason AsyncDropReason) error {
			if reason != AsyncDropTimeoutOldest {
				t.Errorf("drop reason = %q", reason)
			}

			dropped <- entry

			return nil
		}),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))
	_ = async.Record(asyncTestEntry(3))

	if entry := <-dropped; entry.Time != 2 {
		t.Fatalf("dropped entry = %v", entry.Time)
	}

	close(sink.release)
	closeAsyncRecorder(t, async)

	if got := sink.indices(); fmt.Sprint(got) != "[1 3]" {
		t.Fatalf("entries = %v, want [1 3]", got)
	}

	stats := async.Stats()
	if stats.DroppedTimeoutOldest != 1 || stats.DroppedOldest != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderBlockTimeoutConcurrentProducers(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncBlockTimeout(10*time.Millisecond, AsyncDropNewest),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))

	const producers = 1000

	var wg sync.WaitGroup
	wg.Add(producers)

	for i := range producers {
		go func() {
			defer wg.Done()

			_ = async.Record(asyncTestEntry(i + 3))
		}()
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	waitSignal(t, done, "timed-out concurrent producers")

	stats := async.Stats()
	if stats.DroppedTimeoutNewest != producers || stats.CurrentlyBlocked != 0 || stats.OldestBlockAge != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	close(sink.release)
	closeAsyncRecorder(t, async)
}

func TestAsyncRecorderDropHandlerCanReleaseFileBodyAssets(t *testing.T) {
	t.Parallel()

	store := mustFileBodyStore(t, t.TempDir())

	w, err := store.NewWriter(context.Background(), BodyMetadata{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dropped := &Entry{Recorder: &RecorderEntryExtension{SchemaVersion: RecorderExtensionVersion, ResponseBody: &BodyInfo{Store: w.Ref()}}}
	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncBackpressurePolicy(AsyncDropNewest),
		withAsyncDropHandler(func(entry *Entry, _ AsyncDropReason) error {
			return store.ReleaseEntryAssets(entry)
		}),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))
	_ = async.Record(dropped)

	if stats := store.Stats(); stats.CommittedFiles != 0 || stats.ReleasedTotal != 1 {
		t.Fatalf("body store stats = %+v", stats)
	}

	close(sink.release)
	closeAsyncRecorder(t, async)
}

func TestAsyncRecorderContainsDropHandlerPanic(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	errorsSeen := make(chan error, 1)
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncBackpressurePolicy(AsyncDropNewest),
		withAsyncDropHandler(func(*Entry, AsyncDropReason) error { panic("drop boom") }),
		withAsyncOnInternalError(func(err error) { errorsSeen <- err }),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))
	_ = async.Record(asyncTestEntry(3))

	if err := <-errorsSeen; err == nil {
		t.Fatal("drop handler panic was not reported")
	}

	if stats := async.Stats(); stats.DropHandlerPanics != 1 || stats.SinkPanics != 0 || stats.SinkErrors != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	close(sink.release)

	if err := closeAsyncRecorderError(t, async); err == nil {
		t.Fatal("Close did not report drop handler panic")
	}
}

func TestAsyncRecorderReportsDropHandlerError(t *testing.T) {
	t.Parallel()

	want := errors.New("release failed")
	sink := newGatedRecorder()
	errorsSeen := make(chan error, 1)
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncBackpressurePolicy(AsyncDropNewest),
		withAsyncDropHandler(func(*Entry, AsyncDropReason) error { return want }),
		withAsyncOnInternalError(func(err error) { errorsSeen <- err }),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))
	_ = async.Record(asyncTestEntry(3))

	if err := <-errorsSeen; !errors.Is(err, want) {
		t.Fatalf("internal error = %v, want %v", err, want)
	}

	if stats := async.Stats(); stats.DropHandlerErrors != 1 || stats.DropHandlerPanics != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	close(sink.release)

	if err := closeAsyncRecorderError(t, async); !errors.Is(err, want) {
		t.Fatalf("Close error = %v, want %v", err, want)
	}
}

func TestAsyncRecorderDropNewestPreservesAcceptedPrefix(t *testing.T) {
	t.Parallel()

	sink := newGatedRecorder()
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncBackpressurePolicy(AsyncDropNewest),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))
	_ = async.Record(asyncTestEntry(3))

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
		withAsyncQueueCapacity(1),
		withAsyncBackpressurePolicy(AsyncDropOldest),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))
	_ = async.Record(asyncTestEntry(3))

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
	dropped := make(chan AsyncDropReason, 1)
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(1),
		withAsyncDropHandler(func(entry *Entry, reason AsyncDropReason) error {
			if entry.Time != 3 {
				t.Errorf("dropped entry = %v", entry.Time)
			}

			dropped <- reason

			return nil
		}),
	)
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))

	recorded := make(chan struct{})

	go func() {
		_ = async.Record(asyncTestEntry(3))

		close(recorded)
	}()

	waitFor(t, func() bool { return async.Stats().CurrentlyBlocked == 1 }, "blocked producer")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := async.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", err)
	}

	waitSignal(t, recorded, "Record released by Close")

	if reason := <-dropped; reason != AsyncDropClosed {
		t.Fatalf("drop reason = %q", reason)
	}

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
	async := mustAsyncRecorder(t, sink, withAsyncQueueCapacity(1))
	_ = async.Record(asyncTestEntry(1))

	waitSignal(t, sink.started, "first sink call")

	_ = async.Record(asyncTestEntry(2))

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
	async := mustAsyncRecorder(t, sink, withAsyncCloseSink(true))
	_ = async.Record(asyncTestEntry(1))

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

	sink := RecorderFunc(func(*Entry) error {
		if calls.Add(1) == 1 {
			panic("boom")
		}

		return nil
	})
	async := mustAsyncRecorder(t, sink, withAsyncOnInternalError(func(error) {
		reported.Add(1)
		panic("contained callback panic")
	}))
	_ = async.Record(asyncTestEntry(1))
	_ = async.Record(asyncTestEntry(2))

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

func TestAsyncRecorderUsesTransportInternalErrorSemantics(t *testing.T) {
	t.Parallel()

	logged := make(chan string, 1)
	reported := make(chan error, 1)
	sink := RecorderFunc(func(*Entry) error { panic("boom") })
	async := mustAsyncRecorder(t, sink,
		withAsyncInternalErrorMode(InternalErrorLog),
		withAsyncOnInternalError(func(err error) { reported <- err }),
		withAsyncLogf(func(format string, args ...any) { logged <- fmt.Sprintf(format, args...) }),
	)
	_ = async.Record(asyncTestEntry(1))

	if err := closeAsyncRecorderError(t, async); err == nil {
		t.Fatal("Close did not report sink panic")
	}

	if got := <-logged; got != "recorder: recorder: async sink panic: boom" {
		t.Fatalf("log = %q", got)
	}

	if err := <-reported; err == nil || err.Error() != "recorder: async sink panic: boom" {
		t.Fatalf("reported error = %v", err)
	}
}

func TestAsyncRecorderContainsBatchSinkPanicAndContinues(t *testing.T) {
	t.Parallel()

	sink := &panicbatchRecorder{}
	async := mustAsyncRecorder(t, sink,
		withAsyncQueueCapacity(4),
		withAsyncBatchSize(2),
		withAsyncFlushInterval(time.Second),
	)

	for i := range 4 {
		_ = async.Record(asyncTestEntry(i))
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

func TestAsyncRecorderCloseReturnsFirstSinkError(t *testing.T) {
	t.Parallel()

	sinkErr := errors.New("write failed")
	sink := &errorRecorder{err: sinkErr}
	async := mustAsyncRecorder(t, sink)
	_ = async.Record(asyncTestEntry(1))
	_ = async.Record(asyncTestEntry(2))

	err := closeAsyncRecorderError(t, async)
	if !errors.Is(err, sinkErr) {
		t.Fatalf("Close error = %v, want %v", err, sinkErr)
	}

	if stats := async.Stats(); stats.SinkErrors != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderOptionallyClosesSink(t *testing.T) {
	t.Parallel()

	sink := &closingRecorder{}
	async := mustAsyncRecorder(t, sink, withAsyncCloseSink(true))
	_ = async.Record(asyncTestEntry(1))
	closeAsyncRecorder(t, async)

	if sink.closed.Load() != 1 {
		t.Fatalf("sink Close calls = %d, want 1", sink.closed.Load())
	}
}

func TestAsyncRecorderReportsDownstreamCloseError(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("flush failed")
	sink := &closingRecorder{err: closeErr}
	async := mustAsyncRecorder(t, sink, withAsyncCloseSink(true))

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
		opts []asyncConfigMutation
	}{
		{name: "nil sink"},
		{name: "zero capacity", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncQueueCapacity(0)}},
		{name: "negative capacity", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncQueueCapacity(-1)}},
		{name: "unknown policy", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncBackpressurePolicy(99)}},
		{name: "zero batch size", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncBatchSize(0)}},
		{name: "batch exceeds queue", sink: newBatchCollectingRecorder(), opts: []asyncConfigMutation{withAsyncQueueCapacity(1), withAsyncBatchSize(2)}},
		{name: "negative flush interval", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncFlushInterval(-1)}},
		{name: "flush interval without batch", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncFlushInterval(time.Second)}},
		{name: "batch sink unsupported", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncBatchSize(2)}},
		{name: "negative block timeout", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncBlockTimeout(-time.Second, AsyncDropNewest)}},
		{name: "block fallback missing", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncBlockTimeout(time.Second, AsyncBlock)}},
		{name: "block timeout with drop policy", sink: noopRecorder, opts: []asyncConfigMutation{withAsyncBackpressurePolicy(AsyncDropNewest), withAsyncBlockTimeout(time.Second, AsyncDropNewest)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := newAsyncRecorderForTest(tt.sink, tt.opts...); err == nil {
				t.Fatal("NewAsyncRecorder succeeded")
			}
		})
	}
}

func TestAsyncRecorderConcurrentProducers(t *testing.T) {
	t.Parallel()

	sink := &collectingRecorder{}
	async := mustAsyncRecorder(t, sink, withAsyncQueueCapacity(8))

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
				_ = async.Record(asyncTestEntry(producer*entriesPerProducer + sequence))
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

	async := mustAsyncRecorder(t, noopRecorder)
	closeAsyncRecorder(t, async)

	if err := async.Record(asyncTestEntry(1)); !errors.Is(err, ErrAsyncRecorderClosed) {
		t.Fatalf("Record error = %v, want %v", err, ErrAsyncRecorderClosed)
	}

	if stats := async.Stats(); stats.DroppedClosed != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncRecorderCloseRejectsNilContext(t *testing.T) {
	t.Parallel()

	async := mustAsyncRecorder(t, noopRecorder)
	if err := async.Close(nil); err == nil { //nolint:staticcheck // Explicitly test the defensive nil-context contract.
		t.Fatal("Close(nil) succeeded")
	}

	closeAsyncRecorder(t, async)
}

type collectingRecorder struct {
	mu      sync.Mutex
	entries []*Entry
}

var noopRecorder = RecorderFunc(func(*Entry) error { return nil })

type batchCollectingRecorder struct {
	collectingRecorder
	muBatch   sync.Mutex
	batches   []int
	delivered chan struct{}
}

func newBatchCollectingRecorder() *batchCollectingRecorder {
	return &batchCollectingRecorder{delivered: make(chan struct{}, 16)}
}

func (r *batchCollectingRecorder) RecordBatch(entries []*Entry) error {
	r.muBatch.Lock()
	r.batches = append(r.batches, len(entries))
	r.muBatch.Unlock()

	for _, entry := range entries {
		_ = r.Record(entry)
	}

	r.delivered <- struct{}{}

	return nil
}

func (r *batchCollectingRecorder) batchSizes() []int {
	r.muBatch.Lock()
	defer r.muBatch.Unlock()

	return append([]int(nil), r.batches...)
}

func (r *collectingRecorder) Record(entry *Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = append(r.entries, entry)

	return nil
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

func (r *gatedRecorder) Record(entry *Entry) error {
	r.once.Do(func() { close(r.started) })
	<-r.release

	return r.collectingRecorder.Record(entry)
}

type errorRecorder struct {
	err error
}

func (r *errorRecorder) Record(*Entry) error { return r.err }

type panicbatchRecorder struct {
	calls atomic.Int64
}

func (*panicbatchRecorder) Record(*Entry) error { return nil }
func (r *panicbatchRecorder) RecordBatch([]*Entry) error {
	if r.calls.Add(1) == 1 {
		panic("batch boom")
	}

	return nil
}

type closingRecorder struct {
	closed atomic.Int64
	err    error
}

func (*closingRecorder) Record(*Entry) error { return nil }
func (r *closingRecorder) Close() error {
	r.closed.Add(1)

	return r.err
}

func asyncTestEntry(index int) *Entry {
	return &Entry{Time: float64(index)}
}

func mustAsyncRecorder(t *testing.T, sink Recorder, opts ...asyncConfigMutation) *AsyncRecorder {
	t.Helper()

	async, err := newAsyncRecorderForTest(sink, opts...)
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
