package recorder

import (
	"io"
	"net/url"
	"strings"
)

const maxFormKeyBytes = 64 << 10

const formRedactedValue = "%5BREDACTED%5D"

// formStreamRedactor preserves the application/x-www-form-urlencoded wire
// representation except for values whose decoded field name matches a
// configured query-parameter rule. Only the current raw key is buffered;
// matched values are discarded as they stream.
type formStreamRedactor struct {
	dst      io.Writer
	fields   map[string]struct{}
	key      []byte
	inValue  bool
	suppress bool
	err      error
}

func newFormStreamRedactor(dst io.Writer, fields map[string]struct{}) *formStreamRedactor {
	return &formStreamRedactor{dst: dst, fields: fields}
}

func (r *formStreamRedactor) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for i, b := range p {
		if err := r.consume(b); err != nil {
			r.err = err
			return i, err
		}
	}
	return len(p), nil
}

func (r *formStreamRedactor) Close() error {
	if r.err != nil {
		return r.err
	}
	if !r.inValue && len(r.key) > 0 {
		_, r.err = r.dst.Write(r.key)
		r.key = nil
	}
	return r.err
}

func (r *formStreamRedactor) consume(b byte) error {
	if !r.inValue {
		switch b {
		case '&':
			if err := r.emitKey(); err != nil {
				return err
			}
			return r.emitByte(b)
		case '=':
			name := string(r.key)
			if decoded, err := url.QueryUnescape(name); err == nil {
				name = decoded
			}
			_, r.suppress = r.fields[strings.ToLower(name)]
			if err := r.emitKey(); err != nil {
				return err
			}
			if err := r.emitByte(b); err != nil {
				return err
			}
			r.inValue = true
			if r.suppress {
				_, err := io.WriteString(r.dst, formRedactedValue)
				return err
			}
			return nil
		default:
			if len(r.key) >= maxFormKeyBytes {
				return errRedactionLimit
			}
			r.key = append(r.key, b)
			return nil
		}
	}

	if b == '&' {
		r.inValue = false
		r.suppress = false
		return r.emitByte(b)
	}
	if r.suppress {
		return nil
	}
	return r.emitByte(b)
}

func (r *formStreamRedactor) emitKey() error {
	if len(r.key) == 0 {
		return nil
	}
	_, err := r.dst.Write(r.key)
	r.key = r.key[:0]
	return err
}

func (r *formStreamRedactor) emitByte(b byte) error {
	_, err := r.dst.Write([]byte{b})
	return err
}
