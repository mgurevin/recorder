package recorder

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

type markerBodyRedactor struct {
	marker string
	opens  atomic.Int64
	closes atomic.Int64
}

func (r *markerBodyRedactor) Redact(dst io.Writer, _ string) (io.WriteCloser, error) {
	r.opens.Add(1)
	return &markerBodyWriter{dst: dst, marker: r.marker, closes: &r.closes}, nil
}

type markerBodyWriter struct {
	dst     io.Writer
	marker  string
	written bool
	closes  *atomic.Int64
}

func (w *markerBodyWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.written = true
		if _, err := io.WriteString(w.dst, w.marker); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

func (w *markerBodyWriter) Close() error {
	w.closes.Add(1)
	return nil
}

func TestCustomBodyRedactorRunsOnceAndOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	custom := &markerBodyRedactor{marker: "[CUSTOM-ONCE]"}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Application/JSON; charset=utf-8")
		w.Write([]byte(`{"password":"wire-secret"}`))
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts,
		WithBodyStore(FileBodyStore{Dir: dir}),
		WithRedactJSONFields("password"),
		WithBodyRedactor("application/json", custom),
	)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(mustReadAll(t, resp.Body)); !strings.Contains(got, "wire-secret") {
		t.Fatalf("caller body changed: %q", got)
	}

	e := singleEntry(t, rec)
	if e.Response.Content.Text != custom.marker {
		t.Fatalf("embedded body = %q", e.Response.Content.Text)
	}

	stored, err := os.ReadFile(e.ResponseBody.Store)
	if err != nil || string(stored) != custom.marker {
		t.Fatalf("stored body = %q, err=%v", stored, err)
	}

	if custom.opens.Load() != 1 || custom.closes.Load() != 1 {
		t.Fatalf("redactor lifecycle: opens=%d closes=%d", custom.opens.Load(), custom.closes.Load())
	}
}

func TestCustomBodyRedactorLastRegistrationWins(t *testing.T) {
	first := &markerBodyRedactor{marker: "first"}
	last := &markerBodyRedactor{marker: "last"}
	o := DefaultOptions()
	WithBodyRedactor("text/csv", first)(&o)
	WithBodyRedactor("TEXT/CSV; charset=utf-8", last)(&o)
	red := newRedactor(&o)

	var out bytes.Buffer

	w := newBodyStreamRedactor(&out, "text/csv; charset=iso-8859-1", red)
	if _, err := w.Write([]byte("secret")); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if out.String() != "last" || first.opens.Load() != 0 || last.opens.Load() != 1 {
		t.Fatalf("output=%q first=%d last=%d", out.String(), first.opens.Load(), last.opens.Load())
	}
}

func TestCustomBodyRedactorCompressedStream(t *testing.T) {
	custom := &markerBodyRedactor{marker: "redacted-csv"}
	wire := gzipBytes(t, "card_number,amount\n4111111111111111,10")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/csv")
		w.Write(wire)
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts, WithBodyRedactor("text/csv", custom))
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if got := mustReadAll(t, resp.Body); !bytes.Equal(got, wire) {
		t.Fatal("caller compressed bytes changed")
	}

	e := singleEntry(t, rec)
	if !e.Response.Content.Decoded || e.Response.Content.Text != custom.marker {
		t.Fatalf("content = %+v", e.Response.Content)
	}

	if custom.opens.Load() != 1 || custom.closes.Load() != 1 {
		t.Fatalf("redactor lifecycle: opens=%d closes=%d", custom.opens.Load(), custom.closes.Load())
	}
}

type testBodyRedactorFunc func(io.Writer, string) (io.WriteCloser, error)

func (f testBodyRedactorFunc) Redact(dst io.Writer, contentType string) (io.WriteCloser, error) {
	return f(dst, contentType)
}

type failingRedactorWriter struct {
	write func([]byte) (int, error)
	close func() error
}

func (w *failingRedactorWriter) Write(p []byte) (int, error) { return w.write(p) }
func (w *failingRedactorWriter) Close() error                { return w.close() }

