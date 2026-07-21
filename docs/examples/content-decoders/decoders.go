// Command content-decoders contains copy-oriented Brotli and Zstandard decoder
// examples. It is deliberately a main package so applications cannot depend on
// it as a library.
package main

import (
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/mgurevin/recorder"
)

func main() {}

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
