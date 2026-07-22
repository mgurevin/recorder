package recorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ErrDebugStreamRecorderClosed is returned when Record is called after the
// development stream has been closed.
var ErrDebugStreamRecorderClosed = errors.New("recorder: debug stream recorder is closed")

// DebugStreamRecorderConfig controls the bounded, single-subscriber
// development stream. QueueCapacity must be positive. AllowedOrigins adds
// exact HTTP(S) browser origins to the loopback origins accepted by default.
type DebugStreamRecorderConfig struct {
	QueueCapacity  int
	AllowedOrigins []string
}

// DefaultDebugStreamRecorderConfig returns conservative settings for local
// interactive inspection.
func DefaultDebugStreamRecorderConfig() DebugStreamRecorderConfig {
	return DebugStreamRecorderConfig{QueueCapacity: 256}
}

// DebugStreamRecorderStats is a point-in-time view of the development stream.
type DebugStreamRecorderStats struct {
	SubscriberActive bool
	Published        uint64
	Dropped          uint64
}

type debugStreamMessage struct {
	id   uint64
	data []byte
}

// DebugStreamRecorder publishes finalized entries as server-sent events to
// one local subscriber. It is intended only for live local development and
// debugging: its bounded queue retains recent entries while disconnected,
// drops the oldest queued entry when full, and is not a durable evidence sink.
//
// DebugStreamRecorder is an http.Handler. The application owns the HTTP
// server and its lifecycle; ServeHTTP accepts only loopback peers. Browser
// origins are restricted to loopback unless explicitly configured.
type DebugStreamRecorder struct {
	mu             sync.Mutex
	queue          chan debugStreamMessage
	nextID         uint64
	published      uint64
	dropped        uint64
	pendingDrops   uint64
	subscriber     bool
	closed         bool
	allowedOrigins map[string]struct{}
}

// NewDebugStreamRecorder creates a bounded, single-subscriber development
// stream. Use DefaultDebugStreamRecorderConfig as the baseline.
func NewDebugStreamRecorder(config DebugStreamRecorderConfig) (*DebugStreamRecorder, error) {
	if config.QueueCapacity <= 0 {
		return nil, errors.New("recorder: debug stream queue capacity must be positive")
	}

	allowedOrigins := make(map[string]struct{}, len(config.AllowedOrigins))

	for i, origin := range config.AllowedOrigins {
		normalized, ok := normalizeHTTPOrigin(origin)
		if !ok {
			return nil, fmt.Errorf("recorder: invalid debug stream allowed origin at index %d", i)
		}

		allowedOrigins[normalized] = struct{}{}
	}

	return &DebugStreamRecorder{
		queue:          make(chan debugStreamMessage, config.QueueCapacity),
		allowedOrigins: allowedOrigins,
	}, nil
}

// Record implements Recorder. Entries wait in the bounded queue until the
// subscriber receives them. When the queue is full, Record drops its oldest
// entry and reports that loss to the next subscriber as a gap event.
func (r *DebugStreamRecorder) Record(entry *Entry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("recorder: encode debug stream entry: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return ErrDebugStreamRecorderClosed
	}

	r.nextID++
	message := debugStreamMessage{id: r.nextID, data: data}

	select {
	case r.queue <- message:
	default:
		<-r.queue
		r.dropped++

		r.pendingDrops++
		r.queue <- message
	}

	r.published++

	return nil
}

// Stats returns a concurrency-safe snapshot.
func (r *DebugStreamRecorder) Stats() DebugStreamRecorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	return DebugStreamRecorderStats{
		SubscriberActive: r.subscriber,
		Published:        r.published,
		Dropped:          r.dropped,
	}
}

// Close disconnects the active subscriber and rejects future records. It is
// safe to call more than once.
func (r *DebugStreamRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	r.closed = true

	close(r.queue)

	return nil
}

// ServeHTTP streams entry and gap events. A second concurrent subscriber gets
// HTTP 409. Requests from non-loopback peers are always rejected. Browser
// origins must be loopback or explicitly listed in AllowedOrigins.
func (r *DebugStreamRecorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !isLoopbackRemote(request.RemoteAddr) {
		http.Error(w, "debug stream is restricted to loopback clients", http.StatusForbidden)

		return
	}

	origin := request.Header.Get("Origin")

	if origin != "" {
		if !r.isOriginAllowed(origin) {
			http.Error(w, "debug stream origin is not allowed", http.StatusForbidden)

			return
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)

		return
	}

	queue, ok := r.subscribe()
	if !ok {
		http.Error(w, "a debug stream subscriber is already connected", http.StatusConflict)

		return
	}

	defer r.unsubscribe(queue)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")

	flusher.Flush()

	for {
		select {
		case <-request.Context().Done():
			return

		case message, open := <-queue:
			if !open {
				return
			}

			if dropped := r.takePendingDrops(queue); dropped > 0 {
				if _, err := fmt.Fprintf(w, "event: gap\ndata: {\"dropped\":%d}\n\n", dropped); err != nil {
					return
				}
			}

			if _, err := fmt.Fprintf(w, "id: %d\nevent: entry\ndata: %s\n\n", message.id, message.data); err != nil {
				return
			}

			flusher.Flush()
		}
	}
}

func (r *DebugStreamRecorder) subscribe() (chan debugStreamMessage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.subscriber {
		return nil, false
	}

	r.subscriber = true

	return r.queue, true
}

func (r *DebugStreamRecorder) unsubscribe(queue chan debugStreamMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.queue == queue {
		r.subscriber = false
	}
}

func (r *DebugStreamRecorder) takePendingDrops(queue chan debugStreamMessage) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.queue != queue {
		return 0
	}

	dropped := r.pendingDrops
	r.pendingDrops = 0

	return dropped
}

func isLoopbackRemote(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}

	ip := net.ParseIP(strings.Trim(host, "[]"))

	return ip != nil && ip.IsLoopback()
}

func isLoopbackOrigin(origin string) bool {
	normalized, ok := normalizeHTTPOrigin(origin)
	if !ok {
		return false
	}

	parsed, _ := url.Parse(normalized)
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

func (r *DebugStreamRecorder) isOriginAllowed(origin string) bool {
	if isLoopbackOrigin(origin) {
		return true
	}

	normalized, ok := normalizeHTTPOrigin(origin)
	if !ok {
		return false
	}

	_, ok = r.allowedOrigins[normalized]

	return ok
}

func normalizeHTTPOrigin(origin string) (string, bool) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Opaque != "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}

	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", false
	}

	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}

	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}

	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}

	return parsed.Scheme + "://" + host, true
}
