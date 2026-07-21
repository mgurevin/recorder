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

// AsyncRecorderStats is a point-in-time snapshot of queue and sink activity.
// Processed means the downstream Record call was attempted; the minimal
// Recorder interface cannot prove that a sink durably persisted an entry.
type AsyncRecorderStats struct {
	Capacity int

	Accepted  uint64
	Processed uint64

	BlockedRecords   uint64
	CurrentlyBlocked int
	TotalBlockTime   time.Duration
	MaxBlockTime     time.Duration

	DroppedNewest uint64
	DroppedOldest uint64
	DroppedClosed uint64

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
		capacity: defaultAsyncQueueCapacity,
		policy:   AsyncBlock,
		now:      time.Now,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}

	if config.capacity <= 0 {
		return nil, errors.New("recorder: async queue capacity must be positive")
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

	var blockedAt time.Time

	for r.state == asyncAccepting && r.size == len(r.queue) && r.policy == AsyncBlock {
		if !blocked {
			blocked = true
			blockedAt = r.now()
			r.stats.BlockedRecords++
			r.stats.CurrentlyBlocked++
		}

		r.notFull.Wait()
	}

	if blocked {
		d := r.now().Sub(blockedAt)
		r.stats.CurrentlyBlocked--

		r.stats.TotalBlockTime += d
		if d > r.stats.MaxBlockTime {
			r.stats.MaxBlockTime = d
		}
	}

	if r.state != asyncAccepting {
		r.stats.DroppedClosed++
		r.mu.Unlock()

		return
	}

	if r.size == len(r.queue) {
		switch r.policy {
		case AsyncDropNewest:
			r.stats.DroppedNewest++
			r.mu.Unlock()

			return

		case AsyncDropOldest:
			r.queue[r.head] = nil
			r.head = (r.head + 1) % len(r.queue)
			r.size--
			r.stats.DroppedOldest++
		}
	}

	index := (r.head + r.size) % len(r.queue)
	r.queue[index] = entry
	r.size++
	r.stats.Accepted++
	r.notEmpty.Signal()
	r.mu.Unlock()
}

// Stats returns a concurrency-safe point-in-time snapshot.
func (r *AsyncRecorder) Stats() AsyncRecorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	stats := r.stats
	stats.Capacity = len(r.queue)
	stats.Pending = r.size

	return stats
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

		entry := r.queue[r.head]
		r.queue[r.head] = nil
		r.head = (r.head + 1) % len(r.queue)
		r.size--
		r.stats.InFlight = 1
		r.notFull.Broadcast()
		r.mu.Unlock()

		r.deliver(entry)

		r.mu.Lock()
		r.stats.Processed++
		r.stats.InFlight = 0
		r.mu.Unlock()
	}
}

func (r *AsyncRecorder) deliver(entry *Entry) {
	panicked := true

	defer func() {
		if recovered := recover(); panicked {
			r.recordError(fmt.Errorf("recorder: async sink panic: %v", recovered), true)
		}
	}()

	r.sink.Record(entry)

	panicked = false

	r.observeSinkError()
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
