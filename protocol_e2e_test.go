package recorder_test

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // WebSocket handshake requires SHA-1.
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	rec "github.com/mgurevin/recorder"
)

// TestProtocolE2E exercises Recorder through real net/http clients and loopback
// servers. The table deliberately runs the same streaming and trailer contract
// over HTTP/1.1 and HTTP/2 so protocol-specific behavior cannot silently drift.
func TestProtocolE2E(t *testing.T) {
	for _, mode := range []struct {
		name  string
		http2 bool
	}{
		{name: "http1"},
		{name: "http2", http2: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("server_sent_events", func(t *testing.T) {
				testSSERecording(t, mode.http2)
			})

			t.Run("request_and_response_trailers", func(t *testing.T) {
				testTrailerRecording(t, mode.http2)
			})

			t.Run("concurrent_streams", func(t *testing.T) {
				testConcurrentProtocolRecording(t, mode.http2)
			})
		})
	}

	t.Run("websocket_upgrade_boundary", testUpgradeRecording)
	t.Run("network_failures", testNetworkFailureRecording)
	t.Run("malformed_http1", testMalformedHTTP1Recording)
}

func testSSERecording(t *testing.T, enableHTTP2 bool) {
	t.Helper()

	const events = "event: ready\ndata: one\n\ndata: two\n\n"

	server := newProtocolServer(t, enableHTTP2, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")

		for _, event := range []string{"event: ready\ndata: one\n\n", "data: two\n\n"} {
			testWriteString(writer, event)

			if err := http.NewResponseController(writer).Flush(); err != nil {
				panic(fmt.Errorf("flush SSE event: %w", err))
			}
		}
	}))

	client, recorder := newRecordedClient(server)

	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	if body := string(mustReadAll(t, response.Body)); body != events {
		t.Fatalf("caller body = %q, want %q", body, events)
	}

	entry := singleEntry(t, recorder)
	assertCompletedProtocolEntry(t, entry, enableHTTP2)

	if entry.Response.Content.MimeType != "text/event-stream" || entry.Response.Content.Text != events {
		t.Errorf("recorded SSE content = %+v", entry.Response.Content)
	}
}

func testTrailerRecording(t *testing.T, enableHTTP2 bool) {
	t.Helper()

	serverSawTrailer := make(chan string, 1)
	server := newProtocolServer(t, enableHTTP2, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		testCopy(io.Discard, request.Body)

		serverSawTrailer <- request.Trailer.Get("X-Request-Checksum")

		writer.Header().Set("Trailer", "X-Response-Checksum")
		testWriteString(writer, "ack")
		writer.Header().Set("X-Response-Checksum", "response-sha")
	}))

	client, recorder := newRecordedClient(server)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}

	request.ContentLength = -1
	request.Trailer = http.Header{"X-Request-Checksum": []string{"request-sha"}}

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if body := string(mustReadAll(t, response.Body)); body != "ack" {
		t.Fatalf("caller body = %q", body)
	}

	if got := <-serverSawTrailer; got != "request-sha" {
		t.Fatalf("server request trailer = %q", got)
	}

	entry := singleEntry(t, recorder)
	assertCompletedProtocolEntry(t, entry, enableHTTP2)
	assertRecordedTrailer(t, entry.Recorder.RequestTrailers, "X-Request-Checksum", "request-sha")
	assertRecordedTrailer(t, entry.Recorder.ResponseTrailers, "X-Response-Checksum", "response-sha")
}

func testConcurrentProtocolRecording(t *testing.T, enableHTTP2 bool) {
	t.Helper()

	server := newProtocolServer(t, enableHTTP2, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		testWriteString(writer, request.URL.Query().Get("id"))
	}))

	client, recorder := newRecordedClient(server)

	const requests = 12

	errCh := make(chan error, requests)

	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)

		go func() {
			defer wait.Done()

			want := fmt.Sprintf("%d", index)

			response, err := client.Get(server.URL + "?id=" + want)
			if err != nil {
				errCh <- err

				return
			}

			body, err := io.ReadAll(response.Body)
			closeErr := response.Body.Close()

			switch {
			case err != nil:
				errCh <- err

			case closeErr != nil:
				errCh <- closeErr

			case string(body) != want:
				errCh <- fmt.Errorf("body = %q, want %q", body, want)
			}
		}()
	}

	wait.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}

	entries := recorder.Entries()
	if len(entries) != requests {
		t.Fatalf("entries = %d, want %d", len(entries), requests)
	}

	for _, entry := range entries {
		assertCompletedProtocolEntry(t, entry, enableHTTP2)
	}
}

