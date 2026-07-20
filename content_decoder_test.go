package recorder

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func gzipBytes(t *testing.T, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(plain)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	gz.Close()
	return buf.Bytes()
}

// TestManualGzipDecodedForRecord: the caller advertises gzip itself, so the
// transport hands compressed bytes through — the record must still store the
// decoded text while every wire-level fact keeps the compressed view.
func TestManualGzipDecodedForRecord(t *testing.T) {
	const plain = "manually negotiated gzip content"
	wire := gzipBytes(t, plain)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		w.Write(wire)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip") // manual: transport won't decompress
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := mustReadAll(t, resp.Body)
	if !bytes.Equal(body, wire) {
		t.Fatalf("caller must receive the raw compressed bytes")
	}

	e := singleEntry(t, rec)
	c := e.Response.Content
	if c.Text != plain || c.Encoding != "" {
		t.Errorf("content = %q (encoding %q), want decoded text", c.Text, c.Encoding)
	}
	if !c.Decoded {
		t.Errorf("_decoded flag not set")
	}
	if c.Size != int64(len(plain)) {
		t.Errorf("content.size = %d, want decoded length %d", c.Size, len(plain))
	}
	if e.Response.BodySize != int64(len(wire)) {
		t.Errorf("bodySize = %d, want wire length %d", e.Response.BodySize, len(wire))
	}
	if want := c.Size - e.Response.BodySize; c.Compression != want {
		t.Errorf("compression = %d, want %d bytes saved", c.Compression, want)
	}
	// Hash and stream counters describe the wire bytes, not the decoded form.
	if e.ResponseBody.Hash != sha256Hex(wire) || e.ResponseBody.TotalBytes != int64(len(wire)) {
		t.Errorf("wire accounting altered by decoding: %+v", e.ResponseBody)
	}
}

// TestCustomContentDecoder registers a decoder for a made-up encoding,
// standing in for brotli/zstd wired up by the user.
func TestCustomContentDecoder(t *testing.T) {
	const plain = `{"password":"hunter2","ok":true}`
	dir := t.TempDir()
	xor := func(b []byte) []byte {
		out := make([]byte, len(b))
		for i, c := range b {
			out[i] = c ^ 0x5A
		}
		return out
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "x-xor")
		w.Header().Set("Content-Type", "application/json")
		w.Write(xor([]byte(plain)))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts,
		WithRedactJSONFields("password"),
		WithBodyStore(FileBodyStore{Dir: dir}),
		// Registered with different casing to prove case-insensitivity.
		WithContentDecoder("X-XOR", func(r io.Reader) (io.ReadCloser, error) {
			b, err := io.ReadAll(r)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(bytes.NewReader(xor(b))), nil
		}),
	)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	c := e.Response.Content
	if !c.Decoded || c.Encoding != "" {
		t.Fatalf("content not decoded: %+v", c)
	}
	// Structured redaction must run on the *decoded* JSON.
	if strings.Contains(c.Text, "hunter2") || !strings.Contains(c.Text, redactedValue) {
		t.Errorf("redaction did not reach decoded content: %q", c.Text)
	}
	if !strings.Contains(c.Text, `"ok":true`) {
		t.Errorf("decoded content mangled: %q", c.Text)
	}
	stored, err := os.ReadFile(e.ResponseBody.Store)
	if err != nil {
		t.Fatalf("read decoded store: %v", err)
	}
	if string(stored) != c.Text {
		t.Errorf("stored body = %q, want decoded/redacted %q", stored, c.Text)
	}
}

func TestStreamingCompressedFormRedaction(t *testing.T) {
	const plain = `keep=yes&token=compressed-secret`
	wire := gzipBytes(t, plain)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		w.Write(wire)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithRedactQueryParameters("token"))
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := mustReadAll(t, resp.Body); !bytes.Equal(got, wire) {
		t.Fatal("caller-visible compressed bytes changed")
	}
	e := singleEntry(t, rec)
	if !e.Response.Content.Decoded {
		t.Fatal("compressed form not marked decoded")
	}
	if got, want := e.Response.Content.Text, `keep=yes&token=%5BREDACTED%5D`; got != want {
		t.Fatalf("decoded form = %q, want %q", got, want)
	}
	if e.ResponseBody.Hash != sha256Hex(wire) || e.ResponseBody.TotalBytes != int64(len(wire)) {
		t.Fatalf("wire accounting changed: %+v", e.ResponseBody)
	}
}

func TestStreamingCompressedMultipartRedaction(t *testing.T) {
	plain := multipartFixture("compressed-secret")
	wire := gzipBytes(t, plain)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", multipartTestType)
		w.Write(wire)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithRedactQueryParameters("token", "upload"))
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := mustReadAll(t, resp.Body); !bytes.Equal(got, wire) {
		t.Fatal("caller-visible compressed bytes changed")
	}
	e := singleEntry(t, rec)
	if !e.Response.Content.Decoded {
		t.Fatal("compressed multipart not marked decoded")
	}
	for _, leaked := range []string{"compressed-secret", "still-secret", "customer-123.pdf"} {
		if strings.Contains(e.Response.Content.Text, leaked) {
			t.Fatalf("decoded multipart leaked %q", leaked)
		}
	}
	if e.ResponseBody.Hash != sha256Hex(wire) || e.ResponseBody.TotalBytes != int64(len(wire)) {
		t.Fatalf("wire accounting changed: %+v", e.ResponseBody)
	}
}

