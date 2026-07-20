package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runSampleTraffic drives a mixed workload (success, JSON POST, 404,
// transport failure) through one recorder.
func runSampleTraffic(t *testing.T, rec Recorder, opts ...Option) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "v", Path: "/", Expires: time.Now().Add(time.Hour)})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := ts.Client()
	client.Transport = NewTransport(client.Transport, rec, opts...)

	for _, path := range []string{"/ok", "/missing"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	resp, err := client.Post(ts.URL+"/ok", "application/json", strings.NewReader(`{"in":1}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// One failed exchange (connection refused) through the same recorder.
	failClient := &http.Client{Transport: NewTransport(&http.Transport{}, rec, opts...)}
	failClient.Get("http://" + closedPortAddr(t) + "/") //nolint:bodyclose,errcheck
}

// validateHAR structurally checks a serialized document against the HAR 1.2
// requirements this library promises, independent of the Go structs.
func validateHAR(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("HAR is not valid JSON: %v", err)
	}
	logObj := requireMap(t, doc, "log")
	if v, _ := logObj["version"].(string); v != "1.2" {
		t.Fatalf("log.version = %v", logObj["version"])
	}
	creator := requireMap(t, logObj, "creator")
	if got, ok := creator["name"].(string); !ok || got != "github.com/mgurevin/recorder" {
		t.Fatalf("creator.name = %#v, want %q", creator["name"], "github.com/mgurevin/recorder")
	}
	if got, ok := creator["version"].(string); !ok || got != "0.2.1" {
		t.Fatalf("creator.version = %#v, want %q", creator["version"], "0.2.1")
	}
	entries, ok := logObj["entries"].([]any)
	if !ok {
		t.Fatalf("log.entries is not an array: %T", logObj["entries"])
	}
	var prev time.Time
	for i, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("entry %d not an object", i)
		}
		started, _ := entry["startedDateTime"].(string)
		st, err := time.Parse(time.RFC3339, started)
		if err != nil {
			t.Fatalf("entry %d startedDateTime %q: %v", i, started, err)
		}
		if st.Before(prev) {
			t.Errorf("entries not ordered by start time at index %d", i)
		}
		prev = st
		if tt := requireNumber(t, entry, "time", i); tt < 0 {
			t.Errorf("entry %d time = %v", i, tt)
		}

		req := requireMap(t, entry, "request")
		for _, key := range []string{"method", "url", "httpVersion"} {
			if _, ok := req[key].(string); !ok {
				t.Errorf("entry %d request.%s missing", i, key)
			}
		}
		for _, key := range []string{"cookies", "headers", "queryString"} {
			if _, ok := req[key].([]any); !ok {
				t.Errorf("entry %d request.%s is not an array", i, key)
			}
		}
		requireNumber(t, req, "headersSize", i)
		requireNumber(t, req, "bodySize", i)
		for _, h := range req["headers"].([]any) {
			pair := h.(map[string]any)
			if _, ok := pair["name"].(string); !ok {
				t.Errorf("entry %d header without name", i)
			}
			if _, ok := pair["value"].(string); !ok {
				t.Errorf("entry %d header without value", i)
			}
		}

		resp := requireMap(t, entry, "response")
		requireNumber(t, resp, "status", i)
		for _, key := range []string{"statusText", "httpVersion", "redirectURL"} {
			if _, ok := resp[key].(string); !ok {
				t.Errorf("entry %d response.%s missing", i, key)
			}
		}
		for _, key := range []string{"cookies", "headers"} {
			if _, ok := resp[key].([]any); !ok {
				t.Errorf("entry %d response.%s is not an array", i, key)
			}
		}
		requireNumber(t, resp, "headersSize", i)
		requireNumber(t, resp, "bodySize", i)
		content := requireMap(t, resp, "content")
		requireNumber(t, content, "size", i)
		if _, ok := content["mimeType"].(string); !ok {
			t.Errorf("entry %d content.mimeType missing", i)
		}

		if _, ok := entry["cache"].(map[string]any); !ok {
			t.Errorf("entry %d cache missing", i)
		}
		timings := requireMap(t, entry, "timings")
		for _, key := range []string{"blocked", "dns", "connect", "send", "wait", "receive", "ssl"} {
			v := requireNumber(t, timings, key, i)
			if v < -1 {
				t.Errorf("entry %d timings.%s = %v", i, key, v)
			}
		}
	}
	return doc
}

func requireMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object: %T", key, m[key])
	}
	return v
}

func requireNumber(t *testing.T, m map[string]any, key string, i int) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("entry %d: %q is not a number: %T", i, key, m[key])
	}
	return v
}

// stripExtensions removes every "_"-prefixed key recursively.
func stripExtensions(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.HasPrefix(k, "_") {
				delete(t, k)
				continue
			}
			t[k] = stripExtensions(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = stripExtensions(val)
		}
		return t
	default:
		return v
	}
}

func countExtensionKeys(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.HasPrefix(k, "_") {
				n++
			}
			n += countExtensionKeys(val)
		}
	case []any:
		for _, val := range t {
			n += countExtensionKeys(val)
		}
	}
	return n
}

func TestHARExportValidatesAndSurvivesExtensionStripping(t *testing.T) {
	rec := NewMemoryRecorder()
	runSampleTraffic(t, rec, WithCaptureRawTrace(true))
	if rec.Len() != 4 {
		t.Fatalf("entries = %d, want 4", rec.Len())
	}

	var buf bytes.Buffer
	if err := rec.WriteHAR(&buf); err != nil {
		t.Fatalf("WriteHAR: %v", err)
	}
	doc := validateHAR(t, buf.Bytes())

	// The document must carry extensions (we recorded a failed exchange)...
	if countExtensionKeys(doc) == 0 {
		t.Fatalf("no extension fields present")
	}
	// ...and stripping every extension must leave a valid HAR 1.2 document.
	stripped := stripExtensions(doc)
	if countExtensionKeys(stripped) != 0 {
		t.Fatalf("extensions survived stripping")
	}
	data, err := json.Marshal(stripped)
	if err != nil {
		t.Fatalf("marshal stripped: %v", err)
	}
	validateHAR(t, data)
}

func TestHARExportDeterministic(t *testing.T) {
	rec := NewMemoryRecorder()
	runSampleTraffic(t, rec)

	har := rec.HAR()
	a, err := json.MarshalIndent(har, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.MarshalIndent(rec.HAR(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("two exports of the same recording differ")
	}

	var buf bytes.Buffer
	if err := rec.WriteHAR(&buf); err != nil {
		t.Fatalf("WriteHAR: %v", err)
	}
	if !bytes.Equal(bytes.TrimRight(buf.Bytes(), "\n"), a) {
		t.Fatalf("WriteHAR and MarshalIndent disagree")
	}
}

func TestInFlightEntriesAbsentUntilFinalized(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.Write([]byte("12345"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("67890"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	// The body is mid-stream: the exchange must not be visible yet.
	if rec.Len() != 0 {
		t.Fatalf("in-flight exchange leaked into the recorder")
	}
	close(release)
	mustReadAll(t, resp.Body)
	if rec.Len() != 1 {
		t.Fatalf("entries = %d after completion", rec.Len())
	}
	if e := rec.Entries()[0]; e.State != StateCompleted || e.ResponseBody.TotalBytes != 10 {
		t.Fatalf("entry = %q %+v", e.State, e.ResponseBody)
	}
}

func TestHARFileRecorder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.har")
	rec := NewHARFileRecorder(path)
	runSampleTraffic(t, rec)
	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	doc := validateHAR(t, data)
	entries := doc["log"].(map[string]any)["entries"].([]any)
	if len(entries) != 4 {
		t.Fatalf("entries in file = %d", len(entries))
	}
}

func TestJSONStreamRecorder(t *testing.T) {
	var buf bytes.Buffer
	rec := NewJSONStreamRecorder(&buf)
	runSampleTraffic(t, rec)
	if err := rec.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, line := range lines {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
		if _, ok := e["startedDateTime"].(string); !ok {
			t.Errorf("line %d missing startedDateTime", i)
		}
	}
}

func TestCallbackRecorderAndOnEntryCompleted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	var recorded, completed []*Entry
	var completedTraceID string
	client := ts.Client()
	client.Transport = NewTransport(client.Transport,
		RecorderFunc(func(e *Entry) { recorded = append(recorded, e) }),
		WithOnEntryCompleted(func(ctx context.Context, e *Entry) {
			completed = append(completed, e)
			completedTraceID, _ = TraceIDFromContext(ctx)
		}),
	)

	ctx := WithTraceID(context.Background(), "cb-trace")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	mustReadAll(t, resp.Body)

	if len(recorded) != 1 || len(completed) != 1 || recorded[0] != completed[0] {
		t.Fatalf("recorded=%d completed=%d", len(recorded), len(completed))
	}
	if recorded[0].TraceID != "cb-trace" || completedTraceID != "cb-trace" {
		t.Errorf("trace ids: entry=%q ctx=%q", recorded[0].TraceID, completedTraceID)
	}
}

func TestFileBodyStore(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("spooled body"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithBodyStore(FileBodyStore{Dir: dir}))

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if e.Response.Content.Text != "spooled body" {
		t.Errorf("content = %q", e.Response.Content.Text)
	}
	if e.ResponseBody.Store == "" || !strings.HasPrefix(e.ResponseBody.Store, dir) {
		t.Errorf("store ref = %q", e.ResponseBody.Store)
	}
	if _, err := os.Stat(e.ResponseBody.Store); err != nil {
		t.Errorf("spool file missing: %v", err)
	}
}

func TestFileBodyStoreStreamsRedactedBodiesWithoutEmbedding(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<response><password>response-secret</password><keep>yes</keep></response>`))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts,
		WithEmbedBodies(false),
		WithBodyStore(FileBodyStore{Dir: dir}),
		WithRedactJSONFields("password"),
		WithRedactXMLElements("password"),
	)
	payload := []byte(`{"password":"request-secret","keep":"yes"}`)
	resp, err := client.Post(ts.URL, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if e.Request.PostData != nil || e.Response.Content.Text != "" {
		t.Fatal("bodies unexpectedly embedded")
	}
	for _, tc := range []struct {
		name, path, secret string
	}{
		{"request", e.RequestBody.Store, "request-secret"},
		{"response", e.ResponseBody.Store, "response-secret"},
	} {
		stored, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s store: %v", tc.name, err)
		}
		if bytes.Contains(stored, []byte(tc.secret)) || !bytes.Contains(stored, []byte(redactedValue)) {
			t.Errorf("%s store leaked: %q", tc.name, stored)
		}
		if !bytes.Contains(stored, []byte("yes")) {
			t.Errorf("%s store lost safe bytes: %q", tc.name, stored)
		}
	}
}

