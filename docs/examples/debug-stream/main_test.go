package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mgurevin/recorder"
)

func TestExampleRecordersPersistDeliveredEntries(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	stream, async, err := newExampleRecorders(&output)
	if err != nil {
		t.Fatalf("create example recorders: %v", err)
	}

	entry := &recorder.Entry{
		Request:  &recorder.Request{Method: "GET", URL: "https://example.test/"},
		Response: &recorder.Response{Status: 200},
	}
	if err := async.Record(entry); err != nil {
		t.Fatalf("record entry: %v", err)
	}

	if err := async.Close(context.Background()); err != nil {
		t.Fatalf("close async recorder: %v", err)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close debug stream: %v", err)
	}

	if encoded := output.String(); !strings.Contains(encoded, `"url":"https://example.test/"`) {
		t.Fatalf("NDJSON output does not contain recorded request: %q", encoded)
	}
}
