package hartest_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hartest"
)

const fakeEncryptedToken = "REC-ENC-v1.a2V5.cGF5bG9hZA"

func entry(method, target, requestBody, responseBody string) *recorder.Entry {
	requestInfo := &recorder.BodyInfo{Present: requestBody != "", Complete: true, CapturedBytes: int64(len(requestBody)), TotalBytes: int64(len(requestBody))}
	responseInfo := &recorder.BodyInfo{Present: responseBody != "", Complete: true, CapturedBytes: int64(len(responseBody)), TotalBytes: int64(len(responseBody))}

	var postData *recorder.PostData
	if requestBody != "" {
		postData = &recorder.PostData{MimeType: "application/json", Text: requestBody}
	}

	return &recorder.Entry{
		StartedDateTime: "2026-07-23T00:00:00.000Z",
		Time:            1,
		Request: &recorder.Request{
			Method:      method,
			URL:         target,
			HTTPVersion: "HTTP/1.1",
			Headers:     []recorder.NameValuePair{{Name: "Content-Type", Value: "application/json"}},
			PostData:    postData,
			HeadersSize: -1,
			BodySize:    int64(len(requestBody)),
		},
		Response: &recorder.Response{
			Status:      200,
			StatusText:  "OK",
			HTTPVersion: "HTTP/1.1",
			Headers:     []recorder.NameValuePair{{Name: "Content-Type", Value: "application/json"}},
			Content:     &recorder.Content{Size: int64(len(responseBody)), MimeType: "application/json", Text: responseBody},
			HeadersSize: -1,
			BodySize:    int64(len(responseBody)),
		},
		Cache:   &recorder.Cache{},
		Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, Send: 0, Wait: 1, Receive: 0, SSL: -1},
		Recorder: &recorder.RecorderEntryExtension{
			SchemaVersion: recorder.RecorderExtensionVersion,
			RequestBody:   requestInfo,
			ResponseBody:  responseInfo,
		},
	}
}

func TestTransportMatchesPostBodyAndHeaders(t *testing.T) {
	t.Parallel()

	first := entry("POST", "https://api.example.com/orders", `{"id":1}`, `{"result":"first"}`)
	second := entry("POST", "https://api.example.com/orders", `{"id":2}`, `{"result":"second"}`)

	fixture, err := hartest.NewTransport([]*recorder.Entry{first, second}, hartest.DefaultConfig())
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}

	client := &http.Client{Transport: fixture}

	request, err := http.NewRequest(http.MethodPost, "https://api.example.com/orders", strings.NewReader(`{"id":2}`))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if err := response.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got, want := string(body), `{"result":"second"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}

	if err := fixture.Verify(); err == nil || !strings.Contains(err.Error(), "1 fixture") {
		t.Fatalf("Verify = %v", err)
	}
}

func TestTransportConsumesDuplicateFixturesInOrder(t *testing.T) {
	t.Parallel()

	first := entry("GET", "https://api.example.com/items", "", "first")
	second := entry("GET", "https://api.example.com/items", "", "second")
	config := hartest.DefaultConfig()
	config.Match.Headers = nil

	fixture, err := hartest.NewTransport([]*recorder.Entry{first, second}, config)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: fixture}

	for _, want := range []string{"first", "second"} {
		response, err := client.Get("https://api.example.com/items")
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()

		if err != nil {
			t.Fatal(err)
		}

		if string(body) != want {
			t.Fatalf("body = %q, want %q", body, want)
		}
	}

	if err := fixture.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestTransportResolvesProtectedJSONWithoutMutatingEntry(t *testing.T) {
	t.Parallel()

	requestBody := `{"secret":"` + fakeEncryptedToken + `"}`
	responseBody := `{"secret":"` + fakeEncryptedToken + `"}`
	captured := entry("POST", "https://api.example.com/protected", requestBody, responseBody)

	config := hartest.DefaultConfig()
	config.ProtectedValues = hartest.ProtectedValueResolverFunc(func(token string) ([]byte, bool, error) {
		if token != fakeEncryptedToken {
			return nil, false, nil
		}

		return []byte(`"plain"`), true, nil
	})

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, config)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, captured.Request.URL, strings.NewReader(`{"secret":"plain"}`))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := (&http.Client{Transport: fixture}).Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	_ = response.Body.Close()

	if got, want := string(body), `{"secret":"plain"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}

	if captured.Request.PostData.Text != requestBody || captured.Response.Content.Text != responseBody {
		t.Fatal("source entry was mutated")
	}
}

func TestTransportFailsClosedForUnresolvedRequestBody(t *testing.T) {
	t.Parallel()

	captured := entry("POST", "https://api.example.com/protected", `{"secret":"`+fakeEncryptedToken+`"}`, "ok")

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, hartest.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, captured.Request.URL, strings.NewReader(`{"secret":"plain"}`))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json")

	_, err = fixture.RoundTrip(request)
	if err == nil || !strings.Contains(err.Error(), "unresolved protected") {
		t.Fatalf("RoundTrip error = %v", err)
	}

	if strings.Contains(err.Error(), "plain") {
		t.Fatalf("error leaks plaintext: %v", err)
	}
}

