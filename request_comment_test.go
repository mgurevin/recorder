package recorder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type commentRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f commentRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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

	recorder := NewMemoryRecorder()
	client := server.Client()
	client.Transport = NewTransport(client.Transport, recorder, DefaultConfig())
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	request = RequestWithComment(request, "redirected operation")

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	entries := recorder.Entries()
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
	recorder := NewMemoryRecorder()
	transport := NewTransport(commentRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	}), recorder, DefaultConfig())
	client := &http.Client{Transport: transport}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test/payment", nil)
	if err != nil {
		t.Fatal(err)
	}
	request = RequestWithComment(request, "payment authorization attempt")

	_, _ = client.Do(request)
	entries := recorder.Entries()
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

func TestWithRequestCommentReplacementAndRequestClone(t *testing.T) {
	ctx := WithRequestComment(context.Background(), "first")
	ctx = WithRequestComment(ctx, "second")
	if got := requestCommentFromContext(ctx); got != "second" {
		t.Fatalf("comment = %q, want second", got)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	clone := RequestWithComment(request, "clone")
	if clone == request {
		t.Fatal("RequestWithComment returned the original request")
	}
	if got := requestCommentFromContext(request.Context()); got != "" {
		t.Fatalf("original request comment = %q", got)
	}
	if got := requestCommentFromContext(clone.Context()); got != "clone" {
		t.Fatalf("cloned request comment = %q", got)
	}
	if RequestWithComment(nil, "ignored") != nil {
		t.Fatal("RequestWithComment(nil) must return nil")
	}
}
