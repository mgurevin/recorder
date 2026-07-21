package recorder

import (
	"bytes"
	"io"
	"strings"
)

const (
	maxXMLMarkupBytes = 64 << 10
	maxXMLDepth       = 1024
)

// xmlStreamRedactor buffers only the current markup token. Ordinary character
// data is passed through immediately, or discarded while inside a redacted
// element. Namespace prefixes and all bytes outside matched subtrees survive
// unchanged.
type xmlStreamRedactor struct {
	dst      io.Writer
	bytes    byteSink
	elements map[string]struct{}
	markup   []byte
	inMarkup bool
	quote    byte
	brackets int

	suppressNames     []string
	suppressNameBytes int
	err               error
	replacements      int64
	protected         protectedValueBuffer
}

func (r *xmlStreamRedactor) BodyRedactionReport() BodyRedactionReport {
	return BodyRedactionReport{Replacements: r.replacements, Protection: r.protected.protectionReport()}
}

func (r *xmlStreamRedactor) bodyProtectionFailure() (error, int64) {
	return r.protected.protectionFailure()
}

func newXMLStreamRedactor(dst io.Writer, elements map[string]struct{}, protectors ...*sensitiveValueProtector) *xmlStreamRedactor {
	protector := newSensitiveValueProtector(SensitiveValueProtection{})
	if len(protectors) > 0 && protectors[0] != nil {
		protector = protectors[0]
	}

	r := &xmlStreamRedactor{dst: dst, bytes: newByteSink(dst), elements: elements}
	r.protected.reset(protector)

	return r
}

func (r *xmlStreamRedactor) Write(p []byte) (int, error) {
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

func (r *xmlStreamRedactor) Close() error {
	if r.err != nil {
		return r.err
	}
	// Preserve an incomplete markup token only when it is outside a redacted
	// subtree. Inside a matched element, failing closed avoids leaking content.
	if r.inMarkup && len(r.suppressNames) == 0 {
		_, r.err = r.dst.Write(r.markup)
	} else if len(r.suppressNames) > 0 {
		r.err = r.emitProtected()
	}

	return r.err
}

func (r *xmlStreamRedactor) consume(b byte) error {
	if !r.inMarkup {
		if b == '<' {
			r.inMarkup = true
			r.markup = append(r.markup[:0], b)

			return nil
		}

		if len(r.suppressNames) == 0 {
			return r.bytes.WriteByte(b)
		}

		r.protected.append(b)

		return nil
	}

	if len(r.markup) >= maxXMLMarkupBytes {
		return errRedactionLimit
	}

	r.markup = append(r.markup, b)
	if !r.markupComplete(b) {
		return nil
	}

	return r.finishMarkup()
}

func (r *xmlStreamRedactor) markupComplete(b byte) bool {
	t := r.markup
	if bytes.HasPrefix(t, []byte("<!--")) {
		return bytes.HasSuffix(t, []byte("-->"))
	}

	if bytes.HasPrefix(t, []byte("<![CDATA[")) {
		return bytes.HasSuffix(t, []byte("]]>"))
	}

	if bytes.HasPrefix(t, []byte("<?")) {
		return bytes.HasSuffix(t, []byte("?>"))
	}

	if r.quote != 0 {
		if b == r.quote {
			r.quote = 0
		}

		return false
	}

	if b == '\'' || b == '"' {
		r.quote = b
		return false
	}

	if bytes.HasPrefix(t, []byte("<!")) {
		if b == '[' {
			r.brackets++
		} else if b == ']' && r.brackets > 0 {
			r.brackets--
		}

		return b == '>' && r.brackets == 0
	}

	return b == '>'
}

func (r *xmlStreamRedactor) finishMarkup() error {
	token := r.markup
	r.inMarkup = false
	r.quote = 0
	r.brackets = 0

	kind, local, selfClosing := xmlMarkupInfo(token)
	if len(r.suppressNames) > 0 {
		switch kind {
		case 's':
			if !selfClosing {
				if len(r.suppressNames) >= maxXMLDepth || r.suppressNameBytes+len(local) > maxXMLMarkupBytes {
					return errRedactionLimit
				}

				r.suppressNames = append(r.suppressNames, local)
				r.suppressNameBytes += len(local)
			}

		case 'e':
			top := len(r.suppressNames) - 1
			if local != r.suppressNames[top] {
				return nil
			}

			r.suppressNameBytes -= len(r.suppressNames[top])

			r.suppressNames = r.suppressNames[:top]
			if len(r.suppressNames) == 0 {
				if err := r.emitProtected(); err != nil {
					return err
				}

				_, err := r.dst.Write(token)

				return err
			}
		}

		if len(r.suppressNames) > 0 {
			r.protected.append(token...)
		}

		return nil
	}

	if _, err := r.dst.Write(token); err != nil {
		return err
	}

	if kind == 's' && !selfClosing {
		if _, matched := r.elements[local]; matched {
			r.replacements++
			r.suppressNames = append(r.suppressNames[:0], local)
			r.suppressNameBytes = len(local)
			r.protected.reset(r.protected.protector)

			if r.protected.redactImmediately() {
				_, err := io.WriteString(r.dst, redactedValue)
				return err
			}
		}
	}

	return nil
}

func (r *xmlStreamRedactor) emitProtected() error {
	value, _, _ := r.protected.finish()
	if value == "" {
		return nil
	}

	_, err := io.WriteString(r.dst, value)

	return err
}

// xmlMarkupInfo returns s=start, e=end, or 0 for comments, CDATA, processing
// instructions and declarations.
func xmlMarkupInfo(token []byte) (kind byte, local string, selfClosing bool) {
	if len(token) < 3 || token[0] != '<' || token[1] == '!' || token[1] == '?' {
		return 0, "", false
	}

	i := 1

	kind = 's'
	if token[i] == '/' {
		kind = 'e'
		i++
	}

	start := i
	for i < len(token) {
		switch token[i] {
		case ' ', '\t', '\r', '\n', '/', '>':
			goto nameDone

		default:
			i++
		}
	}

nameDone:
	if start == i {
		return 0, "", false
	}

	name := string(token[start:i])
	if colon := strings.LastIndexByte(name, ':'); colon >= 0 {
		name = name[colon+1:]
	}

	local = strings.ToLower(name)

	if kind == 's' {
		j := len(token) - 2
		for j >= 0 && (token[j] == ' ' || token[j] == '\t' || token[j] == '\r' || token[j] == '\n') {
			j--
		}

		selfClosing = j >= 0 && token[j] == '/'
	}

	return kind, local, selfClosing
}
