package otelrecorder

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	recorder "github.com/mgurevin/recorder"
)

type asyncRecorderMetrics struct {
	registration metric.Registration
	closeOnce    sync.Once
	closeErr     error
}

func newAsyncRecorderMetrics(meter metric.Meter, asyncRecorder *recorder.AsyncRecorder) (*asyncRecorderMetrics, error) {
	queueDepth, err := meter.Int64ObservableGauge("recorder.async.queue.depth",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Entries waiting in the bounded asynchronous recorder queue"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async queue depth gauge: %w", err)
	}

	queueCapacity, err := meter.Int64ObservableGauge("recorder.async.queue.capacity",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Configured asynchronous recorder queue capacity"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async queue capacity gauge: %w", err)
	}

	blockedNow, err := meter.Int64ObservableGauge("recorder.async.producers.blocked",
		metric.WithUnit("{producer}"),
		metric.WithDescription("Producers currently blocked waiting for queue capacity"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async blocked producers gauge: %w", err)
	}

	inFlight, err := meter.Int64ObservableGauge("recorder.async.entries.in_flight",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Entries currently being processed by the downstream recorder"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async in-flight gauge: %w", err)
	}

	maxBlockTime, err := meter.Float64ObservableGauge("recorder.async.block.max",
		metric.WithUnit("ms"),
		metric.WithDescription("Longest observed producer block duration"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async maximum block time gauge: %w", err)
	}

	accepted, err := meter.Int64ObservableCounter("recorder.async.entries.accepted",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Entries accepted by the asynchronous recorder"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async accepted counter: %w", err)
	}

	processed, err := meter.Int64ObservableCounter("recorder.async.entries.processed",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Downstream Record calls attempted by the asynchronous recorder"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async processed counter: %w", err)
	}

	blocked, err := meter.Int64ObservableCounter("recorder.async.records.blocked",
		metric.WithUnit("{record}"),
		metric.WithDescription("Record calls that waited for asynchronous queue capacity"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async blocked records counter: %w", err)
	}

	blockTime, err := meter.Float64ObservableCounter("recorder.async.block.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Cumulative producer time spent waiting for queue capacity"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async block duration counter: %w", err)
	}

	dropped, err := meter.Int64ObservableCounter("recorder.async.entries.dropped",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Entries dropped by the asynchronous recorder, by bounded reason"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async dropped counter: %w", err)
	}

	sinkPanics, err := meter.Int64ObservableCounter("recorder.async.sink.panics",
		metric.WithUnit("{panic}"),
		metric.WithDescription("Panics recovered from the downstream recorder"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async sink panic counter: %w", err)
	}

	sinkErrors, err := meter.Int64ObservableCounter("recorder.async.sink.errors",
		metric.WithUnit("{error}"),
		metric.WithDescription("Observable downstream recorder and close errors"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create async sink error counter: %w", err)
	}

	instruments := []metric.Observable{
		queueDepth, queueCapacity, blockedNow, inFlight, maxBlockTime,
		accepted, processed, blocked, blockTime, dropped, sinkPanics, sinkErrors,
	}

	registration, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := asyncRecorder.Stats()
		observer.ObserveInt64(queueDepth, int64(stats.Pending))
		observer.ObserveInt64(queueCapacity, int64(stats.Capacity))
		observer.ObserveInt64(blockedNow, int64(stats.CurrentlyBlocked))
		observer.ObserveInt64(inFlight, int64(stats.InFlight))
		observer.ObserveFloat64(maxBlockTime, float64(stats.MaxBlockTime)/float64(time.Millisecond))
		observer.ObserveInt64(accepted, int64(stats.Accepted))
		observer.ObserveInt64(processed, int64(stats.Processed))
		observer.ObserveInt64(blocked, int64(stats.BlockedRecords))
		observer.ObserveFloat64(blockTime, float64(stats.TotalBlockTime)/float64(time.Millisecond))
		observer.ObserveInt64(dropped, int64(stats.DroppedNewest), metric.WithAttributes(attribute.String("recorder.async.drop.reason", "newest")))
		observer.ObserveInt64(dropped, int64(stats.DroppedOldest), metric.WithAttributes(attribute.String("recorder.async.drop.reason", "oldest")))
		observer.ObserveInt64(dropped, int64(stats.DroppedClosed), metric.WithAttributes(attribute.String("recorder.async.drop.reason", "closed")))
		observer.ObserveInt64(sinkPanics, int64(stats.SinkPanics))
		observer.ObserveInt64(sinkErrors, int64(stats.SinkErrors))

		return nil
	}, instruments...)
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: register async recorder metrics: %w", err)
	}

	return &asyncRecorderMetrics{registration: registration}, nil
}

func (m *asyncRecorderMetrics) close() error {
	m.closeOnce.Do(func() {
		m.closeErr = m.registration.Unregister()
	})

	return m.closeErr
}