func TestFileBodyStoreStreamsRedactedFormWithoutEmbedding(t *testing.T) {
	dir := t.TempDir()
	const payload = `keep=a+b&token=request-secret&T%4fKEN=second-secret`
	var serverGot string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		serverGot = string(body)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts,
		WithEmbedBodies(false),
		WithBodyStore(FileBodyStore{Dir: dir}),
		WithRedactQueryParameters("token"),
	)
	resp, err := client.Post(ts.URL, "application/x-www-form-urlencoded; charset=utf-8", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)
	if serverGot != payload {
		t.Fatalf("live request changed: %q", serverGot)
	}
	e := singleEntry(t, rec)
	if e.Request.PostData != nil {
		t.Fatal("form body unexpectedly embedded")
	}
	stored, err := os.ReadFile(e.RequestBody.Store)
	if err != nil {
		t.Fatalf("read request store: %v", err)
	}
	want := `keep=a+b&token=%5BREDACTED%5D&T%4fKEN=%5BREDACTED%5D`
	if string(stored) != want {
		t.Fatalf("stored form = %q, want %q", stored, want)
	}
}

func TestEmbeddedFormTextAndParamsAreRedacted(t *testing.T) {
	const payload = `keep=a+b&token=request-secret&T%4fKEN=second-secret&empty=`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithRedactQueryParameters("token"))
	resp, err := client.Post(ts.URL, "application/x-www-form-urlencoded", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)
	pd := singleEntry(t, rec).Request.PostData
	if pd == nil {
		t.Fatal("missing postData")
	}
	wantText := `keep=a+b&token=%5BREDACTED%5D&T%4fKEN=%5BREDACTED%5D&empty=`
	if pd.Text != wantText {
		t.Fatalf("postData.text = %q, want %q", pd.Text, wantText)
	}
	if len(pd.Params) != 4 {
		t.Fatalf("postData.params = %+v", pd.Params)
	}
	for _, p := range pd.Params {
		if strings.EqualFold(p.Name, "token") && p.Value != redactedValue {
			t.Fatalf("parameter leaked: %+v", p)
		}
	}
}

func multipartRequestFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.SetBoundary("integration-boundary"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("keep", "safe-value"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("token", "request-secret"); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("upload", "customer-123.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("file-secret")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), w.FormDataContentType()
}

func TestMultipartRedactionEndToEnd(t *testing.T) {
	payload, contentType := multipartRequestFixture(t)
	dir := t.TempDir()
	var serverGot []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverGot, _ = io.ReadAll(r.Body)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts,
		WithBodyStore(FileBodyStore{Dir: dir}),
		WithRedactQueryParameters("token", "upload"),
	)
	resp, err := client.Post(ts.URL, contentType, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)
	if !bytes.Equal(serverGot, payload) {
		t.Fatal("multipart redaction changed the live request")
	}
	e := singleEntry(t, rec)
	stored, err := os.ReadFile(e.RequestBody.Store)
	if err != nil {
		t.Fatalf("read request store: %v", err)
	}
	for _, leaked := range []string{"request-secret", "file-secret", "customer-123.pdf"} {
		if bytes.Contains(stored, []byte(leaked)) || strings.Contains(e.Request.PostData.Text, leaked) {
			t.Fatalf("multipart leaked %q", leaked)
		}
	}
	if !bytes.Contains(stored, []byte("safe-value")) || !strings.Contains(e.Request.PostData.Text, "safe-value") {
		t.Fatal("multipart lost unmatched field")
	}
	if len(e.Request.PostData.Params) != 3 {
		t.Fatalf("postData.params = %+v", e.Request.PostData.Params)
	}
	for _, p := range e.Request.PostData.Params {
		switch p.Name {
		case "keep":
			if p.Value != "safe-value" {
				t.Fatalf("keep param = %+v", p)
			}
		case "token":
			if p.Value != redactedValue {
				t.Fatalf("token param = %+v", p)
			}
		case "upload":
			if p.FileName != redactedValue || p.Value != "" {
				t.Fatalf("upload param = %+v", p)
			}
		}
	}
}

func TestMultipartFileBodyStoreWithoutEmbedding(t *testing.T) {
	payload, contentType := multipartRequestFixture(t)
	dir := t.TempDir()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts,
		WithEmbedBodies(false),
		WithBodyStore(FileBodyStore{Dir: dir}),
		WithRedactQueryParameters("token", "upload"),
	)
	resp, err := client.Post(ts.URL, contentType, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	mustReadAll(t, resp.Body)
	e := singleEntry(t, rec)
	if e.Request.PostData != nil {
		t.Fatal("multipart unexpectedly embedded")
	}
	stored, err := os.ReadFile(e.RequestBody.Store)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"request-secret", "file-secret", "customer-123.pdf"} {
		if bytes.Contains(stored, []byte(leaked)) {
			t.Fatalf("file store leaked %q", leaked)
		}
	}
}

