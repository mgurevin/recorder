package otelrecorder

import (
	"context"
	"net/http"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	recorder "github.com/mgurevin/recorder"
)

type samplingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f samplingRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSamplingMetrics(t *testing.T) {
	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusNoContent,
			Status:        "204 No Content",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          http.NoBody,
			ContentLength: 0,
			Request:       req,
		}, nil
	})

	transport := recorder.NewTransport(base, recorder.NewMemoryRecorder(),
		recorder.WithHeadSamplingPolicy(recorder.HeadSamplingPolicyFunc(func(_ context.Context, meta recorder.HeadSamplingMeta) recorder.HeadSamplingDecision {
			if meta.Path == "/drop" {
				return recorder.HeadSampleDrop
			}

			return recorder.HeadSampleFull
		})),
		recorder.WithRetentionPolicy(recorder.RetentionPolicyFunc(func(context.Context, *recorder.Entry) recorder.RetentionDecision {
			return recorder.DiscardEntry
		})),
	)

	for _, path := range []string{"/drop", "/discard"} {
		request, err := http.NewRequest(http.MethodGet, "https://example.test"+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := transport.RoundTrip(request); err != nil {
			t.Fatalf("RoundTrip(%s): %v", path, err)
		}
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	exporter, err := NewExporter(WithMeterProvider(mp), WithSamplingTransport(transport))
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
	head := metrics["recorder.sampling.head.decisions"].Data.(metricdata.Sum[int64])

	headValues := samplingDecisionValues(head, "recorder.sampling.decision")
	if headValues["drop"] != 1 || headValues["full"] != 1 || headValues["metadata_only"] != 0 {
		t.Errorf("head values = %+v", headValues)
	}

	retention := metrics["recorder.sampling.retention.outcomes"].Data.(metricdata.Sum[int64])

	retentionValues := samplingDecisionValues(retention, "recorder.sampling.decision")
	if retentionValues["discarded"] != 1 || retentionValues["retained"] != 0 {
		t.Errorf("retention values = %+v", retentionValues)
	}
}

func samplingDecisionValues(sum metricdata.Sum[int64], key string) map[string]int64 {
	values := make(map[string]int64, len(sum.DataPoints))
	for _, point := range sum.DataPoints {
		attrs := attrSetToMap(point.Attributes)
		values[attrs[key].AsString()] = point.Value
	}

	return values
}
