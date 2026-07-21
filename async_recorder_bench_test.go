package recorder

import (
	"context"
	"testing"
	"time"
)

func BenchmarkAsyncRecorder(b *testing.B) {
	entry := &Entry{}

	b.Run("direct", func(b *testing.B) {
		sink := RecorderFunc(func(*Entry) error { return nil })

		b.ReportAllocs()

		for b.Loop() {
			_ = sink.Record(entry)
		}
	})

	b.Run("bounded-block", func(b *testing.B) {
		async, err := newAsyncRecorderForTest(RecorderFunc(func(*Entry) error { return nil }),
			withAsyncQueueCapacity(1024))
		if err != nil {
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			_ = async.Record(entry)
		}

		b.StopTimer()

		if err := async.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	})

	b.Run("bounded-batch-64", func(b *testing.B) {
		sink := batchRecorderFunc(func([]*Entry) {})

		async, err := newAsyncRecorderForTest(sink,
			withAsyncQueueCapacity(1024),
			withAsyncBatchSize(64))
		if err != nil {
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			_ = async.Record(entry)
		}

		b.StopTimer()

		if err := async.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	})

	b.Run("full-drop-newest", func(b *testing.B) {
		sink := newBenchmarkGatedRecorder()

		async, err := newAsyncRecorderForTest(sink,
			withAsyncQueueCapacity(1),
			withAsyncBackpressurePolicy(AsyncDropNewest))
		if err != nil {
			b.Fatal(err)
		}

		_ = async.Record(entry)

		<-sink.started

		_ = async.Record(entry)

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			_ = async.Record(entry)
		}

		b.StopTimer()

		close(sink.release)

		if err := async.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	})

	b.Run("full-block-timeout-drop-newest", func(b *testing.B) {
		sink := newBenchmarkGatedRecorder()

		async, err := newAsyncRecorderForTest(sink,
			withAsyncQueueCapacity(1),
			withAsyncBlockTimeout(time.Nanosecond, AsyncDropNewest))
		if err != nil {
			b.Fatal(err)
		}

		_ = async.Record(entry)

		<-sink.started

		_ = async.Record(entry)

		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			_ = async.Record(entry)
		}

		b.StopTimer()
		close(sink.release)

		if err := async.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	})
}

type batchRecorderFunc func([]*Entry)

func (f batchRecorderFunc) Record(entry *Entry) error {
	f([]*Entry{entry})

	return nil
}

func (f batchRecorderFunc) RecordBatch(entries []*Entry) error {
	f(entries)

	return nil
}

type benchmarkGatedRecorder struct {
	started chan struct{}
	release chan struct{}
}

func newBenchmarkGatedRecorder() *benchmarkGatedRecorder {
	return &benchmarkGatedRecorder{started: make(chan struct{}), release: make(chan struct{})}
}

func (r *benchmarkGatedRecorder) Record(*Entry) error {
	select {
	case <-r.started:
	default:
		close(r.started)
	}

	<-r.release

	return nil
}
