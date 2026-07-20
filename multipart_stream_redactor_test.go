package recorder

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

const multipartTestType = `multipart/form-data; boundary="recorder-boundary"`

func multipartFixture(secret string) string {
	return "preamble\r\n" +
		"--recorder-boundary\r\n" +
		"Content-Disposition: form-data; name=keep\r\n\r\n" +
		"safe-value\r\n" +
		"--recorder-boundary\r\n" +
		"Content-Disposition: form-data; name=token\r\n\r\n" +
		secret + "\r\n--recorder-boundaryX\r\nstill-secret\r\n" +
		"--recorder-boundary\r\n" +
		"Content-Disposition: form-data; name=upload; filename=customer-123.pdf\r\n" +
		"Content-Type: application/pdf\r\n\r\n" +
		"file-secret-" + secret + "\r\n" +
		"--recorder-boundary--\r\n" +
		"epilogue"
}

func streamMultipart(t *testing.T, input string, chunk int) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	r := newMultipartStreamRedactor(&out, multipartTestType, lowerSet([]string{"token", "upload"}))
	for pos := 0; pos < len(input); {
		end := min(len(input), pos+chunk)
		if _, err := r.Write([]byte(input[pos:end])); err != nil {
			return out.Bytes(), err
		}
		pos = end
	}
	err := r.Close()
	return out.Bytes(), err
}

func TestMultipartStreamRedactorChunkBoundaries(t *testing.T) {
	const secret = "part-secret"
	in := multipartFixture(secret)
	for chunk := 1; chunk <= 97; chunk++ {
		got, err := streamMultipart(t, in, chunk)
		if err != nil {
			t.Fatalf("chunk %d: %v; output=%q", chunk, err, got)
		}
		if bytes.Contains(got, []byte(secret)) || bytes.Contains(got, []byte("still-secret")) || bytes.Contains(got, []byte("customer-123.pdf")) {
			t.Fatalf("chunk %d leaked matched part: %q", chunk, got)
		}
		if !bytes.Contains(got, []byte("safe-value")) || !bytes.Contains(got, []byte("preamble")) || !bytes.Contains(got, []byte("epilogue")) {
			t.Fatalf("chunk %d lost unmatched bytes: %q", chunk, got)
		}
		if bytes.Count(got, []byte(redactedValue)) != 3 { // field body, file name, file body
			t.Fatalf("chunk %d redaction count = %d: %q", chunk, bytes.Count(got, []byte(redactedValue)), got)
		}
		assertMultipartParts(t, got)
	}
}

func assertMultipartParts(t *testing.T, body []byte) {
	t.Helper()
	start := bytes.Index(body, []byte("--recorder-boundary"))
	if start < 0 {
		t.Fatal("missing first boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body[start:]), "recorder-boundary")
	var parts []struct{ name, filename, body string }
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse redacted multipart: %v", err)
		}
		b, _ := io.ReadAll(p)
		parts = append(parts, struct{ name, filename, body string }{p.FormName(), p.FileName(), string(b)})
	}
	if len(parts) != 3 || parts[0].body != "safe-value" || parts[1].body != redactedValue || parts[2].body != redactedValue || parts[2].filename != redactedValue {
		t.Fatalf("redacted parts = %+v", parts)
	}
}

func TestMultipartStreamRedactorFailsClosed(t *testing.T) {
	t.Run("invalid boundary", func(t *testing.T) {
		var out bytes.Buffer
		r := newMultipartStreamRedactor(&out, "multipart/form-data", lowerSet([]string{"token"}))
		if _, err := r.Write([]byte("token=secret")); !errors.Is(err, errMalformedMultipart) || out.Len() != 0 {
			t.Fatalf("error=%v output=%q", err, out.Bytes())
		}
	})
	t.Run("oversized header", func(t *testing.T) {
		input := "--recorder-boundary\r\nX-Test: " + strings.Repeat("a", maxMultipartHeaderBytes+1)
		_, err := streamMultipart(t, input, 37)
		if !errors.Is(err, errRedactionLimit) {
			t.Fatalf("error = %v, want limit", err)
		}
	})
	t.Run("unmatched nested multipart", func(t *testing.T) {
		input := "--recorder-boundary\r\n" +
			"Content-Disposition: form-data; name=keep\r\n" +
			"Content-Type: multipart/mixed; boundary=inner\r\n\r\n--inner--\r\n" +
			"--recorder-boundary--\r\n"
		_, err := streamMultipart(t, input, 11)
		if !errors.Is(err, errMalformedMultipart) {
			t.Fatalf("error = %v, want malformed", err)
		}
	})
	t.Run("incomplete matched part", func(t *testing.T) {
		input := "--recorder-boundary\r\nContent-Disposition: form-data; name=token\r\n\r\nsecret-tail"
		got, err := streamMultipart(t, input, 3)
		if !errors.Is(err, errMalformedMultipart) || bytes.Contains(got, []byte("secret-tail")) {
			t.Fatalf("error=%v output=%q", err, got)
		}
	})
}

func TestMultipartExtendedFilenameIsRedacted(t *testing.T) {
	block := []byte("Content-Disposition: form-data; name=upload; filename*=UTF-8''customer-%C3%B6zel.pdf\r\n\r\n")
	got, matched, nested, err := parseMultipartHeaders(block, lowerSet([]string{"upload"}))
	if err != nil || !matched || nested {
		t.Fatalf("matched=%v nested=%v err=%v", matched, nested, err)
	}
	if bytes.Contains(got, []byte("customer")) || bytes.Contains(got, []byte("%C3")) {
		t.Fatalf("extended filename leaked: %q", got)
	}
	line := bytes.TrimSuffix(got, []byte("\r\n\r\n"))
	colon := bytes.IndexByte(line, ':')
	_, params, err := mime.ParseMediaType(strings.TrimSpace(string(line[colon+1:])))
	if err != nil || params["filename"] != redactedValue || params["name"] != "upload" {
		t.Fatalf("rewritten disposition = %q, params=%v err=%v", line, params, err)
	}
}

func TestMultipartStreamRedactorLargeMatchedFileUsesBoundedState(t *testing.T) {
	input := "--recorder-boundary\r\n" +
		"Content-Disposition: form-data; name=upload; filename=large.bin\r\n\r\n" +
		strings.Repeat("s", 8<<20) + "\r\n--recorder-boundary--\r\n"
	got, err := streamMultipart(t, input, 19)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 1024 || bytes.Count(got, []byte(redactedValue)) != 2 {
		t.Fatalf("unexpected bounded output: %d bytes", len(got))
	}
}

func FuzzMultipartStreamRedactor(f *testing.F) {
	f.Add([]byte("value"), uint8(1))
	f.Add([]byte{0, 1, 2, 255}, uint8(31))
	f.Fuzz(func(t *testing.T, payload []byte, chunkByte uint8) {
		if len(payload) > 4096 {
			t.Skip()
		}
		secret := "multipart-secret-" + hex.EncodeToString(payload)
		got, err := streamMultipart(t, multipartFixture(secret), int(chunkByte%64)+1)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(got, []byte(secret)) || bytes.Contains(got, []byte("customer-123.pdf")) {
			t.Fatalf("stream leaked matched part: %q", got)
		}
		assertMultipartParts(t, got)
	})
}
