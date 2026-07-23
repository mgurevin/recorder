package otelrecorder_test

import (
	"fmt"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mgurevin/recorder/otelrecorder"
)

func ExampleNewExporter() {
	config := otelrecorder.DefaultConfig()
	config.TracerProvider = tracenoop.NewTracerProvider()
	config.MeterProvider = metricnoop.NewMeterProvider()

	exporter, err := otelrecorder.NewExporter(config)
	if err != nil {
		panic(err)
	}

	if err := exporter.Close(); err != nil {
		panic(err)
	}

	fmt.Println(otelrecorder.EventName)
	// Output: recorder.http.exchange
}
