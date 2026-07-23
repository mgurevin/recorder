package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/mgurevin/recorder"
)

func TestRecordedBrotliAndZstandardResponses(t *testing.T) {
	const body = `{"password":"secret","keep":true}`

	tests := []struct {
		name     string
		encoding string
		compress func(*testing.T, []byte) []byte
	}{
		{name: "brotli", encoding: "br", compress: compressBrotli},
		{name: "zstandard", encoding: "zstd", compress: compressZstandard},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire := tt.compress(t, []byte(body))

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", tt.encoding)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)

				if _, err := w.Write(wire); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer server.Close()

			record := recorder.NewMemoryRecorder()
			config := recorder.DefaultConfig()
			config.CaptureResponseBody = true
			config.EmbedBodies = true
			config.HashBodies = true

			config.Redaction = recorder.RedactionConfig{Common: recorder.RedactionRules{JSONFields: []string{"password"}}}
			for name, decoder := range Decoders() {
				config.ContentDecoders[name] = decoder
			}

			if err := config.Validate(); err != nil {
				t.Fatalf("validate recorder config: %v", err)
			}

			client := &http.Client{
				Transport: recorder.NewTransport(http.DefaultTransport, record, config),
			}

			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatalf("get compressed response: %v", err)
			}

			callerBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}

			if err := response.Body.Close(); err != nil {
				t.Fatalf("close response: %v", err)
			}

			if !bytes.Equal(callerBody, wire) {
				t.Fatal("recorder changed the caller-visible compressed bytes")
			}

			entries := record.Entries()
			if len(entries) != 1 {
				t.Fatalf("recorded entries = %d, want 1", len(entries))
			}

			entry := entries[0]

			content := entry.Response.Content
			if !entry.Recorder.ResponseBodyDecoded {
				t.Fatal("recorded content is not marked decoded")
			}

			if got, want := content.Text, `{"password":"[REDACTED]","keep":true}`; got != want {
				t.Fatalf("recorded content = %q, want %q", got, want)
			}

			if entry.Response.BodySize != int64(len(wire)) {
				t.Fatalf("wire body size = %d, want %d", entry.Response.BodySize, len(wire))
			}

			sum := sha256.Sum256(wire)
			if got, want := entry.Recorder.ResponseBody.Hash, hex.EncodeToString(sum[:]); got != want {
				t.Fatalf("wire hash = %q, want %q", got, want)
			}

			if got, want := entry.Recorder.ResponseBody.TotalBytes, int64(len(wire)); got != want {
				t.Fatalf("wire byte count = %d, want %d", got, want)
			}
		})
	}
}

func compressBrotli(t *testing.T, src []byte) []byte {
	t.Helper()

	var dst bytes.Buffer

	writer := brotli.NewWriter(&dst)

	if _, err := writer.Write(src); err != nil {
		t.Fatalf("write Brotli stream: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close Brotli stream: %v", err)
	}

	return dst.Bytes()
}

func compressZstandard(t *testing.T, src []byte) []byte {
	t.Helper()

	var dst bytes.Buffer

	writer, err := zstd.NewWriter(&dst)
	if err != nil {
		t.Fatalf("open Zstandard encoder: %v", err)
	}

	if _, err := writer.Write(src); err != nil {
		t.Fatalf("write Zstandard stream: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close Zstandard stream: %v", err)
	}

	return dst.Bytes()
}
