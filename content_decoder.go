package recorder

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// ContentDecoder turns a compressed body stream into its decoded form. It is
// used by the recording pipeline. With structured redaction enabled, a
// captured response carrying a registered Content-Encoding is decoded and
// redacted before bytes reach the BodyStore. Otherwise, a fully captured
// body may be decoded when embedded in the HAR. Decoded HAR content is marked
// "_decoded": true while bodySize, the body hash and "_responseBody"
// counters keep describing the real wire bytes. Caller-visible bytes are
// never touched.
//
// Decoders for encodings outside the standard library (brotli, zstd) are
// deliberately not bundled — the module stays dependency-free. Registering
// one is a few lines with the de-facto standard implementations:
//
//	import "github.com/andybalholm/brotli"
//
//	recorder.WithContentDecoder("br", func(r io.Reader) (io.ReadCloser, error) {
//		return io.NopCloser(brotli.NewReader(r)), nil
//	})
//
//	import "github.com/klauspost/compress/zstd"
//
//	recorder.WithContentDecoder("zstd", func(r io.Reader) (io.ReadCloser, error) {
//		zr, err := zstd.NewReader(r)
//		if err != nil {
//			return nil, err
//		}
//		return zr.IOReadCloser(), nil
//	})
type ContentDecoder func(io.Reader) (io.ReadCloser, error)

var errDecodedBodyTooLarge = errors.New("recorder: decoded body exceeds capture limit")

// decodingRedactingBodyWriter streams encoded input through a ContentDecoder
// and then through the structured redactor before it reaches the BodyStore.
// The pipe intentionally applies backpressure: memory remains bounded by the
// decoder, parser state, and io.Pipe's synchronous handoff. As with entry
// finalization, the caller must read or close the HTTP body; otherwise the
// capture pipeline, including this worker, remains live with that body.
type decodingRedactingBodyWriter struct {
	BodyWriter
	pw   *io.PipeWriter
	done chan error
}

func newDecodingRedactingBodyWriter(dst BodyWriter, decoder ContentDecoder, mimeType string, red *redactor, limit int64) BodyWriter {
	pr, pw := io.Pipe()
	w := &decodingRedactingBodyWriter{BodyWriter: dst, pw: pw, done: make(chan error, 1)}
	go func() {
		decoded, err := decoder(pr)
		if err != nil {
			_ = pr.CloseWithError(err)
			w.done <- fmt.Errorf("recorder: open streaming body decoder: %w", err)
			return
		}
		buffered := bufio.NewWriterSize(dst, 32<<10)
		sr := newBodyStreamRedactor(buffered, mimeType, red)
		var copied int64
		buf := make([]byte, 32<<10)
		for err == nil {
			var n int
			n, err = decoded.Read(buf)
			if n > 0 {
				if limit > 0 && copied+int64(n) > limit {
					err = errDecodedBodyTooLarge
					break
				}
				copied += int64(n)
				if _, writeErr := sr.Write(buf[:n]); writeErr != nil {
					err = writeErr
					break
				}
			}
		}
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if closeErr := decoded.Close(); err == nil {
			err = closeErr
		}
		if closeErr := sr.Close(); err == nil {
			err = closeErr
		}
		if flushErr := buffered.Flush(); err == nil {
			err = flushErr
		}
		_ = pr.CloseWithError(err)
		w.done <- err
	}()
	return w
}

func (w *decodingRedactingBodyWriter) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

func (w *decodingRedactingBodyWriter) Close() error {
	pipeErr := w.pw.Close()
	decodeErr := <-w.done
	closeErr := w.BodyWriter.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if pipeErr != nil {
		return pipeErr
	}
	return closeErr
}

// GzipDecoder decodes gzip content using the standard library. Registered by
// default for "gzip" and "x-gzip".
func GzipDecoder(r io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(r)
}

// DeflateDecoder decodes "deflate" content using the standard library.
// Registered by default. HTTP's deflate is formally zlib-wrapped (RFC 9110),
// but a number of servers send raw DEFLATE streams; the decoder sniffs the
// zlib header and handles both, like browsers do.
func DeflateDecoder(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	hdr, err := br.Peek(2)
	if err == nil && len(hdr) == 2 && hdr[0]&0x0f == 8 && (uint16(hdr[0])<<8|uint16(hdr[1]))%31 == 0 {
		return zlib.NewReader(br)
	}
	return flate.NewReader(br), nil
}

// defaultContentDecoders returns the stdlib-only decoder set installed by
// DefaultOptions.
func defaultContentDecoders() map[string]ContentDecoder {
	return map[string]ContentDecoder{
		"gzip":    GzipDecoder,
		"x-gzip":  GzipDecoder,
		"deflate": DeflateDecoder,
	}
}
