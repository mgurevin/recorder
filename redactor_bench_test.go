package recorder

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

const benchmarkBoundary = "recorder-benchmark-boundary"

var (
	benchmarkJSONSparse  = []byte(`{"items":[` + strings.Repeat(`{"id":1,"name":"ordinary-value","active":true},`, 1024) + `{"password":"secret"}]}`)
	benchmarkJSONDense   = []byte(`[` + strings.Repeat(`{"id":1,"password":"secret","token":"ordinary"},`, 1024) + `{"id":1,"password":"secret"}]`)
	benchmarkJSONNoMatch = bytes.ReplaceAll(benchmarkJSONDense, []byte(`"password"`), []byte(`"credential"`))
	benchmarkNDJSON      = []byte(strings.Repeat("{\"id\":1,\"password\":\"secret\",\"keep\":true}\n", 1024))
	benchmarkXML         = []byte(`<root>` + strings.Repeat(`<item><id>1</id><password>secret</password><keep>ordinary</keep></item>`, 1024) + `</root>`)
	benchmarkForm        = []byte(strings.Repeat("keep=ordinary&password=secret&", 2048) + "tail=1")
	benchmarkMultipart   = benchmarkMultipartPayload(512)
)

type benchmarkRedactorFactory func(io.Writer) (io.WriteCloser, error)

func benchmarkMultipartPayload(parts int) []byte {
	var out strings.Builder
	for i := 0; i < parts; i++ {
		fmt.Fprintf(&out, "--%s\r\nContent-Disposition: form-data; name=\"keep\"\r\n\r\nvalue-%d\r\n", benchmarkBoundary, i)
		fmt.Fprintf(&out, "--%s\r\nContent-Disposition: form-data; name=\"password\"\r\n\r\nsecret-%d\r\n", benchmarkBoundary, i)
	}

	fmt.Fprintf(&out, "--%s--\r\n", benchmarkBoundary)

	return []byte(out.String())
}

func benchmarkFieldSet(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[strings.ToLower(name)] = struct{}{}
	}

	return result
}

func benchmarkStreamRedactor(b *testing.B, payload []byte, chunkSize int, factory benchmarkRedactorFactory) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))

	for b.Loop() {
		writer, err := factory(io.Discard)
		if err != nil {
			b.Fatal(err)
		}

		for offset := 0; offset < len(payload); offset += chunkSize {
			end := min(offset+chunkSize, len(payload))
			if _, err := writer.Write(payload[offset:end]); err != nil {
				b.Fatal(err)
			}
		}

		if err := writer.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamRedactors(b *testing.B) {
	redact := newSensitiveValueProtector(SensitiveValueProtection{Mode: ProtectionRedact})
	fields := benchmarkFieldSet("password")

	cases := []struct {
		name        string
		payload     []byte
		contentType string
		factory     benchmarkRedactorFactory
	}{
		{"JSON/sparse", benchmarkJSONSparse, "application/json", func(dst io.Writer) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, fields, newBodyValueProtector(redact)), nil
		}},
		{"JSON/no_match", benchmarkJSONNoMatch, "application/json", func(dst io.Writer) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, fields, newBodyValueProtector(redact)), nil
		}},
		{"JSON/dense", benchmarkJSONDense, "application/json", func(dst io.Writer) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, fields, newBodyValueProtector(redact)), nil
		}},
		{"NDJSON/dense", benchmarkNDJSON, "application/x-ndjson", func(dst io.Writer) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, fields, newBodyValueProtector(redact)), nil
		}},
		{"XML/dense", benchmarkXML, "application/xml", func(dst io.Writer) (io.WriteCloser, error) {
			return newXMLStreamRedactor(dst, fields, newBodyValueProtector(redact)), nil
		}},
		{"Form/dense", benchmarkForm, "application/x-www-form-urlencoded", func(dst io.Writer) (io.WriteCloser, error) {
			return newFormStreamRedactor(dst, fields, newBodyValueProtector(redact)), nil
		}},
		{"Multipart/dense", benchmarkMultipart, "multipart/form-data; boundary=" + benchmarkBoundary, func(dst io.Writer) (io.WriteCloser, error) {
			writer := newMultipartStreamRedactor(dst, "multipart/form-data; boundary="+benchmarkBoundary, fields, newBodyValueProtector(redact))
			if writer.err != nil {
				return nil, writer.err
			}

			return writer, nil
		}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			for _, chunkSize := range []int{32, 4096, len(tc.payload)} {
				name := "chunk=" + strconv.Itoa(chunkSize)
				if chunkSize == len(tc.payload) {
					name = "chunk=whole"
				}

				b.Run(name, func(b *testing.B) {
					benchmarkStreamRedactor(b, tc.payload, chunkSize, tc.factory)
				})
			}
		})
	}
}

