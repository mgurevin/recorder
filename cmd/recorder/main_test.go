package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mgurevin/recorder"
)

func TestValidateAndSummarizeHAR(t *testing.T) {
	path := writeCapture(t, "capture.har", testHAR(t))

	var output bytes.Buffer
	if err := run(context.Background(), []string{"validate", path}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}

	if got := output.String(); got != "valid HAR capture: 2 entries\n" {
		t.Fatalf("validate output = %q", got)
	}

	output.Reset()

	if err := run(
		context.Background(),
		[]string{"summarize", "--json", path},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	var summary captureSummary
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}

	if summary.Entries != 2 || summary.Failures != 1 || summary.Traces != 1 {
		t.Fatalf("summary = %#v", summary)
	}

	if summary.Methods["POST"] != 2 || summary.StatusClasses["2xx"] != 1 {
		t.Fatalf("summary counts = %#v", summary)
	}

	output.Reset()

	if err := run(
		context.Background(),
		[]string{"summarize", path},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"Format: HAR",
		"Entries: 2",
		"Methods: POST=2",
		"Status classes: 2xx=1 transport-error=1",
		"Hosts: api.example.com=2",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("summary output %q does not contain %q", output.String(), want)
		}
	}
}

func TestConvertRoundTrip(t *testing.T) {
	harPath := writeCapture(t, "capture.har", testHAR(t))
	ndjsonPath := filepath.Join(t.TempDir(), "capture.ndjson")

	if err := run(
		context.Background(),
		[]string{"convert", "--to", "ndjson", "--output", ndjsonPath, harPath},
		io.Discard,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"validate", "--json", ndjsonPath},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), `"entries": 2`) {
		t.Fatalf("validate JSON = %s", output.String())
	}

	roundTripPath := filepath.Join(t.TempDir(), "round-trip.har")
	if err := run(
		context.Background(),
		[]string{"convert", "--to", "har", "--output", roundTripPath, ndjsonPath},
		io.Discard,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if err := run(
		context.Background(),
		[]string{"validate", roundTripPath},
		io.Discard,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"convert", "--to", "ndjson", harPath},
		&stdout,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if got := bytes.Count(stdout.Bytes(), []byte{'\n'}); got != 2 {
		t.Fatalf("NDJSON lines = %d", got)
	}
}

func TestCommandMetadataAndUsage(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), "validate") {
		t.Fatalf("help output = %q", output.String())
	}

	output.Reset()

	if err := run(context.Background(), []string{"version"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(output.String(), "recorder ") {
		t.Fatalf("version output = %q", output.String())
	}

	if err := run(context.Background(), []string{"unknown"}, io.Discard, io.Discard); !errors.Is(err, errUsage) {
		t.Fatalf("unknown command error = %v", err)
	}

	if err := run(context.Background(), nil, io.Discard, io.Discard); !errors.Is(err, errUsage) {
		t.Fatalf("empty command error = %v", err)
	}
}

func TestRejectsInvalidCommandArguments(t *testing.T) {
	path := writeCapture(t, "capture.har", testHAR(t))

	for _, args := range [][]string{
		{"validate"},
		{"validate", "--format", "yaml", path},
		{"convert", "--to", "yaml", path},
		{"convert", "--to", "har", path},
		{"inspect", "-"},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("run(%q) error = %v", args, err)
		}
	}
}

func TestCaptureHandlerRestrictsOriginAndMethods(t *testing.T) {
	path := writeCapture(t, "capture.har", testHAR(t))
	handler := captureHandler(path, "https://inspector.example")

	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/capture", nil)
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Origin", "https://inspector.example")

	response := newResponseRecorder()
	handler.ServeHTTP(response, request)

	if response.status != http.StatusOK {
		t.Fatalf("status = %d", response.status)
	}

	if got := response.header.Get("Access-Control-Allow-Origin"); got != "https://inspector.example" {
		t.Fatalf("allow origin = %q", got)
	}

	if got := response.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q", got)
	}

	forbidden, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/capture", nil)
	if err != nil {
		t.Fatal(err)
	}

	forbidden.Header.Set("Origin", "https://evil.example")

	forbiddenResponse := newResponseRecorder()
	handler.ServeHTTP(forbiddenResponse, forbidden)

	if forbiddenResponse.status != http.StatusForbidden {
		t.Fatalf("forbidden status = %d", forbiddenResponse.status)
	}

	notFound := newResponseRecorder()

	notFoundRequest, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/missing", nil)
	if err != nil {
		t.Fatal(err)
	}

	handler.ServeHTTP(notFound, notFoundRequest)

	if notFound.status != http.StatusNotFound {
		t.Fatalf("not found status = %d", notFound.status)
	}

	method := newResponseRecorder()

	methodRequest, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/capture", nil)
	if err != nil {
		t.Fatal(err)
	}

	handler.ServeHTTP(method, methodRequest)

	if method.status != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", method.status)
	}
}

