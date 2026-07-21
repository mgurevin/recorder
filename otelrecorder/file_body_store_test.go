package otelrecorder

import (
	"context"
	"io"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	recorder "github.com/mgurevin/recorder"
)

func TestFileBodyStoreMetrics(t *testing.T) {
	config := recorder.DefaultFileBodyStoreConfig()
	config.MaxBytes = 1024
	config.MaxFiles = 4

	store, err := recorder.NewFileBodyStore(t.TempDir(), config)
	if err != nil {
		t.Fatalf("NewFileBodyStore: %v", err)
	}

	w, err := store.NewWriter(context.Background(), recorder.BodyMetadata{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if _, err := io.WriteString(w, "body"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	exporter, err := newExporterForTest(withMeterProvider(mp), withFileBodyStore(store))
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}

	t.Cleanup(func() {
		if err := exporter.Close(); err != nil {
			t.Errorf("close exporter: %v", err)
		}

		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})

	metrics := collectMetrics(t, reader)
	assertGaugeValue(t, metrics, "recorder.body.store.capacity.files", 4)
	assertGaugeValue(t, metrics, "recorder.body.store.capacity.bytes", 1024)

	files := metrics["recorder.body.store.files"].Data.(metricdata.Gauge[int64])

	states := make(map[string]int64, len(files.DataPoints))
	for _, point := range files.DataPoints {
		attrs := attrSetToMap(point.Attributes)
		states[attrs["recorder.body.store.state"].AsString()] = point.Value
	}

	if states["partial"] != 0 || states["committed"] != 1 {
		t.Errorf("file states = %+v", states)
	}

	operations := metrics["recorder.body.store.operations"].Data.(metricdata.Sum[int64])

	outcomes := make(map[string]int64, len(operations.DataPoints))
	for _, point := range operations.DataPoints {
		attrs := attrSetToMap(point.Attributes)
		outcomes[attrs["recorder.body.store.outcome"].AsString()] = point.Value
	}

	if outcomes["committed"] != 1 {
		t.Errorf("operation outcomes = %+v", outcomes)
	}
}
