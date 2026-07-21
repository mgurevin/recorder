package otelrecorder

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	recorder "github.com/mgurevin/recorder"
)

type blockingRecorder struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingRecorder) Record(*recorder.Entry) {
	select {
	case r.started <- struct{}{}:
	default:
	}

	<-r.release
}

func TestAsyncRecorderMetrics(t *testing.T) {
	sink := &blockingRecorder{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	asyncRecorder, err := recorder.NewAsyncRecorder(sink,
		recorder.WithAsyncQueueCapacity(2),
		recorder.WithAsyncBackpressurePolicy(recorder.AsyncDropNewest))
	if err != nil {
		t.Fatalf("NewAsyncRecorder: %v", err)
	}

	asyncRecorder.Record(&recorder.Entry{})

	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("downstream recorder did not start")
	}

	asyncRecorder.Record(&recorder.Entry{})
	asyncRecorder.Record(&recorder.Entry{})
	asyncRecorder.Record(&recorder.Entry{})

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	exporter, err := NewExporter(
		WithMeterProvider(mp),
		WithAsyncRecorder(asyncRecorder),
	)
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}

	t.Cleanup(func() {
		close(sink.release)

		if err := asyncRecorder.Close(context.Background()); err != nil {
			t.Errorf("close async recorder: %v", err)
		}

		if err := exporter.Close(); err != nil {
			t.Errorf("close exporter: %v", err)
		}

		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})

	metrics := collectMetrics(t, reader)
	assertGaugeValue(t, metrics, "recorder.async.queue.capacity", 2)
	assertGaugeValue(t, metrics, "recorder.async.queue.depth", 2)
	assertGaugeValue(t, metrics, "recorder.async.entries.in_flight", 1)
	assertCounterValue(t, metrics, "recorder.async.entries.accepted", 3)

	drops := metrics["recorder.async.entries.dropped"].Data.(metricdata.Sum[int64])
	values := make(map[string]int64, len(drops.DataPoints))

	for _, point := range drops.DataPoints {
		attrs := attrSetToMap(point.Attributes)
		values[attrs["recorder.async.drop.reason"].AsString()] = point.Value
	}

	if values[string(recorder.AsyncDropPolicyNewest)] != 1 ||
		values[string(recorder.AsyncDropPolicyOldest)] != 0 ||
		values[string(recorder.AsyncDropTimeoutNewest)] != 0 ||
		values[string(recorder.AsyncDropTimeoutOldest)] != 0 ||
		values[string(recorder.AsyncDropClosed)] != 0 {
		t.Errorf("drop values = %+v", values)
	}
}

func TestExporterCloseUnregistersAsyncMetrics(t *testing.T) {
	asyncRecorder, err := recorder.NewAsyncRecorder(recorder.NewMemoryRecorder())
	if err != nil {
		t.Fatalf("NewAsyncRecorder: %v", err)
	}

	t.Cleanup(func() {
		if err := asyncRecorder.Close(context.Background()); err != nil {
			t.Errorf("close async recorder: %v", err)
		}
	})

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})

	exporter, err := NewExporter(WithMeterProvider(mp), WithAsyncRecorder(asyncRecorder))
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}

	if err := exporter.Close(); err != nil {
		t.Fatalf("close exporter: %v", err)
	}

	if err := exporter.Close(); err != nil {
		t.Fatalf("second close exporter: %v", err)
	}

	metrics := collectMetrics(t, reader)
	if _, ok := metrics["recorder.async.queue.depth"]; ok {
		t.Error("async queue metric remained registered after Close")
	}
}

func assertGaugeValue(t *testing.T, metrics map[string]metricdata.Metrics, name string, want int64) {
	t.Helper()

	gauge := metrics[name].Data.(metricdata.Gauge[int64])
	if len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != want {
		t.Errorf("%s = %+v, want %d", name, gauge.DataPoints, want)
	}
}

func assertCounterValue(t *testing.T, metrics map[string]metricdata.Metrics, name string, want int64) {
	t.Helper()

	sum := metrics[name].Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != want {
		t.Errorf("%s = %+v, want %d", name, sum.DataPoints, want)
	}
}
