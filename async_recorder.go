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
type AsyncDropHandler func(*Entry, AsyncDropReason)

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
	DropHandlerPanics    uint64

	SinkPanics uint64
	SinkErrors uint64

	Pending  int
	InFlight int
}

type asyncRecorderConfig struct {
	capacity  int
	policy    AsyncBackpressurePolicy
	closeSink bool
	onError   func(error)
	now       func() time.Time
	batchSize int
	flushWait time.Duration
	blockWait time.Duration
	blockDrop AsyncBackpressurePolicy
	onDrop    AsyncDropHandler
}

// AsyncRecorderOption configures an AsyncRecorder.
type AsyncRecorderOption func(*asyncRecorderConfig)

// WithAsyncQueueCapacity sets the maximum number of entries waiting for the
// downstream recorder. The value must be positive.
func WithAsyncQueueCapacity(n int) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) { c.capacity = n }
}

// WithAsyncBackpressurePolicy sets the full-queue behavior. The default is
// AsyncBlock, which favors evidence preservation over HTTP latency.
func WithAsyncBackpressurePolicy(policy AsyncBackpressurePolicy) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) { c.policy = policy }
}

// WithAsyncBlockTimeout bounds how long AsyncBlock waits for queue capacity.
// After timeout, fallback must be AsyncDropNewest or AsyncDropOldest. Zero
// preserves the default unbounded wait; negative durations are invalid.
func WithAsyncBlockTimeout(timeout time.Duration, fallback AsyncBackpressurePolicy) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) {
		c.blockWait = timeout
		c.blockDrop = fallback
	}
}

// WithAsyncDropHandler installs a callback for entries discarded by policy,
// timeout, or close. The callback runs outside the queue lock. Panics are
// contained and reported through Stats and WithAsyncErrorHandler.
func WithAsyncDropHandler(handler AsyncDropHandler) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) { c.onDrop = handler }
}

// FileBodyStoreDropHandler returns a drop handler that releases managed body
// assets owned by discarded entries. Release errors are reported to onError;
// a nil callback ignores them after the store records its failure counters.
func FileBodyStoreDropHandler(store *FileBodyStore, onError func(error)) AsyncDropHandler {
	return func(entry *Entry, reason AsyncDropReason) {
		if store == nil {
			return
		}

		if err := store.ReleaseEntryAssets(entry); err != nil && onError != nil {
			onError(fmt.Errorf("recorder: release async-dropped body assets (%s): %w", reason, err))
		}
	}
}

// WithAsyncCloseSink transfers downstream close ownership to AsyncRecorder.
// When enabled, a sink implementing io.Closer is closed after the queue drains.
func WithAsyncCloseSink(enabled bool) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) { c.closeSink = enabled }
}

// WithAsyncErrorHandler installs a best-effort callback for downstream panics,
// observable sink errors, and downstream close failures. Callback panics are
// contained. Errors never alter the HTTP request or response.
func WithAsyncErrorHandler(fn func(error)) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) { c.onError = fn }
}

// WithAsyncBatchSize sets the maximum entries delivered in one RecordBatch
// call. Values above one require a sink implementing BatchRecorder. Default 1.
func WithAsyncBatchSize(n int) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) { c.batchSize = n }
}

// WithAsyncFlushInterval sets how long a non-empty partial batch may wait for
// more entries. Zero flushes the entries currently available without waiting.
// A positive duration requires a sink implementing BatchRecorder and a batch
// size above one.
func WithAsyncFlushInterval(interval time.Duration) AsyncRecorderOption {
	return func(c *asyncRecorderConfig) { c.flushWait = interval }
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

	sink        Recorder
	queue       []*Entry
	head        int
	size        int
	state       asyncRecorderState
	policy      AsyncBackpressurePolicy
	closeSink   bool
	onError     func(error)
	now         func() time.Time
	batchSize   int
	flushWait   time.Duration
	blockWait   time.Duration
	blockDrop   AsyncBackpressurePolicy
	onDrop      AsyncDropHandler
	blocked     map[uint64]time.Time
	nextBlockID uint64
	done        chan struct{}
	firstErr    error
	sinkErrSeen bool
	stats       AsyncRecorderStats
}

