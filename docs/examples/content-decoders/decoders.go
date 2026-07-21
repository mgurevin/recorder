// Package contentdecoders shows how to add Brotli and Zstandard decoding to
// recorder without adding either implementation to the core module.
package contentdecoders

import (
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/mgurevin/recorder"
)

// Brotli opens a streaming Brotli decoder suitable for Content-Encoding: br.
func Brotli(r io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(brotli.NewReader(r)), nil
}

// Zstandard opens a streaming Zstandard decoder suitable for
// Content-Encoding: zstd.
func Zstandard(r io.Reader) (io.ReadCloser, error) {
	decoder, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}

	return decoder.IOReadCloser(), nil
}

// Options returns the recorder options that register both decoders.
func Options() []recorder.Option {
	return []recorder.Option{
		recorder.WithContentDecoder("br", Brotli),
		recorder.WithContentDecoder("zstd", Zstandard),
	}
}
