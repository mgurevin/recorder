package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mgurevin/recorder"
)

func TestRunReplaysFixtureWithoutNetwork(t *testing.T) {
	t.Parallel()

	entry := &recorder.Entry{
		StartedDateTime: "2026-07-23T00:00:00Z",
		Time:            1,
		Request: &recorder.Request{
			Method:      "GET",
			URL:         "https://api.example.test/orders/42",
			HTTPVersion: "HTTP/1.1",
			HeadersSize: -1,
			BodySize:    -1,
		},
		Response: &recorder.Response{
			Status:      200,
			StatusText:  "OK",
			HTTPVersion: "HTTP/1.1",
			Content: &recorder.Content{
				Size:     2,
				MimeType: "text/plain",
				Text:     "ok",
			},
			HeadersSize: -1,
			BodySize:    2,
		},
		Cache:   &recorder.Cache{},
		Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, SSL: -1},
	}
	path := filepath.Join(t.TempDir(), "capture.har")

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}

	if err := recorder.NewHAR([]*recorder.Entry{entry}).Write(file); err != nil {
		_ = file.Close()

		t.Fatalf("write fixture: %v", err)
	}

	if err := file.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	var output bytes.Buffer
	if err := run(&output, path); err != nil {
		t.Fatalf("run fixture example: %v", err)
	}

	if got, want := output.String(), "200 OK\nok\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}

	if err := run(failingWriter{}, path); !errors.Is(err, errExampleWrite) {
		t.Fatalf("write failure = %v, want %v", err, errExampleWrite)
	}
}

var errExampleWrite = errors.New("example write failure")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errExampleWrite
}