type benchmarkKeyProvider struct{ key ProtectionKey }

func (p benchmarkKeyProvider) ProtectionKey(ProtectionMode) (ProtectionKey, error) { return p.key, nil }

func BenchmarkSensitiveValueProtection(b *testing.B) {
	provider := benchmarkKeyProvider{key: ProtectionKey{ID: "bench-key", Key: bytes.Repeat([]byte{0x42}, 32)}}
	fields := benchmarkFieldSet("password")

	for _, tc := range []struct {
		name   string
		config SensitiveValueProtection
	}{
		{"redact", SensitiveValueProtection{Mode: ProtectionRedact}},
		{"encrypt", SensitiveValueProtection{Mode: ProtectionEncrypt, KeyProvider: provider}},
		{"tokenize", SensitiveValueProtection{Mode: ProtectionTokenize, KeyProvider: provider}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			protector := newSensitiveValueProtector(tc.config)

			benchmarkStreamRedactor(b, benchmarkJSONDense, 4096, func(dst io.Writer) (io.WriteCloser, error) {
				return newJSONStreamRedactor(dst, fields, newBodyValueProtector(protector)), nil
			})
		})
	}

	largeValue := []byte(`{"password":"` + strings.Repeat("x", defaultMaxProtectedValueBytes+1) + `"}`)

	b.Run("encrypt/value_too_large", func(b *testing.B) {
		protector := newSensitiveValueProtector(SensitiveValueProtection{Mode: ProtectionEncrypt, KeyProvider: provider})

		benchmarkStreamRedactor(b, largeValue, 4096, func(dst io.Writer) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, fields, newBodyValueProtector(protector)), nil
		})
	})
}

type benchmarkPassThroughRedactor struct{}

func (benchmarkPassThroughRedactor) Redact(dst io.Writer, _ string, _ BodyValueProtector) (io.WriteCloser, error) {
	return benchmarkWriteCloser{Writer: dst}, nil
}

type benchmarkWriteCloser struct{ io.Writer }

func (benchmarkWriteCloser) Close() error { return nil }

func benchmarkJSONHandler(payload []byte, encoding string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		w.Header().Set("Content-Type", "application/json")

		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	})
}

func benchmarkExchange(b *testing.B, client *http.Client, requestBody []byte, encoding string) {
	b.Helper()

	var body io.Reader

	method := http.MethodGet
	if requestBody != nil {
		method = http.MethodPost
		body = bytes.NewReader(requestBody)
	}

	req, err := http.NewRequest(method, benchURL, body)
	if err != nil {
		b.Fatal(err)
	}

	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if encoding != "" {
		req.Header.Set("Accept-Encoding", encoding)
	}

	resp, err := client.Do(req)
	if err != nil {
		b.Fatal(err)
	}

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		b.Fatal(err)
	}

	if err := resp.Body.Close(); err != nil {
		b.Fatal(err)
	}
}