func TestNewTransportDefaults(t *testing.T) {
	tr := NewTransport(nil, nil)
	if tr.Recorder == nil {
		t.Fatal("nil recorder not defaulted")
	}
	if _, ok := tr.Recorder.(*MemoryRecorder); !ok {
		t.Fatalf("default recorder = %T", tr.Recorder)
	}
	if tr.Options.CaptureRequestBody || tr.Options.CaptureResponseBody || tr.Options.EmbedBodies || tr.Options.HashBodies || tr.Options.MaxResponseBodyBytes != 1<<20 {
		t.Errorf("defaults not applied: %+v", tr.Options)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("via default base"))
	}))
	defer ts.Close()
	client := &http.Client{Transport: tr} // nil Base -> http.DefaultTransport
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	mustReadAll(t, resp.Body)
	if tr.Recorder.(*MemoryRecorder).Len() != 1 {
		t.Fatalf("entry not recorded through defaults")
	}
}

func FuzzHARSerialization(f *testing.F) {
	f.Add("GET", "http://x/", 200, "text/plain", "body")
	f.Add("", "", 0, "", "")
	f.Add("POST", "https://u:p@h/p?q=1#frag", 599, "application/json", `{"a":1}`)
	f.Fuzz(func(t *testing.T, method, rawurl string, status int, mimeType, body string) {
		e := &Entry{
			StartedDateTime: time.Now().UTC().Format(harTimeFormat),
			Time:            1,
			Request: &Request{
				Method: method, URL: rawurl, HTTPVersion: "HTTP/1.1",
				Cookies: []Cookie{}, Headers: []NameValuePair{}, QueryString: []NameValuePair{},
				HeadersSize: -1, BodySize: 0,
			},
			Response: &Response{
				Status: status, StatusText: "s", HTTPVersion: "HTTP/1.1",
				Cookies: []Cookie{}, Headers: []NameValuePair{},
				Content:     &Content{Size: int64(len(body)), MimeType: mimeType, Text: body},
				RedirectURL: "", HeadersSize: -1, BodySize: -1,
			},
			Cache:   &Cache{},
			Timings: &Timings{Blocked: -1, DNS: -1, Connect: -1, Send: -1, Wait: -1, Receive: -1, SSL: -1},
		}
		var buf bytes.Buffer
		if err := NewHAR([]*Entry{e}).Write(&buf); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !json.Valid(buf.Bytes()) {
			t.Fatalf("invalid JSON produced")
		}
		var doc map[string]any
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
	})
}