func testUpgradeRecording(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, buffered, err := http.NewResponseController(writer).Hijack()
		if err != nil {
			panic(fmt.Errorf("hijack: %w", err))
		}
		defer testClose(conn)

		accept := websocketAccept(request.Header.Get("Sec-WebSocket-Key"))
		testWriteString(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")

		if err := buffered.Flush(); err != nil {
			panic(fmt.Errorf("flush upgrade: %w", err))
		}

		message, err := readWebSocketTextFrame(buffered, true)
		if err != nil {
			panic(fmt.Errorf("read WebSocket frame: %w", err))
		}

		writeWebSocketTextFrame(buffered, []byte("pong:"+string(message)), nil)

		if err := buffered.Flush(); err != nil {
			panic(fmt.Errorf("flush upgraded stream: %w", err))
		}
	}))
	defer server.Close()

	client, recorder := newRecordedClient(server)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Version", "13")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	stream, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("upgraded body type = %T, want io.ReadWriteCloser", response.Body)
	}

	writeWebSocketTextFrame(stream, []byte("ping"), &[4]byte{1, 2, 3, 4})

	message, err := readWebSocketTextFrame(bufio.NewReader(stream), false)
	if err != nil {
		t.Fatalf("read WebSocket frame: %v", err)
	}

	if string(message) != "pong:ping" {
		t.Fatalf("WebSocket payload = %q", message)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close upgraded stream: %v", err)
	}

	entry := singleEntry(t, recorder)
	if entry.Response.Status != http.StatusSwitchingProtocols || entry.Recorder.State != rec.StateCompleted {
		t.Fatalf("upgrade entry = status %d, state %q", entry.Response.Status, entry.Recorder.State)
	}

	if entry.Recorder.ResponseBody.TotalBytes != 0 || entry.Response.Content.Size != 0 {
		t.Fatalf("upgraded protocol bytes were recorded as HTTP body: %+v", entry.Recorder.ResponseBody)
	}
}

func testNetworkFailureRecording(t *testing.T) {
	t.Run("dns", func(t *testing.T) {
		base := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.DNSError{Err: "fixture failure", Name: "broken.test", IsNotFound: true}
		}}
		defer base.CloseIdleConnections()

		assertTransportFailure(t, base, "http://broken.test/", rec.PhaseDNS, false)
	})

	t.Run("connect_timeout", func(t *testing.T) {
		base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.ConnectStart != nil {
				trace.ConnectStart(network, address)
			}

			<-ctx.Done()

			return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
		}}
		defer base.CloseIdleConnections()

		assertTransportFailure(t, base, "http://192.0.2.1/", rec.PhaseConnect, true)
	})

	t.Run("tls_certificate", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()

		server.Config.ErrorLog = testDiscardLogger()

		base := &http.Transport{}
		defer base.CloseIdleConnections()

		assertTransportFailure(t, base, server.URL, rec.PhaseTLS, false)
	})
}

func testMalformedHTTP1Recording(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		response string
		doError  bool
	}{
		{name: "invalid_status", response: "HTTP/1.1 pigeon\r\n\r\n", doError: true},
		{name: "truncated_fixed_body", response: "HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort"},
		{name: "invalid_chunk", response: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nZZ\r\n"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			address := startRawHTTPFixture(t, scenario.response)

			base := &http.Transport{}
			defer base.CloseIdleConnections()

			recorder := rec.NewMemoryRecorder()
			client := &http.Client{Transport: rec.NewTransport(base, recorder, rec.DefaultConfig())}

			response, err := client.Get("http://" + address + "/")
			if scenario.doError {
				if err == nil {
					t.Fatal("expected RoundTrip error")
				}
			} else {
				if err != nil {
					t.Fatalf("GET: %v", err)
				}

				_, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()

				if readErr == nil {
					t.Fatal("expected body framing error")
				}
			}

			entry := singleEntry(t, recorder)
			if entry.Recorder.State != rec.StateFailed || entry.Recorder.Error == nil {
				t.Fatalf("malformed response entry = %+v", entry.Recorder)
			}
		})
	}
}

func newProtocolServer(t *testing.T, enableHTTP2 bool, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = enableHTTP2

	if enableHTTP2 {
		server.StartTLS()
	} else {
		server.Start()
	}

	t.Cleanup(server.Close)

	return server
}

func assertCompletedProtocolEntry(t *testing.T, entry *rec.Entry, wantHTTP2 bool) {
	t.Helper()

	if entry.Recorder.State != rec.StateCompleted || entry.Recorder.Error != nil {
		t.Fatalf("entry = state %q, error %+v", entry.Recorder.State, entry.Recorder.Error)
	}

	wantProto := "HTTP/1.1"
	if wantHTTP2 {
		wantProto = "HTTP/2.0"
	}

	if entry.Request.HTTPVersion != wantProto || entry.Response.HTTPVersion != wantProto {
		t.Errorf("protocol = request %q, response %q, want %q", entry.Request.HTTPVersion, entry.Response.HTTPVersion, wantProto)
	}

	if entry.Recorder.Network.HTTP2 != wantHTTP2 {
		t.Errorf("network.http2 = %t, want %t", entry.Recorder.Network.HTTP2, wantHTTP2)
	}

	if entry.Recorder.ResponseBody == nil || !entry.Recorder.ResponseBody.Complete {
		t.Errorf("response body = %+v", entry.Recorder.ResponseBody)
	}
}

