package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// AsyncBackpressurePolicy controls what AsyncRecorder does when its bounded
// queue is full.
type AsyncBackpressurePolicy uint8

const (
	// AsyncBlock preserves entries by waiting for queue capacity. This is the
	// default. A stalled sink can therefore delay response-body Read or Close
	// calls that finalize HTTP exchanges.
	AsyncBlock AsyncBackpressurePolicy = iota

	// AsyncDropNewest preserves the already accepted FIFO prefix and drops the
	// entry currently being recorded when the queue is full.
	AsyncDropNewest

	// AsyncDropOldest removes the oldest queued (not in-flight) entry to make
	// room for the entry currently being recorded.
	AsyncDropOldest
)

const defaultAsyncQueueCapacity = 1024

// ErrAsyncRecorderClosed is returned when Record is called after shutdown has
// begun. The entry is rejected and reported to the configured drop handler.
var ErrAsyncRecorderClosed = errors.New("recorder: async recorder is closed")

// AsyncDropReason identifies the bounded reason an entry was not delivered.
type AsyncDropReason string

const (
	AsyncDropPolicyNewest  AsyncDropReason = "policy_newest"
	AsyncDropPolicyOldest  AsyncDropReason = "policy_oldest"
	AsyncDropTimeoutNewest AsyncDropReason = "timeout_newest"
	AsyncDropTimeoutOldest AsyncDropReason = "timeout_oldest"
	AsyncDropClosed        AsyncDropReason = "closed"
)

// AsyncDropHandler observes an entry that AsyncRecorder discarded. It runs
// after the queue lock is released and must be concurrency-safe and bounded.
// Returned errors are reported through AsyncRecorder's internal-error policy
// and returned by Close.
type AsyncDropHandler func(*Entry, AsyncDropReason) error

// AsyncRecorderStats is a point-in-time snapshot of queue and sink activity.
// Processed means the downstream Record call was attempted; the minimal
// Recorder interface cannot prove that a sink durably persisted an entry.
type AsyncRecorderStats struct {
	Capacity int

	Accepted         uint64
	Processed        uint64
	BatchesProcessed uint64
	MaxBatchSize     int

	BlockedRecords   uint64
	CurrentlyBlocked int
	TotalBlockTime   time.Duration
	MaxBlockTime     time.Duration
	OldestBlockAge   time.Duration

	DroppedNewest        uint64
	DroppedOldest        uint64
	DroppedTimeoutNewest uint64
	DroppedTimeoutOldest uint64
	DroppedClosed        uint64
	DropHandlerErrors    uint64
	DropHandlerPanics    uint64

	SinkPanics uint64
	SinkErrors uint64

	Pending  int
	InFlight int
}

// AsyncRecorderConfig defines bounded queue, batching, backpressure, callback,
// and downstream ownership behavior for NewAsyncRecorder.
type AsyncRecorderConfig struct {
	// QueueCapacity is the maximum number of accepted entries awaiting delivery.
	QueueCapacity int
	// Backpressure controls behavior when QueueCapacity is exhausted.
	Backpressure AsyncBackpressurePolicy
	// CloseSink transfers io.Closer ownership of the downstream sink.
	CloseSink bool
	// InternalErrorMode selects the internal error policy. See the constants.
	InternalErrorMode InternalErrorMode
	// OnInternalError observes contained sink, callback, and close failures.
	OnInternalError func(error)
	// Logf is used by InternalErrorLog. Nil falls back to log.Printf.
	Logf func(format string, args ...any)
	// BatchSize is the maximum entries passed to RecordBatch; one disables batching.
	BatchSize int
	// FlushInterval bounds how long a partial batch waits; zero flushes immediately.
	// A positive value requires BatchSize above one and a batch-capable sink.
	FlushInterval time.Duration
	// BlockTimeout bounds AsyncBlock; zero waits without a deadline. It is valid
	// only with Backpressure set to AsyncBlock.
	BlockTimeout time.Duration
	// BlockTimeoutPolicy is the drop policy used after BlockTimeout.
	BlockTimeoutPolicy AsyncBackpressurePolicy
	// DropHandler observes entries discarded by policy, timeout, or close.
	DropHandler AsyncDropHandler
}

