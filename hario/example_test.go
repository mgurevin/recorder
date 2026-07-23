package hario_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
)

func ExampleReadHAR() {
	document := recorder.NewHAR([]*recorder.Entry{exampleEntry()})

	var encoded bytes.Buffer
	if err := document.Write(&encoded); err != nil {
		panic(err)
	}

	decoded, err := hario.ReadHAR(&encoded, hario.DefaultReadConfig())
	if err != nil {
		panic(err)
	}

	fmt.Println(len(decoded.Log.Entries), decoded.Log.Entries[0].Request.Method)
	// Output: 1 GET
}

func ExampleNewNDJSONStream() {
	document := recorder.NewHAR([]*recorder.Entry{exampleEntry(), exampleEntry()})

	var encoded bytes.Buffer

	encoder := json.NewEncoder(&encoded)
	for _, entry := range document.Log.Entries {
		if err := encoder.Encode(entry); err != nil {
			panic(err)
		}
	}

	stream, err := hario.NewNDJSONStream(&encoded, hario.DefaultReadConfig())
	if err != nil {
		panic(err)
	}

	count := 0

	for {
		_, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			panic(err)
		}

		count++
	}

	fmt.Println(count)
	// Output: 2
}

func exampleEntry() *recorder.Entry {
	return &recorder.Entry{
		StartedDateTime: "2026-07-23T00:00:00Z",
		Time:            1,
		Request: &recorder.Request{
			Method:      "GET",
			URL:         "https://example.test/",
			HTTPVersion: "HTTP/1.1",
			HeadersSize: -1,
		},
		Response: &recorder.Response{
			Status:      http.StatusOK,
			StatusText:  "OK",
			HTTPVersion: "HTTP/1.1",
			Content:     &recorder.Content{},
			HeadersSize: -1,
		},
		Cache:   &recorder.Cache{},
		Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, SSL: -1},
	}
}
