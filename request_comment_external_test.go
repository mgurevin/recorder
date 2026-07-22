package recorder_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	recorder "github.com/mgurevin/recorder"
)

type commentRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f commentRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRequestCommentFollowsRedirectContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "/final", http.StatusFound)

			return
		}

		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	record := recorder.NewMemoryRecorder()
	client := server.Client()
	client.Transport = recorder.NewTransport(client.Transport, record, recorder.DefaultConfig())

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}

	request = recorder.RequestWithComment(request, "redirected operation")

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	entries := record.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	for index, entry := range entries {
		if entry.Comment != "redirected operation" {
			t.Errorf("entry %d comment = %q", index, entry.Comment)
		}
	}
}

func TestRequestCommentIsRecordedOnFailedExchange(t *testing.T) {
	record := recorder.NewMemoryRecorder()
	transport := recorder.NewTransport(commentRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	}), record, recorder.DefaultConfig())
	client := &http.Client{Transport: transport}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test/payment", nil)
	if err != nil {
		t.Fatal(err)
	}

	request = recorder.RequestWithComment(request, "payment authorization attempt")

	_, _ = client.Do(request)

	entries := record.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	if got := entries[0].Comment; got != "payment authorization attempt" {
		t.Fatalf("entry comment = %q", got)
	}

	if got := entries[0].Request.Comment; got != "" {
		t.Fatalf("request comment = %q, want empty", got)
	}
}