type reportingBodyWriter struct {
	io.WriteCloser
	replacements int64
}

func (w *reportingBodyWriter) BodyRedactionReport() BodyRedactionReport {
	return BodyRedactionReport{Replacements: w.replacements}
}

func TestBodyRedactorFailuresAreContained(t *testing.T) {
	boom := errors.New("boom")

	cases := []struct {
		name string
		red  BodyRedactor
	}{
		{"constructor error", testBodyRedactorFunc(func(io.Writer, string) (io.WriteCloser, error) { return nil, boom })},
		{"constructor panic", testBodyRedactorFunc(func(io.Writer, string) (io.WriteCloser, error) { panic("constructor") })},
		{"nil writer", testBodyRedactorFunc(func(io.Writer, string) (io.WriteCloser, error) { return nil, nil })},
		{"write error", testBodyRedactorFunc(func(io.Writer, string) (io.WriteCloser, error) {
			return &failingRedactorWriter{write: func([]byte) (int, error) { return 0, boom }, close: func() error { return nil }}, nil
		})},
		{"write panic", testBodyRedactorFunc(func(io.Writer, string) (io.WriteCloser, error) {
			return &failingRedactorWriter{write: func([]byte) (int, error) { panic("write") }, close: func() error { return nil }}, nil
		})},
		{"short write", testBodyRedactorFunc(func(io.Writer, string) (io.WriteCloser, error) {
			return &failingRedactorWriter{write: func([]byte) (int, error) { return 0, nil }, close: func() error { return nil }}, nil
		})},
		{"close error", testBodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return &failingRedactorWriter{write: dst.Write, close: func() error { return boom }}, nil
		})},
		{"close panic", testBodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return &failingRedactorWriter{write: dst.Write, close: func() error { panic("close") }}, nil
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			red := newRedactor(&Options{BodyRedactors: map[string]BodyRedactor{"text/csv": tc.red}})
			w := newBodyStreamRedactor(&out, "text/csv", red)
			_, writeErr := w.Write([]byte("secret"))

			closeErr := w.Close()
			if writeErr == nil && closeErr == nil {
				t.Fatal("failure was not surfaced")
			}
		})
	}
}

func TestCustomBodyRedactorFailureDoesNotAffectHTTP(t *testing.T) {
	custom := testBodyRedactorFunc(func(io.Writer, string) (io.WriteCloser, error) {
		return &failingRedactorWriter{
			write: func([]byte) (int, error) { panic("custom write") },
			close: func() error { return nil },
		}, nil
	})

	var internal atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Write([]byte("wire-secret"))
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts,
		WithBodyRedactor("text/csv", custom),
		WithOnInternalError(func(error) { internal.Add(1) }),
	)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("HTTP call changed by custom redactor: %v", err)
	}

	if got := string(mustReadAll(t, resp.Body)); got != "wire-secret" {
		t.Fatalf("caller body = %q", got)
	}

	e := singleEntry(t, rec)
	if e.Response.Content.Text != "" || internal.Load() == 0 {
		t.Fatalf("content=%q internalErrors=%d", e.Response.Content.Text, internal.Load())
	}

	if e.Redaction == nil || e.Redaction.Response == nil || e.Redaction.Response.Body == nil || e.Redaction.Response.Body.Outcome != "failed" {
		t.Fatalf("redaction audit = %+v", e.Redaction)
	}
}

func TestCustomBodyRedactorCanReportReplacementCount(t *testing.T) {
	custom := testBodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
		writer := &failingRedactorWriter{write: func(p []byte) (int, error) {
			_, err := io.WriteString(dst, "[CUSTOM]")
			return len(p), err
		}, close: func() error { return nil }}

		return &reportingBodyWriter{WriteCloser: writer, replacements: 2}, nil
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		io.WriteString(w, "secret")
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts, WithBodyRedactor("text/csv", custom))

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	mustReadAll(t, resp.Body)

	info := singleEntry(t, rec).Redaction
	if info == nil || info.Response == nil || info.Response.Body == nil || info.Response.Body.Replacements == nil ||
		*info.Response.Body.Replacements != 2 || info.Response.Body.Outcome != "redacted" {
		t.Fatalf("redaction audit = %+v", info)
	}
}

