package recorder

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- shared helpers ----

// newRecordedClient wraps the httptest server's client with a recording
// transport backed by a fresh MemoryRecorder.
func newRecordedClient(ts *httptest.Server, opts ...Option) (*http.Client, *MemoryRecorder) {
	rec := NewMemoryRecorder()
	c := ts.Client()
	allOpts := append([]Option{
		WithCaptureRequestBody(true),
		WithCaptureResponseBody(true),
		WithEmbedBodies(true),
		WithHashBodies(true, "sha256"),
	}, opts...)
	c.Transport = NewTransport(c.Transport, rec, allOpts...)
	return c, rec
}

func mustReadAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	return b
}

func singleEntry(t *testing.T, rec *MemoryRecorder) *Entry {
	t.Helper()
	entries := rec.Entries()
	if len(entries) != 1 {
		t.Fatalf("recorded entries = %d, want 1", len(entries))
	}
	return entries[0]
}

func findHeader(pairs []NameValuePair, name string) (string, bool) {
	for _, p := range pairs {
		if strings.EqualFold(p.Name, name) {
			return p.Value, true
		}
	}
	return "", false
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

func closedPortAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// errTransport is a broken RoundTripper returning a fixed error.
type errTransport struct{ err error }

func (t errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

// errReader yields some data, then a read error.
type errReader struct {
	data []byte
	err  error
	off  int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

// failStore is a BodyStore whose writers cannot be created.
type failStore struct{}

func (failStore) NewWriter(context.Context, BodyMetadata) (BodyWriter, error) {
	return nil, errors.New("store down")
}

// ---- tests ----

func TestSuccessfulGET(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL + "/greet?x=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := mustReadAll(t, resp.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}

	e := singleEntry(t, rec)
	if e.Request.Method != "GET" {
		t.Errorf("method = %q", e.Request.Method)
	}
	if e.Request.URL != ts.URL+"/greet?x=1" {
		t.Errorf("url = %q", e.Request.URL)
	}
	if len(e.Request.QueryString) != 1 || e.Request.QueryString[0].Name != "x" || e.Request.QueryString[0].Value != "1" {
		t.Errorf("queryString = %+v", e.Request.QueryString)
	}
	if _, ok := findHeader(e.Request.Headers, "Host"); !ok {
		t.Errorf("missing Host header in %+v", e.Request.Headers)
	}
	if e.Response.Status != 200 || e.Response.StatusText != "OK" {
		t.Errorf("status = %d %q", e.Response.Status, e.Response.StatusText)
	}
	if e.Response.Content.Text != "hello" || e.Response.Content.Size != 5 {
		t.Errorf("content = %+v", e.Response.Content)
	}
	if e.Response.BodySize != 5 {
		t.Errorf("bodySize = %d, want 5", e.Response.BodySize)
	}
	if e.Error != nil {
		t.Errorf("unexpected _error: %+v", e.Error)
	}
	if e.State != StateCompleted {
		t.Errorf("state = %q", e.State)
	}
	if e.RequestBody != nil {
		t.Errorf("requestBody = %+v, want nil", e.RequestBody)
	}
	rb := e.ResponseBody
	if rb == nil || !rb.Complete || rb.ClosedEarly || rb.TotalBytes != 5 || rb.CapturedBytes != 5 {
		t.Fatalf("responseBody = %+v", rb)
	}
	if rb.Hash != sha256Hex([]byte("hello")) || rb.HashAlgorithm != "sha256" {
		t.Errorf("hash = %s (%s)", rb.Hash, rb.HashAlgorithm)
	}
	if e.ServerIPAddress != "127.0.0.1" {
		t.Errorf("serverIPAddress = %q", e.ServerIPAddress)
	}
	if e.Connection == "" {
		t.Errorf("connection id empty")
	}
	tm := e.Timings
	if tm.Connect < 0 || tm.Send < 0 || tm.Wait < 0 || tm.Receive < 0 || tm.Blocked < 0 {
		t.Errorf("timings = %+v", tm)
	}
	if tm.DNS != -1 { // literal IP host, no resolver involved
		t.Errorf("dns timing = %v, want -1", tm.DNS)
	}
	if tm.SSL != -1 { // plain http
		t.Errorf("ssl timing = %v, want -1", tm.SSL)
	}
	if e.Time < tm.Receive {
		t.Errorf("time %v < receive %v", e.Time, tm.Receive)
	}
	if e.ExchangeID == "" || e.TraceID == "" {
		t.Errorf("missing ids: %q %q", e.ExchangeID, e.TraceID)
	}
	if e.Network == nil || e.Network.ConnectionReused {
		t.Errorf("network = %+v", e.Network)
	}
}

func TestSuccessfulPOSTJSON(t *testing.T) {
	var serverGot []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverGot, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	payload := []byte(`{"name":"recorder","n":1}`)
	resp, err := client.Post(ts.URL, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)

	if !bytes.Equal(serverGot, payload) {
		t.Fatalf("server received %q", serverGot)
	}
	e := singleEntry(t, rec)
	if e.Request.PostData == nil {
		t.Fatal("postData missing")
	}
	if e.Request.PostData.MimeType != "application/json" || e.Request.PostData.Text != string(payload) {
		t.Errorf("postData = %+v", e.Request.PostData)
	}
	if e.Request.BodySize != int64(len(payload)) {
		t.Errorf("bodySize = %d", e.Request.BodySize)
	}
	rb := e.RequestBody
	if rb == nil || !rb.Complete || rb.TotalBytes != int64(len(payload)) {
		t.Fatalf("requestBody = %+v", rb)
	}
	if rb.Hash != sha256Hex(payload) {
		t.Errorf("request body hash = %s", rb.Hash)
	}
	if e.Response.Status != 201 {
		t.Errorf("status = %d", e.Response.Status)
	}
}

func TestRequestWithoutBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if e.Request.PostData != nil {
		t.Errorf("postData = %+v, want nil", e.Request.PostData)
	}
	if e.Request.BodySize != 0 {
		t.Errorf("bodySize = %d, want 0", e.Request.BodySize)
	}
}

func TestErrorStatusesAreNotTransportErrors(t *testing.T) {
	for _, status := range []int{404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", status)
			}))
			defer ts.Close()
			client, rec := newRecordedClient(ts)

			resp, err := client.Get(ts.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			mustReadAll(t, resp.Body)

			e := singleEntry(t, rec)
			if e.Response.Status != status {
				t.Errorf("status = %d", e.Response.Status)
			}
			if e.Error != nil {
				t.Errorf("4xx/5xx must not produce _error, got %+v", e.Error)
			}
			if e.State != StateCompleted {
				t.Errorf("state = %q", e.State)
			}
		})
	}
}