type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header)}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}

	return r.body.Write(data)
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
}

func TestValidateInspectorURL(t *testing.T) {
	for _, value := range []string{
		"https://mgurevin.github.io/recorder/",
		"http://127.0.0.1:5173/",
		"http://localhost:5173/",
		"http://[::1]:5173/",
	} {
		if _, _, err := validateInspectorURL(value); err != nil {
			t.Fatalf("validateInspectorURL(%q): %v", value, err)
		}
	}

	for _, value := range []string{
		"http://example.com/",
		"https://user:pass@example.com/",
		"file:///tmp/index.html",
	} {
		if _, _, err := validateInspectorURL(value); err == nil {
			t.Fatalf("validateInspectorURL(%q) succeeded", value)
		}
	}
}

func testHAR(t *testing.T) []byte {
	t.Helper()

	now := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	traceID := "trace-1"
	entries := []*recorder.Entry{
		{
			StartedDateTime: now,
			Time:            12.5,
			Request: &recorder.Request{
				Method:      "POST",
				URL:         "https://api.example.com/orders",
				HTTPVersion: "HTTP/1.1",
				Cookies:     []recorder.Cookie{},
				Headers:     []recorder.NameValuePair{},
				QueryString: []recorder.NameValuePair{},
				HeadersSize: -1,
				BodySize:    4,
			},
			Response: &recorder.Response{
				Status:      201,
				StatusText:  "Created",
				HTTPVersion: "HTTP/1.1",
				Cookies:     []recorder.Cookie{},
				Headers:     []recorder.NameValuePair{},
				Content:     &recorder.Content{Size: 2, MimeType: "application/json", Text: "{}"},
				HeadersSize: -1,
				BodySize:    2,
			},
			Cache:   &recorder.Cache{},
			Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, Send: 1, Wait: 10, Receive: 1.5, SSL: -1},
			Recorder: &recorder.RecorderEntryExtension{
				SchemaVersion: recorder.RecorderExtensionVersion,
				TraceID:       traceID,
				ResponseBody: &recorder.BodyInfo{
					Present:       true,
					Complete:      true,
					CapturedBytes: 2,
					TotalBytes:    2,
				},
			},
		},
		{
			StartedDateTime: now,
			Time:            2,
			Request: &recorder.Request{
				Method:      "POST",
				URL:         "https://api.example.com/orders",
				HTTPVersion: "HTTP/1.1",
				Cookies:     []recorder.Cookie{},
				Headers:     []recorder.NameValuePair{},
				QueryString: []recorder.NameValuePair{},
				HeadersSize: -1,
				BodySize:    0,
			},
			Response: &recorder.Response{
				Status:      0,
				StatusText:  "",
				HTTPVersion: "",
				Cookies:     []recorder.Cookie{},
				Headers:     []recorder.NameValuePair{},
				Content:     &recorder.Content{Size: 0, MimeType: ""},
				HeadersSize: -1,
				BodySize:    0,
			},
			Cache:   &recorder.Cache{},
			Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, Send: -1, Wait: -1, Receive: -1, SSL: -1},
			Recorder: &recorder.RecorderEntryExtension{
				SchemaVersion: recorder.RecorderExtensionVersion,
				TraceID:       traceID,
				Error:         &recorder.ErrorInfo{Message: "dial failed"},
			},
		},
	}

	var output bytes.Buffer
	if err := recorder.NewHAR(entries).Write(&output); err != nil {
		t.Fatal(err)
	}

	return output.Bytes()
}

func writeCapture(t *testing.T, name string, contents []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}