func FuzzHeaderPairs(f *testing.F) {
	f.Add("Authorization", "Bearer x")
	f.Add("weird header\x00", "value\r\n")
	f.Add("", "")
	red := newRedactor(&Options{RedactHeaders: DefaultRedactedHeaders()})
	f.Fuzz(func(t *testing.T, name, value string) {
		h := http.Header{}
		h[name] = []string{value}
		pairs := red.headerPairs(h, "host")
		for _, p := range pairs {
			if strings.EqualFold(p.Name, "authorization") && p.Value != redactedValue {
				t.Fatalf("authorization leaked: %+v", p)
			}
		}
	})
}

// TestSizeHintIsUntrusted verifies that a wildly wrong size hint (a lying
// Content-Length) neither breaks the capture nor pre-allocates unbounded
// memory, and that hint clamping follows the capture limit.
func TestSizeHintIsUntrusted(t *testing.T) {
	// Absurd hint straight into the store: pre-allocation must be bounded
	// and writing/reading must work normally.
	w, err := MemoryBodyStore{}.NewWriter(context.Background(), BodyMetadata{SizeHint: 1 << 50})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if cap(w.(*memoryBodyWriter).buf.Bytes()[:0]) > maxPreallocBytes+1024 {
		t.Fatalf("pre-allocated %d bytes despite the cap", cap(w.(*memoryBodyWriter).buf.Bytes()[:0]))
	}
	if _, err := w.Write([]byte("short")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := w.Bytes()
	if err != nil || string(b) != "short" {
		t.Fatalf("bytes = %q, %v", b, err)
	}

	// Through the transport: a body far larger than its announced hint's
	// clamp must still be captured correctly up to the limit.
	tr := NewTransport(nil, NewMemoryRecorder(), WithMaxResponseBodyBytes(64))
	decision := BodyCaptureDecision{Capture: true, MaxBodyBytes: 64}
	bc := tr.newCapture(context.Background(), "x", "response", "text/plain", "", decision, 1<<40, tr.red)
	if bc.meta.SizeHint != 64 {
		t.Fatalf("hint = %d, want clamped to limit 64", bc.meta.SizeHint)
	}
	bc = tr.newCapture(context.Background(), "x", "response", "text/plain", "", decision, -1, tr.red)
	if bc.meta.SizeHint != 0 {
		t.Fatalf("hint = %d, want 0 for unknown length", bc.meta.SizeHint)
	}
	payload := bytes.Repeat([]byte("a"), 4096)
	bc = tr.newCapture(context.Background(), "x", "response", "text/plain", "", decision, 8, tr.red) // hint lies: says 8
	bc.observe(payload)
	bc.finishComplete()
	if bc.totalBytes() != 4096 {
		t.Fatalf("total = %d", bc.totalBytes())
	}
	if got := bc.bytes(); len(got) != 64 {
		t.Fatalf("captured = %d, want limit 64", len(got))
	}
}