func TestBuiltinBodyRedactorsReportReplacementCounts(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		opts        Options
		want        int64
	}{
		{"json", "application/json", `{"password":"one","nested":{"password":"two"}}`, Options{RedactJSONFields: []string{"password"}}, 2},
		{"xml", "application/xml", `<r><password>one</password><password>two</password></r>`, Options{RedactXMLElements: []string{"password"}}, 2},
		{"form", "application/x-www-form-urlencoded", `token=one&keep=x&token=two`, Options{RedactQueryParameters: []string{"token"}}, 2},
		{"multipart", multipartTestType, multipartFixture("secret"), Options{RedactQueryParameters: []string{"token", "upload"}}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			w := newBodyStreamRedactor(&out, tc.contentType, newRedactor(&tc.opts))
			if w == nil {
				t.Fatal("redactor not selected")
			}

			if _, err := w.Write([]byte(tc.body)); err != nil {
				t.Fatal(err)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			reporter, ok := w.(BodyRedactionReporter)
			if !ok || reporter.BodyRedactionReport().Replacements != tc.want {
				t.Fatalf("report = %+v, reporter=%v", reporter, ok)
			}
		})
	}
}

func TestCustomBodyRedactorConcurrentSelection(t *testing.T) {
	custom := &markerBodyRedactor{marker: "x"}
	red := newRedactor(&Options{BodyRedactors: map[string]BodyRedactor{"text/csv": custom}})

	const workers = 64

	done := make(chan error, workers)
	for range workers {
		go func() {
			var out bytes.Buffer

			w := newBodyStreamRedactor(&out, "text/csv", red)

			_, err := w.Write([]byte("secret"))
			if closeErr := w.Close(); err == nil {
				err = closeErr
			}

			if err == nil && out.String() != "x" {
				err = errors.New("unexpected output")
			}

			done <- err
		}()
	}

	for range workers {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	if custom.opens.Load() != workers || custom.closes.Load() != workers {
		t.Fatalf("opens=%d closes=%d", custom.opens.Load(), custom.closes.Load())
	}
}

func TestCustomBodyRedactorCannotCloseDestination(t *testing.T) {
	custom := testBodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
		if _, ok := dst.(io.Closer); ok {
			return nil, errors.New("destination exposes Close")
		}

		return &failingRedactorWriter{write: dst.Write, close: func() error { return nil }}, nil
	})
	red := newRedactor(&Options{BodyRedactors: map[string]BodyRedactor{"text/csv": custom}})

	var out bytes.Buffer

	w := newBodyStreamRedactor(&out, "text/csv", red)
	if _, err := w.Write([]byte("kept")); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if out.String() != "kept" {
		t.Fatalf("output = %q", out.String())
	}
}

func FuzzBodyRedactorSelection(f *testing.F) {
	f.Add("text/csv", []byte("secret"))
	f.Add("TEXT/CSV; charset=utf-8", []byte{0, 1, 2})
	f.Add("application/octet-stream", []byte("plain"))
	f.Fuzz(func(t *testing.T, contentType string, body []byte) {
		custom := &markerBodyRedactor{marker: "[CUSTOM]"}
		red := newRedactor(&Options{BodyRedactors: map[string]BodyRedactor{"text/csv": custom}})

		var out bytes.Buffer

		w := newBodyStreamRedactor(&out, contentType, red)
		if baseMimeType(contentType) != "text/csv" {
			if w != nil {
				t.Fatalf("unexpected redactor selected for %q", contentType)
			}

			return
		}

		if w == nil {
			t.Fatalf("redactor not selected for %q", contentType)
		}

		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		if out.String() != custom.marker || custom.opens.Load() != 1 || custom.closes.Load() != 1 {
			t.Fatalf("output=%q opens=%d closes=%d", out.String(), custom.opens.Load(), custom.closes.Load())
		}
	})
}