// DefaultAsyncRecorderConfig returns the evidence-preserving production
// baseline, including visible internal-error logging.
func DefaultAsyncRecorderConfig() AsyncRecorderConfig {
	return AsyncRecorderConfig{
		QueueCapacity:     defaultAsyncQueueCapacity,
		Backpressure:      AsyncBlock,
		BatchSize:         1,
		InternalErrorMode: InternalErrorLog,
	}
}

type asyncRecorderState uint8

const (
	asyncAccepting asyncRecorderState = iota
	asyncClosing
	asyncClosed
)

// AsyncRecorder decouples entry finalization from a downstream Recorder with
// one bounded FIFO queue and one worker. It preserves accepted-entry order but
// does not provide crash durability, retries, or proof of persistence.
type AsyncRecorder struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond

	sink              Recorder
	queue             []*Entry
	head              int
	size              int
	state             asyncRecorderState
	policy            AsyncBackpressurePolicy
	closeSink         bool
	internalErrorMode InternalErrorMode
	onInternalError   func(error)
	logf              func(string, ...any)
	now               func() time.Time
	batchSize         int
	flushWait         time.Duration
	blockWait         time.Duration
	blockDrop         AsyncBackpressurePolicy
	onDrop            AsyncDropHandler
	blocked           map[uint64]time.Time
	nextBlockID       uint64
	done              chan struct{}
	firstErr          error
	stats             AsyncRecorderStats
}

// NewAsyncRecorder wraps sink with a bounded asynchronous queue. Config must
// specify a positive queue capacity and batch size; DefaultAsyncRecorderConfig
// supplies the evidence-preserving baseline of 1,024 entries, batches of one,
// and AsyncBlock. Blocking prevents queue-overflow loss during normal process
// operation, but a slow or stalled sink can delay application goroutines
// finalizing HTTP exchanges.
func NewAsyncRecorder(sink Recorder, config AsyncRecorderConfig) (*AsyncRecorder, error) {
	if sink == nil {
		return nil, errors.New("recorder: async recorder requires a sink")
	}

	if config.QueueCapacity <= 0 {
		return nil, errors.New("recorder: async queue capacity must be positive")
	}

	if config.BatchSize <= 0 {
		return nil, errors.New("recorder: async batch size must be positive")
	}

	if config.BatchSize > config.QueueCapacity {
		return nil, errors.New("recorder: async batch size must not exceed queue capacity")
	}

	if config.FlushInterval < 0 {
		return nil, errors.New("recorder: async flush interval must not be negative")
	}

	if config.FlushInterval > 0 && config.BatchSize == 1 {
		return nil, errors.New("recorder: async flush interval requires batch size above one")
	}

	if config.BlockTimeout < 0 {
		return nil, errors.New("recorder: async block timeout must not be negative")
	}

	if config.BlockTimeout > 0 {
		if config.Backpressure != AsyncBlock {
			return nil, errors.New("recorder: async block timeout requires AsyncBlock policy")
		}

		if config.BlockTimeoutPolicy != AsyncDropNewest && config.BlockTimeoutPolicy != AsyncDropOldest {
			return nil, errors.New("recorder: async block timeout requires a drop fallback")
		}
	}

	if config.BatchSize > 1 {
		if _, ok := sink.(batchRecorder); !ok {
			return nil, errors.New("recorder: async batching requires a batchRecorder sink")
		}
	}

	switch config.Backpressure {
	case AsyncBlock, AsyncDropNewest, AsyncDropOldest:
	default:
		return nil, fmt.Errorf("recorder: unknown async backpressure policy %d", config.Backpressure)
	}

	r := &AsyncRecorder{
		sink:              sink,
		queue:             make([]*Entry, config.QueueCapacity),
		policy:            config.Backpressure,
		closeSink:         config.CloseSink,
		internalErrorMode: config.InternalErrorMode,
		onInternalError:   config.OnInternalError,
		logf:              config.Logf,
		now:               time.Now,
		batchSize:         config.BatchSize,
		flushWait:         config.FlushInterval,
		blockWait:         config.BlockTimeout,
		blockDrop:         config.BlockTimeoutPolicy,
		onDrop:            config.DropHandler,
		blocked:           make(map[uint64]time.Time),
		done:              make(chan struct{}),
	}
	r.notEmpty = sync.NewCond(&r.mu)
	r.notFull = sync.NewCond(&r.mu)

	go r.run()

	return r, nil
}

