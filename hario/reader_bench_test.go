package hario_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
)

const benchmarkEntryCount = 1_000

func BenchmarkFixtureReaders(b *testing.B) {
	entries := benchmarkEntries(benchmarkEntryCount)
	harData := benchmarkHAR(b, entries)
	ndjsonData := benchmarkNDJSON(b, entries)
	config := hario.DefaultReadConfig()

	for _, benchmark := range []struct {
		name string
		data []byte
		read func([]byte) error
	}{
		{
			name: "HAR/collect",
			data: harData,
			read: func(data []byte) error {
				_, err := hario.ReadHAR(bytes.NewReader(data), config)

				return err
			},
		},
		{
			name: "HAR/stream",
			data: harData,
			read: func(data []byte) error {
				stream, err := hario.NewHARStream(bytes.NewReader(data), config)
				if err != nil {
					return err
				}

				return consumeStream(stream)
			},
		},
		{
			name: "NDJSON/collect",
			data: ndjsonData,
			read: func(data []byte) error {
				_, err := hario.ReadNDJSON(bytes.NewReader(data), config)

				return err
			},
		},
		{
			name: "NDJSON/stream",
			data: ndjsonData,
			read: func(data []byte) error {
				stream, err := hario.NewNDJSONStream(bytes.NewReader(data), config)
				if err != nil {
					return err
				}

				return consumeStream(stream)
			},
		},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(benchmark.data)))

			for b.Loop() {
				if err := benchmark.read(benchmark.data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func consumeStream(stream *hario.EntryStream) error {
	for {
		_, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}
	}
}

func benchmarkEntries(count int) []*recorder.Entry {
	entries := make([]*recorder.Entry, count)
	for index := range entries {
		entries[index] = &recorder.Entry{
			StartedDateTime: "2026-07-23T00:00:00Z",
			Time:            1,
			Request: &recorder.Request{
				Method:      http.MethodPost,
				URL:         "https://api.example.test/orders",
				HTTPVersion: "HTTP/1.1",
				HeadersSize: -1,
				PostData: &recorder.PostData{
					MimeType: "application/json",
					Text:     `{"id":42}`,
				},
			},
			Response: &recorder.Response{
				Status:      http.StatusOK,
				StatusText:  "OK",
				HTTPVersion: "HTTP/1.1",
				Content: &recorder.Content{
					Size:     11,
					MimeType: "application/json",
					Text:     `{"ok":true}`,
				},
				HeadersSize: -1,
			},
			Cache:   &recorder.Cache{},
			Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, SSL: -1},
		}
	}

	return entries
}

func benchmarkHAR(b *testing.B, entries []*recorder.Entry) []byte {
	b.Helper()

	data, err := json.Marshal(recorder.NewHAR(entries))
	if err != nil {
		b.Fatal(err)
	}

	return data
}

func benchmarkNDJSON(b *testing.B, entries []*recorder.Entry) []byte {
	b.Helper()

	var data bytes.Buffer

	encoder := json.NewEncoder(&data)

	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			b.Fatal(err)
		}
	}

	return data.Bytes()
}
