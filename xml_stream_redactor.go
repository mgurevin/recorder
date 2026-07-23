package recorder

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
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
	nameFold []byte
	inMarkup bool
	quote    byte
	brackets int

	suppressNames []xmlNameSpan
	suppressData  []byte
	err           error
	protected     protectedValueBuffer
}

type xmlNameSpan struct {
	start int
	end   int
}

func newXMLStreamRedactor(dst io.Writer, elements map[string]struct{}, protectors ...*bodyValueProtector) *xmlStreamRedactor {
	protector := newBodyValueProtector(nil)
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

		r.protected.appendByte(b)

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
				if len(r.suppressNames) >= maxXMLDepth || len(r.suppressData)+len(local) > maxXMLMarkupBytes {
					return errRedactionLimit
				}

				if err := r.pushSuppressName(local); err != nil {
					return err
				}
			}

		case 'e':
			top := len(r.suppressNames) - 1
			if !r.suppressNameEqual(local, r.suppressNames[top]) {
				return nil
			}

			r.suppressNames = r.suppressNames[:top]
			if len(r.suppressNames) == 0 {
				r.suppressData = r.suppressData[:0]

				if err := r.emitProtected(); err != nil {
					return err
				}

				_, err := r.dst.Write(token)

				return err
			}
		}

		if len(r.suppressNames) > 0 {
			r.protected.appendBytes(token)
		}

		return nil
	}

	if _, err := r.dst.Write(token); err != nil {
		return err
	}

	if kind == 's' && !selfClosing {
		if r.elementMatches(local) {
			r.suppressNames = r.suppressNames[:0]
			r.suppressData = r.suppressData[:0]

			if err := r.pushSuppressName(local); err != nil {
				return err
			}

			r.protected.reset(r.protected.session)

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

func (r *xmlStreamRedactor) elementMatches(local []byte) bool {
	if folded, ok := foldASCIIName(local, r.nameFold[:0]); ok {
		r.nameFold = folded
		_, matched := r.elements[string(folded)]

		return matched
	}

	_, matched := r.elements[strings.ToLower(string(local))]

	return matched
}

func (r *xmlStreamRedactor) pushSuppressName(local []byte) error {
	start := len(r.suppressData)

	if folded, ok := foldASCIIName(local, r.suppressData); ok {
		r.suppressData = folded
	} else {
		r.suppressData = append(r.suppressData, strings.ToLower(string(local))...)
	}

	if len(r.suppressData) > maxXMLMarkupBytes {
		r.suppressData = r.suppressData[:start]

		return errRedactionLimit
	}

	r.suppressNames = append(r.suppressNames, xmlNameSpan{start: start, end: len(r.suppressData)})

	return nil
}

func (r *xmlStreamRedactor) suppressNameEqual(local []byte, span xmlNameSpan) bool {
	saved := r.suppressData[span.start:span.end]
	if folded, ok := foldASCIIName(local, r.nameFold[:0]); ok {
		r.nameFold = folded

		return bytes.Equal(folded, saved)
	}

	return strings.ToLower(string(local)) == string(saved)
}

func foldASCIIName(name, dst []byte) ([]byte, bool) {
	for _, b := range name {
		if b >= utf8.RuneSelf {
			return dst, false
		}
	}

	for _, b := range name {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}

		dst = append(dst, b)
	}

	return dst, true
}

// xmlMarkupInfo returns s=start, e=end, or 0 for comments, CDATA, processing
// instructions and declarations.
func xmlMarkupInfo(token []byte) (kind byte, local []byte, selfClosing bool) {
	if len(token) < 3 || token[0] != '<' || token[1] == '!' || token[1] == '?' {
		return 0, nil, false
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
		return 0, nil, false
	}

	local = token[start:i]
	if colon := bytes.LastIndexByte(local, ':'); colon >= 0 {
		local = local[colon+1:]
	}

	if kind == 's' {
		j := len(token) - 2
		for j >= 0 && (token[j] == ' ' || token[j] == '\t' || token[j] == '\r' || token[j] == '\n') {
			j--
		}

		selfClosing = j >= 0 && token[j] == '/'
	}

	return kind, local, selfClosing
}
