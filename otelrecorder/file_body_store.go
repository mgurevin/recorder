package otelrecorder

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	recorder "github.com/mgurevin/recorder"
)

type fileBodyStoreMetrics struct {
	registration metric.Registration
	closeOnce    sync.Once
	closeErr     error
}

func newFileBodyStoreMetrics(meter metric.Meter, store *recorder.FileBodyStore) (*fileBodyStoreMetrics, error) {
	files, err := meter.Int64ObservableGauge("recorder.body.store.files",
		metric.WithUnit("{file}"),
		metric.WithDescription("Body files owned by the managed store, by bounded state"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create body store files gauge: %w", err)
	}

	bytesUsed, err := meter.Int64ObservableGauge("recorder.body.store.bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Body bytes owned by the managed store, by bounded state"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create body store bytes gauge: %w", err)
	}

	maxFiles, err := meter.Int64ObservableGauge("recorder.body.store.capacity.files",
		metric.WithUnit("{file}"),
		metric.WithDescription("Configured managed body store file capacity"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create body store file capacity gauge: %w", err)
	}

	maxBytes, err := meter.Int64ObservableGauge("recorder.body.store.capacity.bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Configured managed body store byte capacity"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create body store byte capacity gauge: %w", err)
	}

	operations, err := meter.Int64ObservableCounter("recorder.body.store.operations",
		metric.WithUnit("{operation}"),
		metric.WithDescription("Managed body store lifecycle outcomes"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create body store operation counter: %w", err)
	}

	stateKey := attribute.Key("recorder.body.store.state")
	outcomeKey := attribute.Key("recorder.body.store.outcome")

	registration, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := store.Stats()
		observer.ObserveInt64(files, int64(stats.PartialFiles), metric.WithAttributes(stateKey.String("partial")))
		observer.ObserveInt64(files, int64(stats.CommittedFiles), metric.WithAttributes(stateKey.String("committed")))
		observer.ObserveInt64(bytesUsed, stats.PartialBytes, metric.WithAttributes(stateKey.String("partial")))
		observer.ObserveInt64(bytesUsed, stats.CommittedBytes, metric.WithAttributes(stateKey.String("committed")))
		observer.ObserveInt64(maxFiles, int64(stats.MaxFiles))
		observer.ObserveInt64(maxBytes, stats.MaxBytes)
		observer.ObserveInt64(operations, int64(stats.CommittedTotal), metric.WithAttributes(outcomeKey.String("committed")))
		observer.ObserveInt64(operations, int64(stats.AbortedTotal), metric.WithAttributes(outcomeKey.String("aborted")))
		observer.ObserveInt64(operations, int64(stats.ReleasedTotal), metric.WithAttributes(outcomeKey.String("released")))
		observer.ObserveInt64(operations, int64(stats.QuotaRejected), metric.WithAttributes(outcomeKey.String("quota_rejected")))
		observer.ObserveInt64(operations, int64(stats.RecoveredPartials), metric.WithAttributes(outcomeKey.String("recovered_partial")))
		observer.ObserveInt64(operations, int64(stats.WriteFailures), metric.WithAttributes(outcomeKey.String("write_failed")))
		observer.ObserveInt64(operations, int64(stats.CommitFailures), metric.WithAttributes(outcomeKey.String("commit_failed")))
		observer.ObserveInt64(operations, int64(stats.AbortFailures), metric.WithAttributes(outcomeKey.String("abort_failed")))
		observer.ObserveInt64(operations, int64(stats.ReleaseFailures), metric.WithAttributes(outcomeKey.String("release_failed")))

		return nil
	}, files, bytesUsed, maxFiles, maxBytes, operations)
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: register body store metrics: %w", err)
	}

	return &fileBodyStoreMetrics{registration: registration}, nil
}

func (m *fileBodyStoreMetrics) close() error {
	m.closeOnce.Do(func() {
		m.closeErr = m.registration.Unregister()
	})

	return m.closeErr
}
