package recorder

import "io"

// byteSink caches the optional io.ByteWriter fast path once. Its reusable
// fallback buffer avoids converting each emitted byte into a fresh escaping
// []byte when the destination only implements io.Writer.
type byteSink struct {
	dst  io.Writer
	byte io.ByteWriter
	one  [1]byte
}

func newByteSink(dst io.Writer) byteSink {
	byteWriter, _ := dst.(io.ByteWriter)
	return byteSink{dst: dst, byte: byteWriter}
}

func (w *byteSink) WriteByte(b byte) error {
	if w.byte != nil {
		return w.byte.WriteByte(b)
	}

	w.one[0] = b

	n, err := w.dst.Write(w.one[:])
	if err == nil && n != len(w.one) {
		return io.ErrShortWrite
	}

	return err
}