// NewAsyncRecorder wraps sink with a bounded asynchronous queue. The default
// capacity is 1024 and the default backpressure policy is AsyncBlock. Blocking
// prevents queue-overflow loss during normal process operation, but a slow or
// stalled sink can delay application goroutines finalizing HTTP exchanges.
func NewAsyncRecorder(sink Recorder, opts ...AsyncRecorderOption) (*AsyncRecorder, error) {
	if sink == nil {
		return nil, errors.New("recorder: async recorder requires a sink")
	}

	config := asyncRecorderConfig{
		capacity:  defaultAsyncQueueCapacity,
		policy:    AsyncBlock,
		now:       time.Now,
		batchSize: 1,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}

	if config.capacity <= 0 {
		return nil, errors.New("recorder: async queue capacity must be positive")
	}

	if config.batchSize <= 0 {
		return nil, errors.New("recorder: async batch size must be positive")
	}

	if config.batchSize > config.capacity {
		return nil, errors.New("recorder: async batch size must not exceed queue capacity")
	}

	if config.flushWait < 0 {
		return nil, errors.New("recorder: async flush interval must not be negative")
	}

	if config.flushWait > 0 && config.batchSize == 1 {
		return nil, errors.New("recorder: async flush interval requires batch size above one")
	}

	if config.blockWait < 0 {
		return nil, errors.New("recorder: async block timeout must not be negative")
	}

	if config.blockWait > 0 {
		if config.policy != AsyncBlock {
			return nil, errors.New("recorder: async block timeout requires AsyncBlock policy")
		}

		if config.blockDrop != AsyncDropNewest && config.blockDrop != AsyncDropOldest {
			return nil, errors.New("recorder: async block timeout requires a drop fallback")
		}
	}

	if config.batchSize > 1 {
		if _, ok := sink.(BatchRecorder); !ok {
			return nil, errors.New("recorder: async batching requires a BatchRecorder sink")
		}
	}

	switch config.policy {
	case AsyncBlock, AsyncDropNewest, AsyncDropOldest:
	default:
		return nil, fmt.Errorf("recorder: unknown async backpressure policy %d", config.policy)
	}

	r := &AsyncRecorder{
		sink:      sink,
		queue:     make([]*Entry, config.capacity),
		policy:    config.policy,
		closeSink: config.closeSink,
		onError:   config.onError,
		now:       config.now,
		batchSize: config.batchSize,
		flushWait: config.flushWait,
		blockWait: config.blockWait,
		blockDrop: config.blockDrop,
		onDrop:    config.onDrop,
		blocked:   make(map[uint64]time.Time),
		done:      make(chan struct{}),
	}
	r.notEmpty = sync.NewCond(&r.mu)
	r.notFull = sync.NewCond(&r.mu)

	go r.run()

	return r, nil
}

// Record implements Recorder. With the default AsyncBlock policy it may wait
// for queue capacity; dropping policies return immediately when the queue is
// full. Calls made after Close begins are counted as DroppedClosed.
func (r *AsyncRecorder) Record(entry *Entry) {
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

		return
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

			return

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
			r.recordDropHandlerPanic(fmt.Errorf("recorder: async drop handler panic: %v", recovered))
		}
	}()

	r.onDrop(entry, reason)
}

func (r *AsyncRecorder) recordDropHandlerPanic(err error) {
	r.mu.Lock()
	if r.firstErr == nil {
		r.firstErr = err
	}

	r.stats.DropHandlerPanics++
	handler := r.onError
	r.mu.Unlock()

	if handler != nil {
		callSafely(func() { handler(err) })
	}
}

// Err returns the first downstream panic or observable sink/close error.
func (r *AsyncRecorder) Err() error {
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
		return r.Err()

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
	if sink, ok := r.sink.(BatchRecorder); ok && r.batchSize > 1 {
		r.deliver(func() { sink.RecordBatch(entries) })
		r.observeSinkError()

		return
	}

	for _, entry := range entries {
		r.deliver(func() { r.sink.Record(entry) })
	}

	r.observeSinkError()
}

func (r *AsyncRecorder) deliver(call func()) {
	panicked := true

	defer func() {
		if recovered := recover(); panicked {
			r.recordError(fmt.Errorf("recorder: async sink panic: %v", recovered), true)
		}
	}()

	call()

	panicked = false
}

func (r *AsyncRecorder) observeSinkError() {
	errSource, ok := r.sink.(interface{ Err() error })
	if !ok {
		return
	}

	r.mu.Lock()
	seen := r.sinkErrSeen
	r.mu.Unlock()

	if seen {
		return
	}

	var err error

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("recorder: async sink Err panic: %v", recovered)
			}
		}()

		err = errSource.Err()
	}()

	if err == nil {
		return
	}

	r.mu.Lock()
	if r.sinkErrSeen {
		r.mu.Unlock()

		return
	}

	r.sinkErrSeen = true
	r.mu.Unlock()
	r.recordError(fmt.Errorf("recorder: async sink: %w", err), false)
}

func (r *AsyncRecorder) finish() {
	if r.closeSink {
		if closer, ok := r.sink.(io.Closer); ok {
			if err := r.callClose(closer); err != nil {
				r.recordError(fmt.Errorf("recorder: close async sink: %w", err), false)
			}
		}
	}

	r.observeSinkError()

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

	handler := r.onError
	r.mu.Unlock()

	if handler != nil {
		callSafely(func() { handler(err) })
	}
}