func TestEmptyBodyFinalizesAtRoundTrip(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	// The entry must exist before the caller touches the body: a 204 body is
	// legally never read.
	if rec.Len() != 1 {
		t.Fatalf("entries after RoundTrip = %d, want 1", rec.Len())
	}
	resp.Body.Close()

	e := singleEntry(t, rec)
	if e.State != StateCompleted || !e.ResponseBody.Complete || e.ResponseBody.TotalBytes != 0 {
		t.Errorf("entry = state %q body %+v", e.State, e.ResponseBody)
	}
}

func TestRedirectChain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b?token=redirect-secret&keep=1", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/c", http.StatusFound)
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("done"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithRedactQueryParameters("token"))

	ctx := WithTraceID(context.Background(), "chain-1")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/a", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if body := mustReadAll(t, resp.Body); string(body) != "done" {
		t.Fatalf("body = %q", body)
	}

	entries := rec.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	wantStatus := []int{301, 302, 200}
	for i, e := range entries {
		if e.Response.Status != wantStatus[i] {
			t.Errorf("entry %d status = %d, want %d", i, e.Response.Status, wantStatus[i])
		}
		if e.TraceID != "chain-1" {
			t.Errorf("entry %d traceId = %q", i, e.TraceID)
		}
		if e.RedirectIndex == nil || *e.RedirectIndex != i {
			t.Errorf("entry %d redirectIndex = %v", i, e.RedirectIndex)
		}
		if e.Error != nil {
			t.Errorf("entry %d unexpected error %+v", i, e.Error)
		}
	}
	if entries[0].Response.RedirectURL != "/b?token=%5BREDACTED%5D&keep=1" {
		t.Errorf("redirectURL = %q", entries[0].Response.RedirectURL)
	}
	if location, ok := findHeader(entries[0].Response.Headers, "Location"); !ok || strings.Contains(location, "redirect-secret") {
		t.Errorf("Location header leaked redirect secret: %q", location)
	}
	if entries[2].State != StateCompleted {
		t.Errorf("final state = %q", entries[2].State)
	}
}

func TestRedirectLoop(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	_, err := client.Get(ts.URL + "/loop") //nolint:bodyclose // Do returns nil resp on redirect-loop errors
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v", err)
	}
	if rec.Len() != 10 {
		t.Fatalf("entries = %d, want 10 (one per physical exchange)", rec.Len())
	}
	for i, e := range rec.Entries() {
		if e.Response.Status != 302 {
			t.Errorf("entry %d status = %d", i, e.Response.Status)
		}
	}
}

func TestDNSError(t *testing.T) {
	base := &http.Transport{}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	_, err := client.Get("http://recorder-does-not-exist.invalid/") //nolint:bodyclose
	if err == nil {
		t.Fatal("expected DNS failure")
	}
	e := singleEntry(t, rec)
	if e.Error == nil {
		t.Fatal("missing _error")
	}
	if e.Error.Phase != PhaseDNS {
		t.Errorf("phase = %q, want dns (err: %s)", e.Error.Phase, e.Error.Message)
	}
	found := false
	for _, tn := range e.Error.UnwrapChain {
		if tn == "*net.DNSError" {
			found = true
		}
	}
	if !found {
		t.Errorf("unwrapChain = %v, want *net.DNSError", e.Error.UnwrapChain)
	}
	if e.Response.Status != 0 {
		t.Errorf("status = %d, want 0", e.Response.Status)
	}
	if e.State != StateFailed {
		t.Errorf("state = %q", e.State)
	}
	// The request never reached the wire: its HTTP version has no factual
	// value and must not be invented.
	if e.Request.HTTPVersion != "" {
		t.Errorf("httpVersion = %q, want empty for a request that was never sent", e.Request.HTTPVersion)
	}
}

