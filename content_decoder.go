package recorder

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
)

// ContentDecoder turns a compressed body stream into its decoded form. It is
// used only while building the HAR record: when a captured response body
// carries a Content-Encoding with a registered decoder, content.text/size
// are stored in decoded form (marked "_decoded": true) while bodySize, the
// body hash and the "_responseBody" counters keep describing the real wire
// bytes. The bytes handed to the caller are never touched.
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