// Record implements Recorder. With AsyncBlock it may wait for queue capacity;
// dropping policies return immediately when the queue is full. Calls made
// after Close begins return ErrAsyncRecorderClosed and are counted as
// DroppedClosed. Configured drop policies are intentional outcomes and return
// nil while remaining observable through Stats and DropHandler.
func (r *AsyncRecorder) Record(entry *Entry) error {
	r.mu.Lock()

	blocked := false
	timedOut := false

	var blockedAt time.Time

	var blockID uint64

	var timer *time.Timer

	for r.state == asyncAccepting && r.size == len(r.queue) && r.policy == AsyncBlock {
		if !blocked {
			blocked = true
			blockedAt = r.now()
			blockID = r.nextBlockID
			r.nextBlockID++
			r.blocked[blockID] = blockedAt
			r.stats.BlockedRecords++
			r.stats.CurrentlyBlocked++

			if r.blockWait > 0 {
				timer = time.AfterFunc(r.blockWait, func() {
					r.mu.Lock()
					r.notFull.Broadcast()
					r.mu.Unlock()
				})
			}
		}

		if r.blockWait > 0 && !r.now().Before(blockedAt.Add(r.blockWait)) {
			timedOut = true

			break
		}

		r.notFull.Wait()
	}

	if timer != nil {
		timer.Stop()
	}

	if blocked {
		d := r.now().Sub(blockedAt)
		delete(r.blocked, blockID)
		r.stats.CurrentlyBlocked--

		r.stats.TotalBlockTime += d
		if d > r.stats.MaxBlockTime {
			r.stats.MaxBlockTime = d
		}
	}

	if r.state != asyncAccepting {
		r.stats.DroppedClosed++
		r.mu.Unlock()
		r.handleDrop(entry, AsyncDropClosed)

		return ErrAsyncRecorderClosed
	}

	effectivePolicy := r.policy
	if timedOut {
		effectivePolicy = r.blockDrop
	}

	var dropped *Entry

	var dropReason AsyncDropReason

	if r.size == len(r.queue) {
		switch effectivePolicy {
		case AsyncDropNewest:
			if timedOut {
				r.stats.DroppedTimeoutNewest++
				dropReason = AsyncDropTimeoutNewest
			} else {
				r.stats.DroppedNewest++
				dropReason = AsyncDropPolicyNewest
			}

			r.mu.Unlock()
			r.handleDrop(entry, dropReason)

			return nil

		case AsyncDropOldest:
			dropped = r.queue[r.head]
			r.queue[r.head] = nil
			r.head = (r.head + 1) % len(r.queue)
			r.size--

			if timedOut {
				r.stats.DroppedTimeoutOldest++
				dropReason = AsyncDropTimeoutOldest
			} else {
				r.stats.DroppedOldest++
				dropReason = AsyncDropPolicyOldest
			}
		}
	}

	index := (r.head + r.size) % len(r.queue)
	r.queue[index] = entry
	r.size++
	r.stats.Accepted++
	r.notEmpty.Signal()
	r.mu.Unlock()
	r.handleDrop(dropped, dropReason)

	return nil
}

// Stats returns a concurrency-safe point-in-time snapshot.
func (r *AsyncRecorder) Stats() AsyncRecorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	stats := r.stats
	stats.Capacity = len(r.queue)
	stats.Pending = r.size

	now := r.now()
	for _, started := range r.blocked {
		age := now.Sub(started)
		if age > stats.OldestBlockAge {
			stats.OldestBlockAge = age
		}
	}

	return stats
}

func (r *AsyncRecorder) handleDrop(entry *Entry, reason AsyncDropReason) {
	if entry == nil || r.onDrop == nil {
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			r.recordDropHandlerError(fmt.Errorf("recorder: async drop handler panic (%s): %v", reason, recovered), true)
		}
	}()

	if err := r.onDrop(entry, reason); err != nil {
		r.recordDropHandlerError(fmt.Errorf("recorder: async drop handler (%s): %w", reason, err), false)
	}
}

func (r *AsyncRecorder) recordDropHandlerError(err error, panicked bool) {
	r.mu.Lock()
	if r.firstErr == nil {
		r.firstErr = err
	}

	if panicked {
		r.stats.DropHandlerPanics++
	} else {
		r.stats.DropHandlerErrors++
	}

	r.mu.Unlock()

	reportInternalError(r.internalErrorMode, r.onInternalError, r.logf, err)
}