func TestConnectionRefused(t *testing.T) {
	addr := closedPortAddr(t)
	base := &http.Transport{}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	_, err := client.Get("http://" + addr + "/") //nolint:bodyclose
	if err == nil {
		t.Fatal("expected connection failure")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseConnect {
		t.Fatalf("error = %+v, want phase connect", e.Error)
	}
	if e.Error.Timeout {
		t.Errorf("timeout = true for refused connection")
	}
}

func TestTCPConnectTimeout(t *testing.T) {
	// Deterministic connect timeout: the dialer announces ConnectStart like
	// net.Dialer would, then blocks until the context deadline fires,
	// exactly like an unreachable host.
	base := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if tr := httptrace.ContextClientTrace(ctx); tr != nil && tr.ConnectStart != nil {
				tr.ConnectStart(network, addr)
			}
			<-ctx.Done()
			return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
		},
	}
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://192.0.2.1/", nil)
	_, err := client.Do(req) //nolint:bodyclose
	if err == nil {
		t.Fatal("expected timeout")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseConnect {
		t.Fatalf("error = %+v, want phase connect", e.Error)
	}
	if !e.Error.ContextDeadlineExceeded || !e.Error.Timeout {
		t.Errorf("flags = %+v, want deadline+timeout", e.Error)
	}
}

// TestResponseHeaderTimeout: the transport's own header timeout matches
// context.DeadlineExceeded via errors.Is, but the request context never
// fired — the entry must attribute the failure to waiting for the response,
// not to a caller cancellation.
func TestResponseHeaderTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // server-side stall
	}))
	defer ts.Close()
	base := &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	_, err := client.Get(ts.URL) //nolint:bodyclose
	if err == nil {
		t.Fatal("expected header timeout")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseWaitResponse {
		t.Fatalf("error = %+v, want phase wait_response", e.Error)
	}
	if !e.Error.Timeout {
		t.Errorf("timeout flag not set")
	}
	if e.Error.ContextDeadlineExceeded || e.Error.ContextCanceled {
		t.Errorf("context flags set although the request context never fired: %+v", e.Error)
	}
	if e.Timings.Wait != -1 || e.Timings.Send < 0 {
		t.Errorf("timings = %+v", e.Timings)
	}
}

func TestContextCanceled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	_, err := client.Do(req) //nolint:bodyclose
	if err == nil {
		t.Fatal("expected cancellation")
	}
	e := singleEntry(t, rec)
	if e.Error == nil {
		t.Fatal("missing _error")
	}
	if e.Error.Phase != PhaseContext {
		t.Errorf("phase = %q, want context", e.Error.Phase)
	}
	if !e.Error.ContextCanceled || e.Error.ContextDeadlineExceeded {
		t.Errorf("flags = %+v", e.Error)
	}
	if e.Error.Cause == "" {
		t.Errorf("cause not recorded")
	}
	// Headers were written over cleartext through *http.Transport before the
	// cancellation, so HTTP/1.1 is a fact, not a guess.
	if e.Request.HTTPVersion != "HTTP/1.1" {
		t.Errorf("httpVersion = %q, want HTTP/1.1 for a request written on a cleartext connection", e.Request.HTTPVersion)
	}
}

func TestContextDeadlineExceeded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	_, err := client.Do(req) //nolint:bodyclose
	if err == nil {
		t.Fatal("expected deadline")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseContext {
		t.Fatalf("error = %+v, want phase context", e.Error)
	}
	if !e.Error.ContextDeadlineExceeded || !e.Error.Timeout || e.Error.ContextCanceled {
		t.Errorf("flags = %+v", e.Error)
	}
}

func TestTLSCertificateVerificationError(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // expected handshake rejections
	base := &http.Transport{}                       // does not trust the httptest CA
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	_, err := client.Get(ts.URL) //nolint:bodyclose
	if err == nil {
		t.Fatal("expected certificate verification failure")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseTLS {
		t.Fatalf("error = %+v, want phase tls", e.Error)
	}
	if e.Error.Timeout {
		t.Errorf("timeout = true")
	}
}

func TestTLSHostnameMismatch(t *testing.T) {
	tlsCert, parsed := selfSignedCert(t, "recorder.test")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:  http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		ErrorLog: log.New(io.Discard, "", 0), // expected handshake rejections
	}
	go srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{tlsCert}}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	base := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	// The cert is only valid for "recorder.test"; we connect by IP.
	_, err = client.Get("https://" + ln.Addr().String() + "/") //nolint:bodyclose
	if err == nil {
		t.Fatal("expected hostname mismatch")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseTLS {
		t.Fatalf("error = %+v, want phase tls", e.Error)
	}
	found := false
	for _, tn := range e.Error.UnwrapChain {
		if strings.Contains(tn, "HostnameError") {
			found = true
		}
	}
	if !found {
		t.Errorf("unwrapChain = %v, want x509.HostnameError", e.Error.UnwrapChain)
	}
}

func TestTLSHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept the TCP connection, never answer the ClientHello.
			go func(c net.Conn) {
				<-stop
				c.Close()
			}(conn)
		}
	}()

	base := &http.Transport{TLSHandshakeTimeout: 200 * time.Millisecond}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	_, err = client.Get("https://" + ln.Addr().String() + "/") //nolint:bodyclose
	if err == nil {
		t.Fatal("expected handshake timeout")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseTLS {
		t.Fatalf("error = %+v, want phase tls", e.Error)
	}
	if !e.Error.Timeout {
		t.Errorf("timeout flag not set: %+v", e.Error)
	}
}

func TestRequestBodyReadError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	boom := errors.New("boom while reading request body")
	req, _ := http.NewRequest(http.MethodPost, ts.URL, &errReader{data: []byte("abc"), err: boom})
	_, err := client.Do(req) //nolint:bodyclose
	if err == nil {
		t.Fatal("expected request body failure")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseWriteRequestBody {
		t.Fatalf("error = %+v, want phase write_request_body", e.Error)
	}
	if e.RequestBody == nil || !strings.Contains(e.RequestBody.ReadError, "boom") {
		t.Errorf("requestBody = %+v", e.RequestBody)
	}
	if e.State != StateFailed {
		t.Errorf("state = %q", e.State)
	}
}

func TestResponseBodyReadError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		w.Write(make([]byte, 100))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // slam the connection shut mid-body
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr == nil {
		t.Fatal("expected body read error")
	}

	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseReadResponseBody {
		t.Fatalf("error = %+v, want phase read_response_body", e.Error)
	}
	rb := e.ResponseBody
	if rb == nil || rb.Complete || rb.ReadError == "" {
		t.Errorf("responseBody = %+v", rb)
	}
	if e.State != StateFailed {
		t.Errorf("state = %q", e.State)
	}
}

func TestResponseBodyClosedEarly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 64<<10))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	resp.Body.Close()

	e := singleEntry(t, rec)
	if e.State != StateClosedEarly {
		t.Errorf("state = %q", e.State)
	}
	rb := e.ResponseBody
	if rb == nil || rb.Complete || !rb.ClosedEarly {
		t.Fatalf("responseBody = %+v", rb)
	}
	if rb.CapturedBytes != 10 || rb.TotalBytes != 10 {
		t.Errorf("bytes = %d/%d, want exactly the 10 the caller read", rb.CapturedBytes, rb.TotalBytes)
	}
	if rb.Hash != "" {
		t.Errorf("partial stream must not carry a hash, got %s", rb.Hash)
	}
	if e.Response.BodySize != -1 {
		t.Errorf("bodySize = %d, want -1 for unread wire size", e.Response.BodySize)
	}
	if e.Error != nil {
		t.Errorf("early close is not an error: %+v", e.Error)
	}
}

func TestResponseBodyClosedWithoutRead(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("unread payload"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	e := singleEntry(t, rec)
	rb := e.ResponseBody
	if rb == nil || !rb.ClosedEarly || rb.CapturedBytes != 0 || rb.TotalBytes != 0 {
		t.Fatalf("responseBody = %+v", rb)
	}
	if e.Response.Content.Size != 0 {
		t.Errorf("content.size = %d", e.Response.Content.Size)
	}
}

func TestLargeRequestBodyTruncation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithMaxRequestBodyBytes(1024))

	payload := bytes.Repeat([]byte("a"), 10240)
	resp, err := client.Post(ts.URL, "text/plain", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	rb := e.RequestBody
	if rb == nil || !rb.Truncated || !rb.Complete {
		t.Fatalf("requestBody = %+v", rb)
	}
	if rb.CapturedBytes != 1024 || rb.TotalBytes != 10240 {
		t.Errorf("bytes = %d/%d", rb.CapturedBytes, rb.TotalBytes)
	}
	if rb.Hash != sha256Hex(payload) {
		t.Errorf("hash must cover the full stream, not the truncated capture")
	}
	if got := len(e.Request.PostData.Text); got != 1024 {
		t.Errorf("captured text length = %d", got)
	}
	if e.Request.BodySize != 10240 {
		t.Errorf("bodySize = %d, want full size", e.Request.BodySize)
	}
}

func TestLargeResponseBodyTruncation(t *testing.T) {
	payload := bytes.Repeat([]byte("b"), 10240)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(payload)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithMaxResponseBodyBytes(1024))

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := mustReadAll(t, resp.Body); !bytes.Equal(got, payload) {
		t.Fatal("caller must receive the untruncated body")
	}

	e := singleEntry(t, rec)
	rb := e.ResponseBody
	if rb == nil || !rb.Truncated || !rb.Complete {
		t.Fatalf("responseBody = %+v", rb)
	}
	if rb.CapturedBytes != 1024 || rb.TotalBytes != 10240 {
		t.Errorf("bytes = %d/%d", rb.CapturedBytes, rb.TotalBytes)
	}
	if rb.Hash != sha256Hex(payload) {
		t.Errorf("hash must cover the full stream")
	}
	if e.Response.Content.Size != 10240 {
		t.Errorf("content.size = %d, want total read size", e.Response.Content.Size)
	}
	if len(e.Response.Content.Text) != 1024 {
		t.Errorf("captured text length = %d", len(e.Response.Content.Text))
	}
}

