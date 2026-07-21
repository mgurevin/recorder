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
	dst          io.Writer
	bytes        byteSink
	fields       map[string]struct{}
	key          []byte
	inValue      bool
	suppress     bool
	err          error
	replacements int64
	protected    protectedValueBuffer
}

func (r *formStreamRedactor) BodyRedactionReport() BodyRedactionReport {
	return BodyRedactionReport{Replacements: r.replacements, Protection: r.protected.protectionReport()}
}

func (r *formStreamRedactor) bodyProtectionFailure() (error, int64) {
	return r.protected.protectionFailure()
}

func newFormStreamRedactor(dst io.Writer, fields map[string]struct{}, protectors ...*sensitiveValueProtector) *formStreamRedactor {
	protector := newSensitiveValueProtector(SensitiveValueProtection{})
	if len(protectors) > 0 && protectors[0] != nil {
		protector = protectors[0]
	}

	r := &formStreamRedactor{dst: dst, bytes: newByteSink(dst), fields: fields}
	r.protected.reset(protector)

	return r
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

	if r.err == nil && r.inValue && r.suppress {
		r.err = r.emitProtected()
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
				r.replacements++
				r.protected.reset(r.protected.protector)

				if r.protected.redactImmediately() {
					_, err := io.WriteString(r.dst, formRedactedValue)
					return err
				}
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
		if r.suppress {
			if err := r.emitProtected(); err != nil {
				return err
			}
		}

		r.inValue = false
		r.suppress = false

		return r.emitByte(b)
	}

	if r.suppress {
		r.protected.append(b)
		return nil
	}

	return r.emitByte(b)
}

func (r *formStreamRedactor) emitProtected() error {
	value, _, _ := r.protected.finish()
	if value == "" {
		return nil
	}

	_, err := io.WriteString(r.dst, url.QueryEscape(value))

	return err
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
	return r.bytes.WriteByte(b)
}
