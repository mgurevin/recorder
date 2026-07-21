package recorder

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"testing"
)

// ---- in-memory network ----
//
// Benchmarks run the full HTTP client/server stack over net.Pipe instead of
// OS sockets: no ports, no file descriptors, no TIME_WAIT accumulation. This
// keeps high-parallelism benchmarks from exhausting the ephemeral port range
// ("connect: can't assign requested address" on macOS) and measures recorder
// overhead rather than kernel networking. Note that net.Pipe is unbuffered
// (writes rendezvous with reads), so absolute MB/s numbers are not comparable
// to TCP loopback — relative differences between benchmarks are what matter.

type memListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newMemListener() *memListener {
	return &memListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil

	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

type memAddr struct{}

func (memAddr) Network() string { return "mem" }
func (memAddr) String() string  { return "mem" }

func (l *memListener) Addr() net.Addr { return memAddr{} }

// dial hands the server side of a fresh pipe to Accept and returns the
// client side.
func (l *memListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil

	case <-l.closed:
		testClose(client)
		testClose(server)

		return nil, net.ErrClosed

	case <-ctx.Done():
		testClose(client)
		testClose(server)

		return nil, ctx.Err()
	}
}

const benchURL = "http://bench.mem/"

// benchClient serves handler over an in-memory listener and returns a client
// whose transport dials that listener directly.
func benchClient(b *testing.B, handler http.Handler) *http.Client {
	b.Helper()

	ln := newMemListener()

	srv := &http.Server{Handler: handler}
	go testServe(srv, ln)

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return ln.dial(ctx)
		},
		MaxIdleConns:        0, // unlimited: every connection stays reusable
		MaxIdleConnsPerHost: 4096,
	}

	b.Cleanup(func() {
		tr.CloseIdleConnections()
		testClose(srv)
		testClose(ln)
	})

	return &http.Client{Transport: tr}
}

func echoHandler(payload []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testCopy(io.Discard, r.Body)
		// Announce the length: large responses would otherwise go out
		// chunked, hiding the Content-Length-driven pre-allocation path
		// these benchmarks are meant to exercise.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		testWrite(w, payload)
	})
}

func benchDo(b *testing.B, client *http.Client) {
	resp, err := client.Get(benchURL)
	if err != nil {
		b.Fatalf("GET: %v", err)
	}

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		b.Fatalf("read: %v", err)
	}

	testClose(resp.Body)
}

var discardRecorder = RecorderFunc(func(*Entry) error { return nil })

// ---- benchmarks ----

// BenchmarkBaselineNoRecorder measures the bare net/http + in-memory pipe
// cost without any recorder wrapping. The delta between this and the other
// benchmarks is the recorder's true overhead.
func BenchmarkBaselineNoRecorder(b *testing.B) {
	client := benchClient(b, echoHandler(bytes.Repeat([]byte("x"), 1024)))
	b.SetBytes(1024)

	for b.Loop() {
		benchDo(b, client)
	}
}

func BenchmarkHeadSampleDrop(b *testing.B) {
	client := benchClient(b, echoHandler(bytes.Repeat([]byte("x"), 1024)))
	client.Transport = NewTransport(client.Transport, discardRecorder, configWith(withHeadSamplingPolicy(HeadSamplingPolicy(func(context.Context, HeadSamplingMeta) HeadSamplingDecision {
		return HeadSampleDrop
	}))))

	b.SetBytes(1024)
	b.ReportAllocs()

	for b.Loop() {
		benchDo(b, client)
	}
}

func BenchmarkCaptureDisabled(b *testing.B) {
	client := benchClient(b, echoHandler(bytes.Repeat([]byte("x"), 1024)))
	client.Transport = NewTransport(client.Transport, discardRecorder, Config{})

	b.SetBytes(1024)

	for b.Loop() {
		benchDo(b, client)
	}
}

func BenchmarkHeaderOnlyCapture(b *testing.B) {
	client := benchClient(b, echoHandler(bytes.Repeat([]byte("x"), 1024)))
	client.Transport = NewTransport(client.Transport, discardRecorder, configWith(withCaptureRequestBody(false),
		withCaptureResponseBody(false),
		withHashBodies(false, "")),
	)

	b.SetBytes(1024)

	for b.Loop() {
		benchDo(b, client)
	}
}

func BenchmarkSmallBody(b *testing.B) {
	client := benchClient(b, echoHandler(bytes.Repeat([]byte("x"), 1024)))
	client.Transport = NewTransport(client.Transport, discardRecorder, configWith(withCaptureResponseBody(true),
		withEmbedBodies(true),
		withHashBodies(true, "sha256")),
	)

	b.SetBytes(1024)

	for b.Loop() {
		benchDo(b, client)
	}
}

func Benchmark1MBBody(b *testing.B) {
	client := benchClient(b, echoHandler(bytes.Repeat([]byte("y"), 1<<20)))
	client.Transport = NewTransport(client.Transport, discardRecorder, configWith(withCaptureResponseBody(true),
		withEmbedBodies(true),
		withHashBodies(true, "sha256")),
	)

	b.SetBytes(1 << 20)

	for b.Loop() {
		benchDo(b, client)
	}
}

func streamingHandler(size int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)

		var written int64
		for written < size {
			n, err := w.Write(chunk)
			if err != nil {
				return
			}

			written += int64(n)
		}
	})
}

func Benchmark100MBStreamingBody(b *testing.B) {
	const size = 100 << 20

	client := benchClient(b, streamingHandler(size))
	// Capture is limited to 1 MiB (default): the remaining 99 MiB stream
	// through counting/hashing only. SHA-256 dominates here; compare with
	// Benchmark100MBStreamingBodyNoHash.
	client.Transport = NewTransport(client.Transport, discardRecorder, configWith(withCaptureResponseBody(true),
		withEmbedBodies(false),
		withHashBodies(true, "sha256")),
	)

	b.SetBytes(size)

	for b.Loop() {
		benchDo(b, client)
	}
}

func Benchmark100MBStreamingBodyNoHash(b *testing.B) {
	const size = 100 << 20

	client := benchClient(b, streamingHandler(size))
	// Same stream without body hashing: past the capture limit the tee is
	// reduced to pure byte counting.
	client.Transport = NewTransport(client.Transport, discardRecorder, configWith(withCaptureResponseBody(true),
		withEmbedBodies(false),
		withHashBodies(false, "")))

	b.SetBytes(size)

	for b.Loop() {
		benchDo(b, client)
	}
}

func Benchmark1000ConcurrentRequests(b *testing.B) {
	client := benchClient(b, echoHandler(bytes.Repeat([]byte("z"), 512)))
	client.Transport = NewTransport(client.Transport, discardRecorder, configWith(withCaptureResponseBody(true),
		withEmbedBodies(false),
		withHashBodies(true, "sha256")),
	)
	// RunParallel spawns SetParallelism * GOMAXPROCS goroutines; divide so
	// the total is ~1000 regardless of core count. b.Loop cannot be used
	// here: parallel benchmarks iterate through pb.Next, so the explicit
	// ResetTimer stays to exclude the setup above.
	parallelism := (1000 + runtime.GOMAXPROCS(0) - 1) / runtime.GOMAXPROCS(0)
	b.SetParallelism(parallelism)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			benchDo(b, client)
		}
	})
}