func TestBinaryResponseBase64(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(payload)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if e.Response.Content.Encoding != "base64" {
		t.Fatalf("encoding = %q", e.Response.Content.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(e.Response.Content.Text)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Errorf("base64 roundtrip failed: %v", err)
	}
}

func TestGzipResponse(t *testing.T) {
	const plain = "hello gzip world"
	var wireLen int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte(plain))
		gz.Close()
		wireLen = buf.Len()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		w.Write(buf.Bytes())
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if body := mustReadAll(t, resp.Body); string(body) != plain {
		t.Fatalf("body = %q", body)
	}

	e := singleEntry(t, rec)
	if e.Response.Content.Text != plain {
		t.Errorf("content.text = %q", e.Response.Content.Text)
	}
	if !e.Response.Content.Decoded {
		t.Errorf("_decoded flag not set for transparently decompressed body")
	}
	if e.Response.BodySize != -1 {
		t.Errorf("bodySize = %d, want -1: the compressed wire size (%d) is not observable after transparent decoding", e.Response.BodySize, wireLen)
	}
	if e.ResponseBody.TotalBytes != int64(len(plain)) {
		t.Errorf("totalBytes = %d, want decoded length", e.ResponseBody.TotalBytes)
	}
}

func TestChunkedResponseUnknownContentLength(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Write([]byte("part1-"))
		f.Flush()
		w.Write([]byte("part2"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.ContentLength != -1 {
		t.Fatalf("test setup: ContentLength = %d, want -1 (chunked)", resp.ContentLength)
	}
	if body := mustReadAll(t, resp.Body); string(body) != "part1-part2" {
		t.Fatalf("body = %q", body)
	}

	e := singleEntry(t, rec)
	if e.Response.Content.Size != 11 || e.Response.BodySize != 11 {
		t.Errorf("sizes = %d/%d", e.Response.Content.Size, e.Response.BodySize)
	}
	found := false
	for _, te := range e.ResponseTransferEncoding {
		if te == "chunked" {
			found = true
		}
	}
	if !found {
		t.Errorf("transferEncoding = %v", e.ResponseTransferEncoding)
	}
}

func TestHTTP2Response(t *testing.T) {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("h2 body"))
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("test setup: proto = %q", resp.Proto)
	}

	e := singleEntry(t, rec)
	if e.Response.HTTPVersion != "HTTP/2.0" || e.Request.HTTPVersion != "HTTP/2.0" {
		t.Errorf("httpVersion = %q / %q", e.Request.HTTPVersion, e.Response.HTTPVersion)
	}
	if e.Network == nil || !e.Network.HTTP2 {
		t.Errorf("network = %+v", e.Network)
	}
	if e.TLS == nil || e.TLS.NegotiatedProtocol != "h2" {
		t.Errorf("tls = %+v", e.TLS)
	}
	if e.TLS != nil && len(e.TLS.PeerCertificates) == 0 {
		t.Errorf("peer certificates missing")
	}
}

func TestConnectionReuse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	for i := 0; i < 2; i++ {
		resp, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		mustReadAll(t, resp.Body)
	}

	entries := rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].Network.ConnectionReused {
		t.Errorf("first request must not reuse")
	}
	if entries[0].Timings.Connect < 0 {
		t.Errorf("first connect timing = %v", entries[0].Timings.Connect)
	}
	second := entries[1]
	if !second.Network.ConnectionReused {
		t.Fatalf("second request did not reuse the connection")
	}
	// HAR semantics: on a reused connection these phases did not happen and
	// must be -1, not a misleading 0.
	if second.Timings.DNS != -1 || second.Timings.Connect != -1 || second.Timings.SSL != -1 {
		t.Errorf("reused timings = %+v, want -1 dns/connect/ssl", second.Timings)
	}
}

func TestConcurrentRequests(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("concurrent"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	const goroutines, perGoroutine = 8, 10
	errs := make(chan error, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				resp, err := client.Get(ts.URL)
				if err != nil {
					errs <- err
					continue
				}
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					errs <- err
				}
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("request error: %v", err)
	}
	if rec.Len() != goroutines*perGoroutine {
		t.Fatalf("entries = %d, want %d", rec.Len(), goroutines*perGoroutine)
	}
	for _, e := range rec.Entries() {
		if e.State != StateCompleted || e.Error != nil {
			t.Fatalf("entry = state %q error %+v", e.State, e.Error)
		}
	}
}