func TestTransportExternalResponseBodyAndTrailers(t *testing.T) {
	t.Parallel()

	captured := entry("GET", "https://api.example.com/external", "", "")
	captured.Response.Content.Text = ""
	captured.Recorder.ResponseBody = &recorder.BodyInfo{Present: true, Complete: true, CapturedBytes: 8, TotalBytes: 8, Store: "body:1"}
	captured.Recorder.ResponseTrailers = []recorder.NameValuePair{{Name: "Digest", Value: "sha-256=value"}}

	config := hartest.DefaultConfig()
	config.Match.Headers = nil
	config.Bodies = hartest.BodyOpenerFunc(func(ref string) (io.ReadCloser, error) {
		if ref != "body:1" {
			return nil, errors.New("unexpected ref")
		}

		return io.NopCloser(strings.NewReader("external")), nil
	})

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, config)
	if err != nil {
		t.Fatal(err)
	}

	response, err := (&http.Client{Transport: fixture}).Get(captured.Request.URL)
	if err != nil {
		t.Fatal(err)
	}

	if response.Trailer.Get("Digest") != "" {
		t.Fatal("trailer visible before EOF")
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "external" {
		t.Fatalf("body = %q", body)
	}

	if response.Trailer.Get("Digest") != "sha-256=value" {
		t.Fatalf("trailer = %q", response.Trailer.Get("Digest"))
	}

	_ = response.Body.Close()
}

func TestTransportReturnsRecordedFailure(t *testing.T) {
	t.Parallel()

	captured := entry("GET", "https://api.example.com/fail", "", "")
	captured.Response.Status = 0
	captured.Response.StatusText = ""
	captured.Recorder.Error = &recorder.ErrorInfo{Phase: recorder.PhaseTLS, Message: "handshake failed", Timeout: true}

	config := hartest.DefaultConfig()
	config.Match.Headers = nil

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, config)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, captured.Request.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = fixture.RoundTrip(request)

	var recorded *hartest.RecordedError
	if !errors.As(err, &recorded) || recorded.Phase != recorder.PhaseTLS || !recorded.Timeout() {
		t.Fatalf("error = %#v", err)
	}
}

func TestTransportDoesNotLeakQueryValuesInMismatch(t *testing.T) {
	t.Parallel()

	captured := entry("GET", "https://api.example.com/items?token=expected", "", "")
	config := hartest.DefaultConfig()
	config.Match.Headers = nil

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, config)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://api.example.com/items?token=actual-secret", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = fixture.RoundTrip(request)
	if err == nil {
		t.Fatal("RoundTrip unexpectedly succeeded")
	}

	if strings.Contains(err.Error(), "actual-secret") || strings.Contains(err.Error(), "expected") {
		t.Fatalf("mismatch leaks query values: %v", err)
	}
}

func TestTransportRejectsOversizedRequestBody(t *testing.T) {
	t.Parallel()

	config := hartest.DefaultConfig()
	config.Match.MaxRequestBodyBytes = 2

	fixture, err := hartest.NewTransport([]*recorder.Entry{entry("POST", "https://api.example.com/", "abc", "ok")}, config)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, "https://api.example.com/", bytes.NewBufferString("abc"))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json")

	if _, err := fixture.RoundTrip(request); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestTransportRejectsTruncatedResponseBody(t *testing.T) {
	t.Parallel()

	captured := entry("GET", "https://api.example.com/truncated", "", "partial")
	captured.Recorder.ResponseBody.Truncated = true
	captured.Recorder.ResponseBody.Complete = false
	captured.Recorder.ResponseBody.TotalBytes++
	config := hartest.DefaultConfig()
	config.Match.Headers = nil

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, config)
	if err != nil {
		t.Fatal(err)
	}

	_, err = (&http.Client{Transport: fixture}).Get(captured.Request.URL)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v", err)
	}
}

func TestTransportBoundsExternalResponseBody(t *testing.T) {
	t.Parallel()

	captured := entry("GET", "https://api.example.com/external", "", "")
	captured.Recorder.ResponseBody = &recorder.BodyInfo{
		Present:       true,
		Complete:      true,
		CapturedBytes: 4,
		TotalBytes:    4,
		Store:         "body:large",
	}
	config := hartest.DefaultConfig()
	config.Match.Headers = nil
	config.MaxResponseBodyBytes = 3
	config.Bodies = hartest.BodyOpenerFunc(func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("four")), nil
	})

	fixture, err := hartest.NewTransport([]*recorder.Entry{captured}, config)
	if err != nil {
		t.Fatal(err)
	}

	_, err = (&http.Client{Transport: fixture}).Get(captured.Request.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}