func benchmarkTransportOptions() []Option {
	return []Option{
		WithCaptureRequestBody(true),
		WithCaptureResponseBody(true),
		WithEmbedBodies(false),
		WithHashBodies(false, ""),
		WithMaxRequestBodyBytes(0),
		WithMaxResponseBodyBytes(0),
	}
}

func BenchmarkTransportBodyPipeline(b *testing.B) {
	provider := benchmarkKeyProvider{key: ProtectionKey{ID: "bench-key", Key: bytes.Repeat([]byte{0x42}, 32)}}
	plain := benchmarkJSONDense

	var compressed bytes.Buffer

	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(plain); err != nil {
		b.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		b.Fatal(err)
	}

	cases := []struct {
		name      string
		response  []byte
		request   []byte
		encoding  string
		extra     []Option
		fileStore bool
	}{
		{name: "response/capture_only", response: plain},
		{name: "response/redact", response: plain, extra: []Option{jsonPasswordRedactionOption()}},
		{name: "request_response/redact", response: plain, request: plain, extra: []Option{jsonPasswordRedactionOption()}},
		{name: "response/encrypt", response: plain, extra: []Option{jsonPasswordRedactionOption(), WithSensitiveValueProtection(SensitiveValueProtection{Mode: ProtectionEncrypt, KeyProvider: provider})}},
		{name: "response/tokenize", response: plain, extra: []Option{jsonPasswordRedactionOption(), WithSensitiveValueProtection(SensitiveValueProtection{Mode: ProtectionTokenize, KeyProvider: provider})}},
		{name: "response/gzip_redact", response: compressed.Bytes(), encoding: "gzip", extra: []Option{jsonPasswordRedactionOption()}},
		{name: "response/custom_redactor", response: plain, extra: []Option{WithRedaction(RedactionConfig{Common: RedactionRules{BodyRedactors: map[string]BodyRedactor{"application/json": benchmarkPassThroughRedactor{}}}})}},
		{name: "response/policy", response: plain, extra: []Option{WithBodyCapturePolicy(BodyCapturePolicyFunc(func(_ context.Context, _ BodyCaptureMeta, defaults BodyCaptureDecision) (BodyCaptureDecision, error) {
			return defaults, nil
		}))}},
		{name: "response/file_store_redact", response: plain, fileStore: true, extra: []Option{jsonPasswordRedactionOption()}},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			options := append(benchmarkTransportOptions(), tc.extra...)
			recorder := Recorder(discardRecorder)

			if tc.fileStore {
				dir := b.TempDir()
				options = append(options, WithBodyStore(FileBodyStore{Dir: dir}))
				recorder = RecorderFunc(func(entry *Entry) {
					if entry.RequestBody != nil && entry.RequestBody.Store != "" {
						_ = os.Remove(entry.RequestBody.Store)
					}

					if entry.ResponseBody != nil && entry.ResponseBody.Store != "" {
						_ = os.Remove(entry.ResponseBody.Store)
					}
				})
			}

			client := benchClient(b, benchmarkJSONHandler(tc.response, tc.encoding))
			client.Transport = NewTransport(client.Transport, recorder, options...)

			b.ReportAllocs()
			b.SetBytes(int64(len(plain) + len(tc.request)))

			for b.Loop() {
				benchmarkExchange(b, client, tc.request, tc.encoding)
			}
		})
	}
}

func BenchmarkTransportRedactionParallel(b *testing.B) {
	client := benchClient(b, benchmarkJSONHandler(benchmarkJSONDense, ""))

	options := append(benchmarkTransportOptions(), jsonPasswordRedactionOption())
	client.Transport = NewTransport(client.Transport, discardRecorder, options...)

	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkJSONDense)))
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			benchmarkExchange(b, client, nil, "")
		}
	})
}

func jsonPasswordRedactionOption() Option {
	return WithRedaction(RedactionConfig{Common: RedactionRules{
		JSONFields: []string{"password"},
	}})
}
