// Command debug-stream demonstrates ephemeral, single-browser live inspection
// during local development. It is deliberately not a production evidence sink.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mgurevin/recorder"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	output, err := os.Create("debug-entries.ndjson")
	if err != nil {
		log.Fatal(err)
	}

	stream, async, err := newExampleRecorders(output)
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              "127.0.0.1:7070",
		Handler:           stream,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)

	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	config := recorder.DefaultConfig()
	if err := config.Validate(); err != nil {
		log.Fatal(err)
	}

	client := &http.Client{
		Transport: recorder.NewTransport(http.DefaultTransport, async, config),
	}

	log.Printf("open the Inspector, choose live, and connect to http://127.0.0.1:7070")

	// This request stands in for application traffic. The exchange appears
	// after the response body reaches EOF or is closed. The bounded debug queue
	// keeps it available when the Inspector connects after this request.
	response, err := client.Get("https://example.com/")
	if err != nil {
		log.Printf("example request: %v", err)
	} else {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}

	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("debug server: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shut down debug server: %v", err)
	}

	if err := async.Close(shutdownCtx); err != nil {
		log.Printf("close async recorder: %v", err)
	}

	if err := output.Sync(); err != nil {
		log.Printf("sync NDJSON output: %v", err)
	}

	if err := output.Close(); err != nil {
		log.Printf("close NDJSON output: %v", err)
	}

	if err := stream.Close(); err != nil {
		log.Printf("close debug stream: %v", err)
	}
}

func newExampleRecorders(output io.Writer) (*recorder.DebugStreamRecorder, *recorder.AsyncRecorder, error) {
	stream, err := recorder.NewDebugStreamRecorder(recorder.DefaultDebugStreamRecorderConfig())
	if err != nil {
		return nil, nil, fmt.Errorf("create debug stream recorder: %w", err)
	}

	fileSink := recorder.NewJSONStreamRecorder(output)
	fanout := recorder.NewMultiRecorder(fileSink, stream)

	asyncConfig := recorder.DefaultAsyncRecorderConfig()
	asyncConfig.QueueCapacity = 256
	asyncConfig.BatchSize = 32
	asyncConfig.FlushInterval = 50 * time.Millisecond

	async, err := recorder.NewAsyncRecorder(fanout, asyncConfig)
	if err != nil {
		_ = stream.Close()

		return nil, nil, fmt.Errorf("create async recorder: %w", err)
	}

	return stream, async, nil
}
