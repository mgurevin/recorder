package otelrecorder

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	recorder "github.com/mgurevin/recorder"
)

type samplingMetrics struct {
	registration metric.Registration
	closeOnce    sync.Once
	closeErr     error
}

func newSamplingMetrics(meter metric.Meter, transport *recorder.Transport) (*samplingMetrics, error) {
	head, err := meter.Int64ObservableCounter("recorder.sampling.head.decisions",
		metric.WithUnit("{decision}"),
		metric.WithDescription("Head sampling outcomes by bounded decision"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create head sampling decision counter: %w", err)
	}

	retention, err := meter.Int64ObservableCounter("recorder.sampling.retention.outcomes",
		metric.WithUnit("{decision}"),
		metric.WithDescription("Finalized-entry retention outcomes after managed-asset cleanup"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create retention decision counter: %w", err)
	}

	policyFailures, err := meter.Int64ObservableCounter("recorder.sampling.policy.failures",
		metric.WithUnit("{failure}"),
		metric.WithDescription("Sampling policy panics and invalid decisions by bounded stage and reason"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create sampling policy failure counter: %w", err)
	}

	assetReleaseFailures, err := meter.Int64ObservableCounter("recorder.sampling.asset_release.failures",
		metric.WithUnit("{failure}"),
		metric.WithDescription("Failures releasing managed assets for discarded entries"))
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: create sampling asset release failure counter: %w", err)
	}

	decisionKey := attribute.Key("recorder.sampling.decision")
	stageKey := attribute.Key("recorder.sampling.stage")
	reasonKey := attribute.Key("recorder.sampling.failure.reason")

	registration, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := transport.SamplingStats()
		observer.ObserveInt64(head, int64(stats.HeadFull), metric.WithAttributes(decisionKey.String("full")))
		observer.ObserveInt64(head, int64(stats.HeadMetadataOnly), metric.WithAttributes(decisionKey.String("metadata_only")))
		observer.ObserveInt64(head, int64(stats.HeadDropped), metric.WithAttributes(decisionKey.String("drop")))
		observer.ObserveInt64(retention, int64(stats.Retained), metric.WithAttributes(decisionKey.String("retained")))
		observer.ObserveInt64(retention, int64(stats.Discarded), metric.WithAttributes(decisionKey.String("discarded")))
		observer.ObserveInt64(policyFailures, int64(stats.HeadPanics), metric.WithAttributes(stageKey.String("head"), reasonKey.String("panic")))
		observer.ObserveInt64(policyFailures, int64(stats.HeadInvalidDecisions), metric.WithAttributes(stageKey.String("head"), reasonKey.String("invalid_decision")))
		observer.ObserveInt64(policyFailures, int64(stats.RetentionPanics), metric.WithAttributes(stageKey.String("retention"), reasonKey.String("panic")))
		observer.ObserveInt64(policyFailures, int64(stats.RetentionInvalidDecisions), metric.WithAttributes(stageKey.String("retention"), reasonKey.String("invalid_decision")))
		observer.ObserveInt64(assetReleaseFailures, int64(stats.AssetReleaseFailures))

		return nil
	}, head, retention, policyFailures, assetReleaseFailures)
	if err != nil {
		return nil, fmt.Errorf("otelrecorder: register sampling metrics: %w", err)
	}

	return &samplingMetrics{registration: registration}, nil
}

func (m *samplingMetrics) close() error {
	m.closeOnce.Do(func() {
		m.closeErr = m.registration.Unregister()
	})

	return m.closeErr
}
