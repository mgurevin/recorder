package recorder

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
)

// ContentDecoder turns a compressed body stream into its decoded form. It is
// used by the recording pipeline. With body redaction enabled, a captured
// request or response carrying a registered Content-Encoding is decoded and
// redacted before bytes reach the BodyStore. Otherwise, a fully captured
// response may be decoded when embedded in the HAR. Decoded HAR response
// content is marked by _recorder.responseBodyDecoded while bodySize, the body
// hash and body counters keep describing the encoded bytes observed by the
// caller. The live request and caller-visible response bytes are never touched.
//
// Decoders for encodings outside the standard library (brotli, zstd) are
// deliberately not bundled — the module stays dependency-free. Registering
// one is a few lines with the de-facto standard implementations. A complete,
// independently pinned and tested example is under
// docs/examples/content-decoders:
//
//	import "github.com/andybalholm/brotli"
//
//	config.ContentDecoders["br"] = func(r io.Reader) (io.ReadCloser, error) {
//		return io.NopCloser(brotli.NewReader(r)), nil
//	}
//
//	import "github.com/klauspost/compress/zstd"
//
//	config.ContentDecoders["zstd"] = func(r io.Reader) (io.ReadCloser, error) {
//		zr, err := zstd.NewReader(r)
//		if err != nil {
//			return nil, err
//		}
//		return zr.IOReadCloser(), nil
//	}
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
// DefaultConfig.
func defaultContentDecoders() map[string]ContentDecoder {
	return map[string]ContentDecoder{
		"gzip":    GzipDecoder,
		"x-gzip":  GzipDecoder,
		"deflate": DeflateDecoder,
	}
}