func (r *AsyncRecorder) firstError() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.firstErr
}

// Close stops accepting entries and waits for the queue and active downstream
// call to drain. If ctx expires, draining continues in the background and a
// later Close call may wait again. Active Recorder.Record calls cannot be
// cancelled because Recorder intentionally has no context-aware method.
func (r *AsyncRecorder) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("recorder: async recorder close requires a context")
	}

	r.mu.Lock()
	if r.state == asyncAccepting {
		r.state = asyncClosing
		r.notEmpty.Broadcast()
		r.notFull.Broadcast()
	}

	done := r.done
	r.mu.Unlock()

	select {
	case <-done:
		return r.firstError()

	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *AsyncRecorder) run() {
	batchBuffer := make([]*Entry, r.batchSize)

	for {
		r.mu.Lock()
		for r.size == 0 && r.state == asyncAccepting {
			r.notEmpty.Wait()
		}

		if r.size == 0 && r.state != asyncAccepting {
			r.mu.Unlock()
			r.finish()

			return
		}

		r.waitForBatchLocked()

		batchSize := min(r.size, r.batchSize)

		batch := batchBuffer[:batchSize]
		for i := range batchSize {
			batch[i] = r.queue[r.head]
			r.queue[r.head] = nil
			r.head = (r.head + 1) % len(r.queue)
			r.size--
		}

		r.stats.InFlight = batchSize
		r.notFull.Broadcast()
		r.mu.Unlock()

		r.deliverBatch(batch)

		r.mu.Lock()
		r.stats.Processed += uint64(batchSize)

		r.stats.BatchesProcessed++
		if batchSize > r.stats.MaxBatchSize {
			r.stats.MaxBatchSize = batchSize
		}

		r.stats.InFlight = 0
		r.mu.Unlock()

		for i := range batch {
			batch[i] = nil
		}
	}
}

func (r *AsyncRecorder) waitForBatchLocked() {
	if r.batchSize == 1 || r.flushWait == 0 || r.size >= r.batchSize || r.state != asyncAccepting {
		return
	}

	expired := false
	timer := time.AfterFunc(r.flushWait, func() {
		r.mu.Lock()
		expired = true

		r.notEmpty.Broadcast()
		r.mu.Unlock()
	})

	for r.size < r.batchSize && r.state == asyncAccepting && !expired {
		r.notEmpty.Wait()
	}

	timer.Stop()
}

func (r *AsyncRecorder) deliverBatch(entries []*Entry) {
	if sink, ok := r.sink.(batchRecorder); ok && r.batchSize > 1 {
		r.deliver(func() error { return sink.RecordBatch(entries) })

		return
	}

	for _, entry := range entries {
		r.deliver(func() error { return r.sink.Record(entry) })
	}
}

func (r *AsyncRecorder) deliver(call func() error) {
	panicked := true

	defer func() {
		if recovered := recover(); panicked {
			r.recordError(fmt.Errorf("recorder: async sink panic: %v", recovered), true)
		}
	}()

	err := call()

	panicked = false

	if err != nil {
		r.recordError(fmt.Errorf("recorder: async sink: %w", err), false)
	}
}

func (r *AsyncRecorder) finish() {
	if r.closeSink {
		if closer, ok := r.sink.(io.Closer); ok {
			if err := r.callClose(closer); err != nil {
				r.recordError(fmt.Errorf("recorder: close async sink: %w", err), false)
			}
		}
	}

	r.mu.Lock()
	r.state = asyncClosed
	close(r.done)
	r.mu.Unlock()
}

func (r *AsyncRecorder) callClose(closer io.Closer) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("sink close panic: %v", recovered)
		}
	}()

	return closer.Close()
}

func (r *AsyncRecorder) recordError(err error, panicked bool) {
	if err == nil {
		return
	}

	r.mu.Lock()
	if r.firstErr == nil {
		r.firstErr = err
	}

	if panicked {
		r.stats.SinkPanics++
	} else {
		r.stats.SinkErrors++
	}

	r.mu.Unlock()

	reportInternalError(r.internalErrorMode, r.onInternalError, r.logf, err)
}
