package recorder

import (
	"context"
	"testing"
)

func BenchmarkAsyncRecorder(b *testing.B) {
	entry := &Entry{}

	b.Run("direct", func(b *testing.B) {
		sink := RecorderFunc(func(*Entry) {})

		b.ReportAllocs()

		for b.Loop() {
			sink.Record(entry)
		}
	})

	b.Run("bounded-block", func(b *testing.B) {
		async, err := NewAsyncRecorder(RecorderFunc(func(*Entry) {}),
			WithAsyncQueueCapacity(1024))
		if err != nil {
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			async.Record(entry)
		}

		b.StopTimer()

		if err := async.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	})

	b.Run("bounded-batch-64", func(b *testing.B) {
		sink := batchRecorderFunc(func([]*Entry) {})

		async, err := NewAsyncRecorder(sink,
			WithAsyncQueueCapacity(1024),
			WithAsyncBatchSize(64))
		if err != nil {
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			async.Record(entry)
		}

		b.StopTimer()

		if err := async.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	})

	b.Run("full-drop-newest", func(b *testing.B) {
		sink := newBenchmarkGatedRecorder()

		async, err := NewAsyncRecorder(sink,
			WithAsyncQueueCapacity(1),
			WithAsyncBackpressurePolicy(AsyncDropNewest))
		if err != nil {
			b.Fatal(err)
		}

		async.Record(entry)
		<-sink.started
		async.Record(entry)

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			async.Record(entry)
		}

		b.StopTimer()

		close(sink.release)

		if err := async.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	})
}

type batchRecorderFunc func([]*Entry)

func (f batchRecorderFunc) Record(entry *Entry) { f([]*Entry{entry}) }
func (f batchRecorderFunc) RecordBatch(entries []*Entry) {
	f(entries)
}

type benchmarkGatedRecorder struct {
	started chan struct{}
	release chan struct{}
}

func newBenchmarkGatedRecorder() *benchmarkGatedRecorder {
	return &benchmarkGatedRecorder{started: make(chan struct{}), release: make(chan struct{})}
}

func (r *benchmarkGatedRecorder) Record(*Entry) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}

	<-r.release
}
