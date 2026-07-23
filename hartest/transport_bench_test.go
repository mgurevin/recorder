package hartest_test

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hartest"
)

const benchmarkReplayEntryCount = 256

func BenchmarkReplayExactRequests(b *testing.B) {
	entries := benchmarkReplayEntries(benchmarkReplayEntryCount)

	b.ReportAllocs()

	for b.Loop() {
		transport, err := hartest.NewTransport(entries, hartest.DefaultConfig())
		if err != nil {
			b.Fatal(err)
		}

		client := &http.Client{Transport: transport}

		for index := range entries {
			request, requestErr := http.NewRequest(
				http.MethodPost,
				"https://api.example.test/orders/"+strconv.Itoa(index),
				bytes.NewBufferString(`{"id":`+strconv.Itoa(index)+`}`),
			)
			if requestErr != nil {
				b.Fatal(requestErr)
			}

			request.Header.Set("Content-Type", "application/json")

			response, responseErr := client.Do(request)
			if responseErr != nil {
				b.Fatal(responseErr)
			}

			if _, readErr := io.Copy(io.Discard, response.Body); readErr != nil {
				b.Fatal(readErr)
			}

			if closeErr := response.Body.Close(); closeErr != nil {
				b.Fatal(closeErr)
			}
		}

		if err := transport.Verify(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkReplayEntries(count int) []*recorder.Entry {
	entries := make([]*recorder.Entry, count)
	for index := range entries {
		requestBody := `{"id":` + strconv.Itoa(index) + `}`
		entries[index] = &recorder.Entry{
			StartedDateTime: "2026-07-23T00:00:00Z",
			Time:            1,
			Request: &recorder.Request{
				Method:      http.MethodPost,
				URL:         "https://api.example.test/orders/" + strconv.Itoa(index),
				HTTPVersion: "HTTP/1.1",
				Headers: []recorder.NameValuePair{
					{Name: "Content-Type", Value: "application/json"},
				},
				HeadersSize: -1,
				PostData: &recorder.PostData{
					MimeType: "application/json",
					Text:     requestBody,
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