func TestRedaction(t *testing.T) {
	var serverAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverAuth = r.Header.Get("Authorization")
		io.Copy(io.Discard, r.Body)
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "server-secret"})
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts,
		WithRedactQueryParameters("token"),
		WithRedactJSONFields("password"),
	)

	body := `{"password":"hunter2","user":"u1"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/?token=supersecret&ok=1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sekrit")
	req.Header.Set("X-Api-Key", "key-123")
	req.AddCookie(&http.Cookie{Name: "session", Value: "sess-secret"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	mustReadAll(t, resp.Body)

	if serverAuth != "Bearer sekrit" {
		t.Fatalf("redaction must not touch the real request; server saw %q", serverAuth)
	}
	e := singleEntry(t, rec)

	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if v, ok := findHeader(e.Request.Headers, name); !ok || v != redactedValue {
			t.Errorf("header %s = %q, want %q", name, v, redactedValue)
		}
	}
	if strings.Contains(e.Request.URL, "supersecret") || !strings.Contains(e.Request.URL, "ok=1") {
		t.Errorf("url = %q", e.Request.URL)
	}
	for _, p := range e.Request.QueryString {
		if p.Name == "token" && p.Value != redactedValue {
			t.Errorf("query token = %q", p.Value)
		}
	}
	if len(e.Request.Cookies) != 1 || e.Request.Cookies[0].Value != redactedValue {
		t.Errorf("request cookies = %+v", e.Request.Cookies)
	}
	pd := e.Request.PostData
	if pd == nil || strings.Contains(pd.Text, "hunter2") || !strings.Contains(pd.Text, redactedValue) || !strings.Contains(pd.Text, "u1") {
		t.Errorf("postData = %+v", pd)
	}
	if v, ok := findHeader(e.Response.Headers, "Set-Cookie"); !ok || v != redactedValue {
		t.Errorf("Set-Cookie = %q", v)
	}
	if len(e.Response.Cookies) != 1 || e.Response.Cookies[0].Value != redactedValue {
		t.Errorf("response cookies = %+v", e.Response.Cookies)
	}
}

func TestRecorderStorageErrorDoesNotBreakHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer ts.Close()

	var internal []error
	var mu sync.Mutex
	client, rec := newRecordedClient(ts,
		WithBodyStore(failStore{}),
		WithOnInternalError(func(err error) {
			mu.Lock()
			internal = append(internal, err)
			mu.Unlock()
		}),
	)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET must succeed despite store failure: %v", err)
	}
	if body := mustReadAll(t, resp.Body); string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}

	mu.Lock()
	n := len(internal)
	mu.Unlock()
	if n == 0 {
		t.Fatal("internal error not reported")
	}
	e := singleEntry(t, rec)
	rb := e.ResponseBody
	if rb == nil || rb.CapturedBytes != 0 || rb.TotalBytes != 5 || !rb.Complete {
		t.Errorf("responseBody = %+v", rb)
	}
	if e.Response.Content.Text != "" {
		t.Errorf("content captured despite store failure: %q", e.Response.Content.Text)
	}
}

func TestInternalErrorCallbackPanicDoesNotBreakHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts,
		WithBodyStore(failStore{}),
		WithInternalErrorMode(InternalErrorLog),
		WithLogf(func(string, ...any) { panic("logger panic") }),
		WithOnInternalError(func(error) { panic("callback panic") }),
	)
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET must survive reporting callback panics: %v", err)
	}
	if body := mustReadAll(t, resp.Body); string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
	if e := singleEntry(t, rec); e.ResponseBody == nil || !e.ResponseBody.Complete {
		t.Fatalf("response body was not finalized: %+v", e.ResponseBody)
	}
}

func TestBaseTransportError(t *testing.T) {
	boom := errors.New("kaboom from base transport")
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(errTransport{err: boom}, rec)}

	_, err := client.Get("http://example.test/") //nolint:bodyclose
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err = %v", err)
	}
	e := singleEntry(t, rec)
	if e.Error == nil {
		t.Fatal("missing _error")
	}
	if e.Error.Phase != PhaseRequestSetup {
		t.Errorf("phase = %q, want request_setup (no network activity happened)", e.Error.Phase)
	}
	if len(e.Error.UnwrapChain) == 0 {
		t.Errorf("unwrapChain empty")
	}
	if e.Response.Status != 0 || e.Response.BodySize != -1 {
		t.Errorf("placeholder response = %+v", e.Response)
	}
	if e.Timings.DNS != -1 || e.Timings.Connect != -1 || e.Timings.Receive != -1 {
		t.Errorf("timings = %+v, want all unmeasured", e.Timings)
	}
}

func TestRecorderPanicPolicies(t *testing.T) {
	panicky := RecorderFunc(func(*Entry) { panic("recorder exploded") })

	t.Run("default ignore", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok"))
		}))
		defer ts.Close()
		client := ts.Client()
		var got error
		client.Transport = NewTransport(client.Transport, panicky,
			WithOnInternalError(func(err error) { got = err }))
		resp, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("HTTP call must survive recorder panic: %v", err)
		}
		mustReadAll(t, resp.Body)
		if got == nil || !strings.Contains(got.Error(), "recorder exploded") {
			t.Errorf("internal error = %v", got)
		}
	})

	t.Run("preserves transport error", func(t *testing.T) {
		baseErr := errors.New("base failure")
		base := errTransport{err: baseErr}
		var got error
		client := &http.Client{Transport: NewTransport(base, panicky,
			WithOnInternalError(func(err error) { got = err }))}
		_, err := client.Get("http://example.test/") //nolint:bodyclose
		if !errors.Is(err, baseErr) {
			t.Errorf("err = %v, want original transport error", err)
		}
		if got == nil || !strings.Contains(got.Error(), "panic while recording") {
			t.Errorf("internal error = %v, want recorder panic reported separately", got)
		}
	})
}

func TestTrailerHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.Write([]byte("payload"))
		w.Header().Set("X-Checksum", "abc123")
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if v, ok := findHeader(e.ResponseTrailers, "X-Checksum"); !ok || v != "abc123" {
		t.Errorf("trailers = %+v", e.ResponseTrailers)
	}
}

func TestProxyError(t *testing.T) {
	proxyAddr := closedPortAddr(t)
	proxyURL, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	base := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	_, err = client.Get("http://recorder-proxy-target.invalid/") //nolint:bodyclose
	if err == nil {
		t.Fatal("expected proxy failure")
	}
	e := singleEntry(t, rec)
	if e.Error == nil || e.Error.Phase != PhaseProxy {
		t.Fatalf("error = %+v, want phase proxy", e.Error)
	}
	if e.Network == nil || e.Network.Proxy != proxyURL.String() {
		t.Errorf("network.proxy = %+v", e.Network)
	}
}

func TestProxyCallbackCalledOnce(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("via proxy"))
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	base := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		calls.Add(1)
		return proxyURL, nil
	}}
	defer base.CloseIdleConnections()
	client := &http.Client{Transport: NewTransport(base, NewMemoryRecorder())}
	resp, err := client.Get("http://origin-behind-proxy.invalid/")
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	mustReadAll(t, resp.Body)
	if got := calls.Load(); got != 1 {
		t.Fatalf("proxy callback calls = %d, want exactly 1", got)
	}
}

// TestProxySuccessFieldSemantics verifies that a proxied exchange never
// claims origin-level facts it cannot observe: serverIPAddress is omitted
// (the TCP peer is the proxy), while the proxy connection details stay
// available under "_network".
func TestProxySuccessFieldSemantics(t *testing.T) {
	// A minimal HTTP proxy: it receives the absolute-form request and
	// answers directly, as the origin never needs to exist.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "" {
			t.Errorf("expected absolute-form proxy request, got %q", r.RequestURI)
		}
		w.Write([]byte("via proxy"))
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}

	base := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	resp, err := client.Get("http://origin-behind-proxy.invalid/res")
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	if body := mustReadAll(t, resp.Body); string(body) != "via proxy" {
		t.Fatalf("body = %q", body)
	}

	e := singleEntry(t, rec)
	if e.ServerIPAddress != "" {
		t.Errorf("serverIPAddress = %q; the origin IP is not observable through a proxy", e.ServerIPAddress)
	}
	if e.Network == nil || e.Network.Proxy == "" {
		t.Fatalf("network.proxy missing: %+v", e.Network)
	}
	if e.Network.Proxy != proxyURL.String() {
		t.Errorf("network.proxy = %q, want %q", e.Network.Proxy, proxyURL.String())
	}
	if !strings.Contains(e.Network.RemoteAddress, proxyURL.Host) {
		t.Errorf("network.remoteAddress = %q, want the proxy %q", e.Network.RemoteAddress, proxyURL.Host)
	}
	if e.Request.HTTPVersion != "HTTP/1.1" {
		t.Errorf("httpVersion = %q (response was received, so it is known)", e.Request.HTTPVersion)
	}
}

func TestProxyURLRedacted(t *testing.T) {
	proxyAddr := closedPortAddr(t)
	proxyURL, err := url.Parse("http://proxy-user:proxy-secret@" + proxyAddr + "?token=query-secret&keep=1")
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	base := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec, WithRedactQueryParameters("token"))}

	_, err = client.Get("http://recorder-proxy-target.invalid/") //nolint:bodyclose
	if err == nil {
		t.Fatal("expected proxy failure")
	}
	e := singleEntry(t, rec)
	if e.Network == nil {
		t.Fatal("network info missing")
	}
	if strings.Contains(e.Network.Proxy, "proxy-secret") || strings.Contains(e.Network.Proxy, "query-secret") {
		t.Fatalf("network.proxy leaked secrets: %q", e.Network.Proxy)
	}
	if !strings.Contains(e.Network.Proxy, "proxy-user:%5BREDACTED%5D@") || !strings.Contains(e.Network.Proxy, "token=%5BREDACTED%5D") {
		t.Errorf("network.proxy = %q, want redacted password and query", e.Network.Proxy)
	}
}

func TestMalformedResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		conn.Read(buf)
		conn.Write([]byte("HTTP/1.1 pigeon status\r\n\r\n"))
	}()

	base := &http.Transport{}
	defer base.CloseIdleConnections()
	rec := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(base, rec)}

	_, err = client.Get("http://" + ln.Addr().String() + "/") //nolint:bodyclose
	if err == nil {
		t.Fatal("expected malformed response error")
	}
	e := singleEntry(t, rec)
	if e.Error == nil {
		t.Fatal("missing _error")
	}
	if p := e.Error.Phase; p != PhaseReadResponseHeaders && p != PhaseWaitResponse {
		t.Errorf("phase = %q, want read_response_headers or wait_response", p)
	}
}

func TestCallerRequestNotMutated(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer ts.Close()
	client, _ := newRecordedClient(ts)

	body := io.NopCloser(strings.NewReader("data"))
	req, _ := http.NewRequest(http.MethodPost, ts.URL, nil)
	req.Body = body
	req.ContentLength = 4
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	mustReadAll(t, resp.Body)

	if req.Body != body {
		t.Errorf("caller's request body was replaced")
	}
	if _, ok := req.Header["Authorization"]; ok {
		t.Errorf("caller's headers mutated")
	}
}

// selfSignedCert generates a throwaway self-signed server certificate valid
// only for the given DNS name.
func selfSignedCert(t *testing.T, dnsName string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, parsed
}

// TestWireHeadersRecorded: request.headers must come from the header fields
// the transport actually wrote (httptrace.WroteHeaderField) — including
// transport-added ones — with redaction still applied.
func TestWireHeadersRecorded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	// These are added by the transport at write time and are invisible in
	// req.Header — their presence proves the wire-derived path.
	for _, name := range []string{"User-Agent", "Accept-Encoding", "Host"} {
		if _, ok := findHeader(e.Request.Headers, name); !ok {
			t.Errorf("wire header %s missing in %+v", name, e.Request.Headers)
		}
	}
	if v, _ := findHeader(e.Request.Headers, "Authorization"); v != redactedValue {
		t.Errorf("Authorization = %q on the wire-derived list", v)
	}
}

func TestExpect100Continue(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // reading the body triggers the 100 Continue
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)
	client.Transport.(*Transport).Base.(*http.Transport).ExpectContinueTimeout = 2 * time.Second

	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("deferred body"))
	req.Header.Set("Expect", "100-continue")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if e.Expect100 == nil {
		t.Fatal("_expect100 missing")
	}
	if !e.Expect100.Waited || !e.Expect100.ContinueReceived {
		t.Errorf("expect100 = %+v", e.Expect100)
	}
	if e.Expect100.WaitMS < 0 {
		t.Errorf("waitMs = %v", e.Expect100.WaitMS)
	}
	if e.RequestBody == nil || !e.RequestBody.Complete {
		t.Errorf("request body not sent after 100: %+v", e.RequestBody)
	}
}

func TestInformationalResponsesRecorded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.Header().Set("X-Api-Key", "hint-secret")
		w.WriteHeader(http.StatusEarlyHints) // 103, sent immediately
		w.Header().Del("Link")
		w.Header().Del("X-Api-Key")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("final"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("final status = %d", resp.StatusCode)
	}

	e := singleEntry(t, rec)
	if len(e.Informational) != 1 || e.Informational[0].Status != 103 {
		t.Fatalf("_informational = %+v", e.Informational)
	}
	if v, ok := findHeader(e.Informational[0].Headers, "Link"); !ok || !strings.Contains(v, "preload") {
		t.Errorf("early hint headers = %+v", e.Informational[0].Headers)
	}
	if v, _ := findHeader(e.Informational[0].Headers, "X-Api-Key"); v != redactedValue {
		t.Errorf("interim response headers must be redacted, got %q", v)
	}
	if e.Response.Status != 200 {
		t.Errorf("final response corrupted: %d", e.Response.Status)
	}
}

// TestEmbedBodiesDisabled: production mode — metadata, hash and store
// reference only; no body text inside the HAR.
func TestEmbedBodiesDisabled(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("response payload"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts,
		WithEmbedBodies(false),
		WithBodyStore(FileBodyStore{Dir: dir}),
	)

	payload := []byte(`{"data":"request payload"}`)
	resp, err := client.Post(ts.URL, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if e.Request.PostData != nil {
		t.Errorf("postData embedded despite EmbedBodies=false: %+v", e.Request.PostData)
	}
	if e.Response.Content.Text != "" {
		t.Errorf("content.text embedded: %q", e.Response.Content.Text)
	}
	// Everything else must survive: sizes, hashes, store references.
	if e.Request.BodySize != int64(len(payload)) || e.Response.Content.Size != 16 {
		t.Errorf("sizes = %d/%d", e.Request.BodySize, e.Response.Content.Size)
	}
	if e.RequestBody.Hash != sha256Hex(payload) {
		t.Errorf("request hash missing")
	}
	for _, bi := range []*BodyInfo{e.RequestBody, e.ResponseBody} {
		if bi == nil || bi.Store == "" {
			t.Fatalf("store reference missing: %+v", bi)
		}
		if _, err := os.Stat(bi.Store); err != nil {
			t.Errorf("spool file missing: %v", err)
		}
	}
}

func TestNetworkExtrasMapping(t *testing.T) {
	tr := NewTransport(nil, NewMemoryRecorder())
	req, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	ex := tr.newExchange(req)
	v := traceView{
		dnsCoalesced: true,
		putIdle:      &putIdleResult{returned: false, err: errors.New("pool full")},
	}
	n := ex.buildNetwork(v, "HTTP/1.1")
	if !n.DNSCoalesced {
		t.Errorf("dnsCoalesced not mapped")
	}
	if n.PutIdle == nil || n.PutIdle.Returned || !strings.Contains(n.PutIdle.Error, "pool full") {
		t.Errorf("putIdle = %+v", n.PutIdle)
	}
	if n = ex.buildNetwork(traceView{putIdle: &putIdleResult{returned: true}}, ""); n.PutIdle == nil || !n.PutIdle.Returned || n.PutIdle.Error != "" {
		t.Errorf("putIdle success = %+v", n.PutIdle)
	}
	if n = ex.buildNetwork(traceView{}, ""); n.PutIdle != nil {
		t.Errorf("putIdle set without observation")
	}
}