func assertRecordedTrailer(t *testing.T, trailers []rec.NameValuePair, name, value string) {
	t.Helper()

	if got, ok := findHeader(trailers, name); !ok || got != value {
		t.Errorf("trailer %s = %q, present %t", name, got, ok)
	}
}

func assertTransportFailure(t *testing.T, base *http.Transport, target, phase string, timeout bool) {
	t.Helper()

	recorder := rec.NewMemoryRecorder()
	client := &http.Client{Transport: rec.NewTransport(base, recorder, rec.DefaultConfig())}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(request) //nolint:bodyclose // failures return no usable response
	if err == nil {
		t.Fatal("expected transport failure")
	}

	entry := singleEntry(t, recorder)
	if entry.Recorder.State != rec.StateFailed || entry.Recorder.Error == nil || entry.Recorder.Error.Phase != phase {
		t.Fatalf("failure entry = %+v, want phase %q", entry.Recorder, phase)
	}

	if entry.Recorder.Error.Timeout != timeout {
		t.Errorf("timeout = %t, want %t", entry.Recorder.Error.Timeout, timeout)
	}
}

func startRawHTTPFixture(t *testing.T, response string) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || line == "\r\n" {
				break
			}
		}

		_, _ = io.WriteString(conn, response)
	}()

	return listener.Addr().String()
}

func testDiscardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func newRecordedClient(server *httptest.Server) (*http.Client, *rec.MemoryRecorder) {
	config := rec.DefaultConfig()
	config.CaptureRequestBody = true
	config.CaptureResponseBody = true
	config.EmbedBodies = true
	config.HashBodies = true
	config.BodyHashAlgorithm = "sha256"

	recorder := rec.NewMemoryRecorder()
	client := server.Client()
	client.Transport = rec.NewTransport(client.Transport, recorder, config)

	return client, recorder
}

func singleEntry(t *testing.T, recorder *rec.MemoryRecorder) *rec.Entry {
	t.Helper()

	entries := recorder.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	return entries[0]
}

func findHeader(headers []rec.NameValuePair, name string) (string, bool) {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value, true
		}
	}

	return "", false
}

func testWrite(writer io.Writer, data []byte) {
	if _, err := writer.Write(data); err != nil {
		panic(fmt.Errorf("test write: %w", err))
	}
}

func testWriteString(writer io.Writer, value string) {
	if _, err := io.WriteString(writer, value); err != nil {
		panic(fmt.Errorf("test write string: %w", err))
	}
}

func testCopy(destination io.Writer, source io.Reader) {
	if _, err := io.Copy(destination, source); err != nil {
		panic(fmt.Errorf("test copy: %w", err))
	}
}

func testClose(closer io.Closer) {
	if err := closer.Close(); err != nil {
		panic(fmt.Errorf("test close: %w", err))
	}
}

func mustReadAll(t *testing.T, body io.ReadCloser) []byte {
	t.Helper()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if err := body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	return data
}

func websocketAccept(key string) string {
	const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	digest := sha1.Sum([]byte(key + magic)) //nolint:gosec // Required by RFC 6455 section 4.2.2.

	return base64.StdEncoding.EncodeToString(digest[:])
}

func writeWebSocketTextFrame(writer io.Writer, payload []byte, mask *[4]byte) {
	if len(payload) > 125 {
		panic("test WebSocket payload exceeds the compact frame limit")
	}

	header := []byte{0x81, byte(len(payload))}
	if mask != nil {
		header[1] |= 0x80
		header = append(header, mask[:]...)
	}

	testWrite(writer, header)

	if mask == nil {
		testWrite(writer, payload)

		return
	}

	masked := append([]byte(nil), payload...)
	for index := range masked {
		masked[index] ^= mask[index%len(mask)]
	}

	testWrite(writer, masked)
}

func readWebSocketTextFrame(reader io.Reader, wantMasked bool) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}

	if header[0] != 0x81 {
		return nil, fmt.Errorf("frame opcode/flags = %#x", header[0])
	}

	masked := header[1]&0x80 != 0
	if masked != wantMasked {
		return nil, fmt.Errorf("frame masked = %t, want %t", masked, wantMasked)
	}

	length := int(header[1] & 0x7f)
	if length > 125 {
		return nil, fmt.Errorf("unsupported fixture frame length %d", length)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return nil, err
		}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}

	if masked {
		for index := range payload {
			payload[index] ^= mask[index%len(mask)]
		}
	}

	return payload, nil
}
