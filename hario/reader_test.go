package hario_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
)

func validEntry() *recorder.Entry {
	return &recorder.Entry{
		StartedDateTime: "2026-07-23T00:00:00.000Z",
		Time:            1,
		Request: &recorder.Request{
			Method:      "GET",
			URL:         "https://example.com/",
			HTTPVersion: "HTTP/1.1",
			HeadersSize: -1,
		},
		Response: &recorder.Response{
			Status:      200,
			StatusText:  "OK",
			HTTPVersion: "HTTP/1.1",
			Content:     &recorder.Content{Size: 0},
			HeadersSize: -1,
		},
		Cache:   &recorder.Cache{},
		Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, Send: 0, Wait: 1, Receive: 0, SSL: -1},
	}
}

func TestReadHAR(t *testing.T) {
	t.Parallel()

	document := recorder.NewHAR([]*recorder.Entry{validEntry()})

	var encoded bytes.Buffer
	if err := document.Write(&encoded); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := hario.ReadHAR(&encoded, hario.DefaultReadConfig())
	if err != nil {
		t.Fatalf("ReadHAR: %v", err)
	}

	if len(got.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(got.Log.Entries))
	}
}

func TestReadHARRejectsTrailingDocumentAndLimits(t *testing.T) {
	t.Parallel()

	config := hario.DefaultReadConfig()
	config.MaxBytes = 8
	config.MaxEntryBytes = 8

	if _, err := hario.ReadHAR(strings.NewReader(`{"log":{}}`), config); !errors.Is(err, hario.ErrLimitExceeded) {
		t.Fatalf("limit error = %v", err)
	}

	config = hario.DefaultReadConfig()
	if _, err := hario.ReadHAR(strings.NewReader(`{"log":{}} {}`), config); !errors.Is(err, hario.ErrInvalidHAR) {
		t.Fatalf("trailing error = %v", err)
	}

	document := recorder.NewHAR([]*recorder.Entry{validEntry()})

	var encoded bytes.Buffer
	if err := document.Write(&encoded); err != nil {
		t.Fatal(err)
	}

	config = hario.DefaultReadConfig()

	config.MaxEntryBytes = 1
	if _, err := hario.ReadHAR(&encoded, config); !errors.Is(err, hario.ErrLimitExceeded) {
		t.Fatalf("entry limit error = %v", err)
	}
}

func TestReadNDJSON(t *testing.T) {
	t.Parallel()

	first, err := json.Marshal(validEntry())
	if err != nil {
		t.Fatal(err)
	}

	second, err := json.Marshal(validEntry())
	if err != nil {
		t.Fatal(err)
	}

	input := append([]byte{0xef, 0xbb, 0xbf}, first...)
	input = append(input, '\n', '\n')
	input = append(input, second...)

	entries, err := hario.ReadNDJSON(bytes.NewReader(input), hario.DefaultReadConfig())
	if err != nil {
		t.Fatalf("ReadNDJSON: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

func TestReadNDJSONRejectsBadLineWithoutPartialResult(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validEntry())
	if err != nil {
		t.Fatal(err)
	}

	input := append(encoded, '\n')
	input = append(input, "{bad}"...)

	entries, err := hario.ReadNDJSON(bytes.NewReader(input), hario.DefaultReadConfig())
	if !errors.Is(err, hario.ErrInvalidHAR) {
		t.Fatalf("error = %v", err)
	}

	if entries != nil {
		t.Fatalf("partial entries = %#v", entries)
	}
}

func TestReadNDJSONLimits(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validEntry())
	if err != nil {
		t.Fatal(err)
	}

	config := hario.DefaultReadConfig()
	config.MaxEntryBytes = int64(len(encoded) - 1)

	if _, err := hario.ReadNDJSON(bytes.NewReader(encoded), config); !errors.Is(err, hario.ErrLimitExceeded) {
		t.Fatalf("entry limit error = %v", err)
	}

	config = hario.DefaultReadConfig()
	config.MaxEntries = 1

	input := append(append(append([]byte{}, encoded...), '\n'), encoded...)
	if _, err := hario.ReadNDJSON(bytes.NewReader(input), config); !errors.Is(err, hario.ErrLimitExceeded) {
		t.Fatalf("entry count error = %v", err)
	}
}

func TestValidateEntriesReportsPath(t *testing.T) {
	t.Parallel()

	entry := validEntry()
	entry.Response.Content = nil

	err := hario.ValidateEntries([]*recorder.Entry{entry})
	if !errors.Is(err, hario.ErrInvalidHAR) || !strings.Contains(err.Error(), "entry 0.response.content") {
		t.Fatalf("error = %v", err)
	}
}

func FuzzReadNDJSON(f *testing.F) {
	encoded, err := json.Marshal(validEntry())
	if err != nil {
		f.Fatal(err)
	}

	f.Add(encoded)
	f.Add([]byte("{bad}\n"))

	f.Fuzz(func(t *testing.T, input []byte) {
		config := hario.DefaultReadConfig()
		config.MaxBytes = 1 << 20
		config.MaxEntryBytes = 1 << 20
		config.MaxEntries = 1_000

		_, _ = hario.ReadNDJSON(bytes.NewReader(input), config)
	})
}