// TestDecoderFailureFallsBackToWireBytes: a corrupt stream must leave the
// raw capture intact and surface through OnInternalError.
func TestDecoderFailureFallsBackToWireBytes(t *testing.T) {
	garbage := []byte("this is definitely not gzip")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(garbage)
	}))
	defer ts.Close()

	var mu sync.Mutex
	var internal []error
	client, rec := newRecordedClient(ts, WithOnInternalError(func(err error) {
		mu.Lock()
		internal = append(internal, err)
		mu.Unlock()
	}))

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	c := e.Response.Content
	if c.Decoded {
		t.Errorf("corrupt stream marked decoded")
	}
	decoded, err := base64.StdEncoding.DecodeString(c.Text)
	if err != nil || !bytes.Equal(decoded, garbage) {
		t.Errorf("record must keep the raw wire bytes: %v", err)
	}
	mu.Lock()
	n := len(internal)
	mu.Unlock()
	if n == 0 {
		t.Errorf("decoder failure not reported through OnInternalError")
	}
}

func TestStreamingRedactionUnknownEncodingFailsClosed(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "x-unknown")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"password":"must-not-reach-store"}`))
	}))
	defer ts.Close()
	var internal []error
	client, rec := newRecordedClient(ts,
		WithBodyStore(FileBodyStore{Dir: dir}),
		WithRedactJSONFields("password"),
		WithOnInternalError(func(err error) { internal = append(internal, err) }),
	)
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "x-unknown")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)
	e := singleEntry(t, rec)
	if e.ResponseBody.Store != "" {
		t.Fatalf("unknown encoding persisted raw body: %q", e.ResponseBody.Store)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("store directory = %v, %v; want empty", files, err)
	}
	if len(internal) == 0 {
		t.Fatal("missing internal error for unavailable streaming decoder")
	}
}

func TestStreamingDecodeBombFailsClosed(t *testing.T) {
	dir := t.TempDir()
	plain := `{"password":"` + strings.Repeat("secret", 50_000) + `"}`
	wire := gzipBytes(t, plain)
	if len(wire) > 1024 {
		t.Fatalf("test wire body too large: %d", len(wire))
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.Write(wire)
	}))
	defer ts.Close()
	var internal []error
	client, rec := newRecordedClient(ts,
		WithBodyStore(FileBodyStore{Dir: dir}),
		WithRedactJSONFields("password"),
		WithMaxResponseBodyBytes(1024),
		WithOnInternalError(func(err error) { internal = append(internal, err) }),
	)
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := mustReadAll(t, resp.Body); !bytes.Equal(got, wire) {
		t.Fatal("caller-visible compressed body changed")
	}
	e := singleEntry(t, rec)
	if e.ResponseBody.Store != "" {
		stored, err := os.ReadFile(e.ResponseBody.Store)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) > 1024 || bytes.Contains(stored, []byte("secret")) {
			t.Fatalf("unsafe decoded bomb store: %d bytes", len(stored))
		}
	}
	if len(internal) == 0 {
		t.Fatal("decoded bomb did not report an internal error")
	}
}

// TestDecodedBombRespectsCaptureBudget: tiny compressed input expanding far
// beyond MaxResponseBodyBytes must not be inflated; the record keeps the
// wire bytes.
func TestDecodedBombRespectsCaptureBudget(t *testing.T) {
	wire := gzipBytes(t, strings.Repeat("a", 100_000)) // ~hundreds of bytes on the wire
	if len(wire) > 512 {
		t.Fatalf("test setup: wire = %d bytes", len(wire))
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		w.Write(wire)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithMaxResponseBodyBytes(1024))

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	c := e.Response.Content
	if c.Decoded {
		t.Errorf("bomb was decoded past the capture budget")
	}
	if decoded, err := base64.StdEncoding.DecodeString(c.Text); err != nil || !bytes.Equal(decoded, wire) {
		t.Errorf("record must keep the wire bytes")
	}
}

func TestDeflateDecoderHandlesZlibAndRawStreams(t *testing.T) {
	const plain = "hello deflate world"

	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write([]byte(plain))
	zw.Close()

	var fbuf bytes.Buffer
	fw, _ := flate.NewWriter(&fbuf, flate.DefaultCompression)
	fw.Write([]byte(plain))
	fw.Close()

	for name, wire := range map[string][]byte{"zlib-wrapped": zbuf.Bytes(), "raw-flate": fbuf.Bytes()} {
		rc, err := DeflateDecoder(bytes.NewReader(wire))
		if err != nil {
			t.Fatalf("%s: open: %v", name, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(got) != plain {
			t.Errorf("%s: got %q, err %v", name, got, err)
		}
	}
}

// TestMultiStepEncodingNotDecoded: "gzip, br" style chains are left as raw
// wire bytes rather than half-decoded.
func TestMultiStepEncodingNotDecoded(t *testing.T) {
	wire := gzipBytes(t, "layered")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br, gzip")
		w.Write(wire)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)

	if c := singleEntry(t, rec).Response.Content; c.Decoded {
		t.Errorf("multi-step encoding must not be partially decoded: %+v", c)
	}
}
