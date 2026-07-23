package recorder_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/mgurevin/recorder"
)

func ExampleNewTransport() {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()

	records := recorder.NewMemoryRecorder()

	config := recorder.DefaultConfig()
	if err := config.Validate(); err != nil {
		panic(err)
	}

	client := &http.Client{
		Transport: recorder.NewTransport(http.DefaultTransport, records, config),
	}

	response, err := client.Get(server.URL)
	if err != nil {
		panic(err)
	}

	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		panic(err)
	}

	entry := records.Entries()[0]
	fmt.Println(entry.Response.Status, entry.Recorder.State, entry.Recorder.ResponseBody.TotalBytes)
	// Output: 200 completed 2
}

func ExampleConfig_redaction() {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"secret":"value","keep":1}`)
	}))
	defer server.Close()

	records := recorder.NewMemoryRecorder()
	config := recorder.DefaultConfig()
	config.CaptureResponseBody = true
	config.EmbedBodies = true

	config.Redaction.Common.JSONFields = []string{"secret"}
	if err := config.Validate(); err != nil {
		panic(err)
	}

	client := &http.Client{
		Transport: recorder.NewTransport(http.DefaultTransport, records, config),
	}

	response, err := client.Get(server.URL)
	if err != nil {
		panic(err)
	}

	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	fmt.Println(records.Entries()[0].Response.Content.Text)
	// Output: {"secret":"[REDACTED]","keep":1}
}

func ExampleConfig_Validate() {
	config := recorder.DefaultConfig()
	config.CaptureTLS = false

	fmt.Println(config.Validate())
	// Output: recorder: Config.CaptureCertificates requires CaptureTLS
}

func ExampleNewAsyncRecorder() {
	sink := recorder.NewMemoryRecorder()

	async, err := recorder.NewAsyncRecorder(sink, recorder.DefaultAsyncRecorderConfig())
	if err != nil {
		panic(err)
	}

	if err := async.Record(&recorder.Entry{}); err != nil {
		panic(err)
	}

	if err := async.Close(context.Background()); err != nil {
		panic(err)
	}

	fmt.Println(sink.Len())
	// Output: 1
}
