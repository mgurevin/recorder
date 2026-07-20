package recorder

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	maxJSONKeyBytes = 64 << 10
	maxJSONDepth    = 1024
)

var errRedactionLimit = errors.New("recorder: streaming redaction limit exceeded")

const maxBodySniffBytes = 4096

type sniffingBodyRedactor struct {
	dst      io.Writer
	red      *redactor
	buf      []byte
	selected io.WriteCloser
	plain    bool
	err      error
}

func (s *sniffingBodyRedactor) BodyRedactionReport() BodyRedactionReport {
	if reporter, ok := s.selected.(BodyRedactionReporter); ok {
		return reporter.BodyRedactionReport()
	}
	return BodyRedactionReport{}
}

func (s *sniffingBodyRedactor) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.selected != nil {
		return s.selected.Write(p)
	}
	if s.plain {
		return s.dst.Write(p)
	}
	for i, b := range p {
		if len(s.buf) >= maxBodySniffBytes {
			s.err = errRedactionLimit
			return i, s.err
		}
		s.buf = append(s.buf, b)
		if jsonSpace(b) || (len(s.buf) <= 3 && bytes.Equal(s.buf, []byte{0xef, 0xbb, 0xbf}[:len(s.buf)])) {
			continue
		}
		switch b {
		case '{', '[':
			if len(s.red.jsonFields) > 0 {
				s.selected = newJSONStreamRedactor(s.dst, s.red.jsonFields, s.red.protector)
			}
		case '<':
			if len(s.red.xmlElements) > 0 {
				s.selected = newXMLStreamRedactor(s.dst, s.red.xmlElements, s.red.protector)
			}
		}
		if s.selected == nil {
			s.plain = true
			_, s.err = s.dst.Write(s.buf)
		} else {
			_, s.err = s.selected.Write(s.buf)
		}
		s.buf = nil
		if s.err != nil {
			return i + 1, s.err
		}
		if i+1 < len(p) {
			n, err := s.Write(p[i+1:])
			return i + 1 + n, err
		}
		return len(p), nil
	}
	return len(p), nil
}

func (s *sniffingBodyRedactor) Close() error {
	if s.err != nil {
		return s.err
	}
	if s.selected != nil {
		return s.selected.Close()
	}
	if len(s.buf) > 0 {
		_, s.err = s.dst.Write(s.buf)
		s.buf = nil
	}
	return s.err
}

type jsonContainer byte

const (
	jsonRoot jsonContainer = iota
	jsonObject
	jsonArray
)

type jsonState byte

const (
	jsonWantValue jsonState = iota
	jsonWantKey
	jsonWantColon
	jsonWantComma
	jsonDone
)

type jsonFrame struct {
	kind  jsonContainer
	state jsonState
}

// jsonStreamRedactor preserves every non-redacted input byte. It deliberately
// redacts a recognized field even if later bytes make the document malformed:
// a streaming sink cannot roll back bytes already committed to storage.
type jsonStreamRedactor struct {
	dst      io.Writer
	fields   map[string]struct{}
	stack    []jsonFrame
	key      []byte
	keyMatch bool
	inString bool
	keyToken bool
	escaped  bool
	scalar   bool
	bom      []byte
	bomDone  bool

	suppress      bool
	suppressMode  byte // s=string, c=composite, v=scalar
	suppressDepth int
	suppressQuote bool
	suppressEsc   bool
	protected     protectedValueBuffer

	err          error
	replacements int64
}

func (r *jsonStreamRedactor) BodyRedactionReport() BodyRedactionReport {
	return BodyRedactionReport{Replacements: r.replacements, Protection: r.protected.protectionReport()}
}

func newJSONStreamRedactor(dst io.Writer, fields map[string]struct{}, protectors ...*sensitiveValueProtector) *jsonStreamRedactor {
	protector := newSensitiveValueProtector(SensitiveValueProtection{})
	if len(protectors) > 0 && protectors[0] != nil {
		protector = protectors[0]
	}
	r := &jsonStreamRedactor{
		dst:    dst,
		fields: fields,
		stack:  []jsonFrame{{kind: jsonRoot, state: jsonWantValue}},
	}
	r.protected.reset(protector)
	return r
}

func (r *jsonStreamRedactor) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for i, b := range p {
		if err := r.consumeWithBOM(b); err != nil {
			r.err = err
			return i, err
		}
	}
	return len(p), nil
}

func (r *jsonStreamRedactor) Close() error {
	if r.err == nil && len(r.bom) > 0 {
		for _, b := range r.bom {
			if err := r.consume(b); err != nil {
				r.err = err
				break
			}
		}
		r.bom = nil
	}
	if r.err == nil && r.suppress {
		r.err = r.emitProtected()
		r.suppress = false
	}
	return r.err
}

func (r *jsonStreamRedactor) consumeWithBOM(b byte) error {
	if r.bomDone {
		return r.consume(b)
	}
	want := [...]byte{0xef, 0xbb, 0xbf}
	if b == want[len(r.bom)] {
		r.bom = append(r.bom, b)
		if len(r.bom) == len(want) {
			r.bomDone = true
			_, err := r.dst.Write(r.bom)
			r.bom = nil
			return err
		}
		return nil
	}
	r.bomDone = true
	for _, prefixByte := range r.bom {
		if err := r.consume(prefixByte); err != nil {
			return err
		}
	}
	r.bom = nil
	return r.consume(b)
}

func (r *jsonStreamRedactor) emitByte(b byte) error {
	_, err := r.dst.Write([]byte{b})
	return err
}

func (r *jsonStreamRedactor) emitString(s string) error {
	_, err := io.WriteString(r.dst, s)
	return err
}

func jsonSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func jsonDelimiter(b byte) bool {
	return jsonSpace(b) || b == ',' || b == ']' || b == '}'
}

func (r *jsonStreamRedactor) consume(b byte) error {
	if r.suppress {
		done, reprocess, err := r.consumeSuppressed(b)
		if err != nil {
			return err
		}
		if !done || !reprocess {
			return nil
		}
	}

	if r.inString {
		if err := r.emitByte(b); err != nil {
			return err
		}
		if r.keyToken {
			if len(r.key) >= maxJSONKeyBytes {
				return errRedactionLimit
			}
			r.key = append(r.key, b)
		}
		if r.escaped {
			r.escaped = false
			return nil
		}
		if b == '\\' {
			r.escaped = true
			return nil
		}
		if b != '"' {
			return nil
		}
		r.inString = false
		if r.keyToken {
			var key string
			if err := json.Unmarshal(r.key, &key); err == nil {
				_, r.keyMatch = r.fields[strings.ToLower(key)]
			}
			r.key = r.key[:0]
			r.keyToken = false
			r.top().state = jsonWantColon
		}
		return nil
	}

	if r.scalar {
		if !jsonDelimiter(b) {
			return r.emitByte(b)
		}
		r.scalar = false
		// The delimiter belongs to the containing structure.
	}

	f := r.top()
	switch f.state {
	case jsonWantKey:
		if err := r.emitByte(b); err != nil {
			return err
		}
		if jsonSpace(b) || b == '}' {
			if b == '}' {
				r.pop()
			}
			return nil
		}
		if b == '"' {
			r.inString, r.keyToken = true, true
			r.key = append(r.key[:0], b)
		}
		return nil

	case jsonWantColon:
		if err := r.emitByte(b); err != nil {
			return err
		}
		if b == ':' {
			f.state = jsonWantValue
		}
		return nil

	case jsonWantComma:
		if err := r.emitByte(b); err != nil {
			return err
		}
		if jsonSpace(b) {
			return nil
		}
		if b == ',' {
			if f.kind == jsonObject {
				f.state = jsonWantKey
			} else {
				f.state = jsonWantValue
			}
		} else if (b == '}' && f.kind == jsonObject) || (b == ']' && f.kind == jsonArray) {
			r.pop()
		}
		return nil

	case jsonDone:
		if jsonSpace(b) {
			return r.emitByte(b)
		}
		// NDJSON and JSON text sequences contain multiple top-level values.
		// Reset only after the previous container/value has completed, then
		// process this byte as the beginning of the next document.
		f.state = jsonWantValue
		return r.consume(b)
	}

	// jsonWantValue
	if jsonSpace(b) {
		return r.emitByte(b)
	}
	if f.kind == jsonArray && b == ']' {
		if err := r.emitByte(b); err != nil {
			return err
		}
		r.pop()
		return nil
	}
	if f.kind == jsonObject && r.keyMatch {
		r.keyMatch = false
		r.replacements++
		r.markValue()
		return r.startSuppression(b)
	}
	r.markValue()
	if err := r.emitByte(b); err != nil {
		return err
	}
	switch b {
	case '{':
		return r.push(jsonObject, jsonWantKey)
	case '[':
		return r.push(jsonArray, jsonWantValue)
	case '"':
		r.inString = true
	default:
		r.scalar = true
	}
	return nil
}

func (r *jsonStreamRedactor) top() *jsonFrame { return &r.stack[len(r.stack)-1] }

func (r *jsonStreamRedactor) markValue() {
	f := r.top()
	if f.kind == jsonRoot {
		f.state = jsonDone
	} else {
		f.state = jsonWantComma
	}
}

func (r *jsonStreamRedactor) push(kind jsonContainer, state jsonState) error {
	if len(r.stack) >= maxJSONDepth {
		return errRedactionLimit
	}
	r.stack = append(r.stack, jsonFrame{kind: kind, state: state})
	return nil
}

func (r *jsonStreamRedactor) pop() {
	if len(r.stack) > 1 {
		r.stack = r.stack[:len(r.stack)-1]
	}
}

func (r *jsonStreamRedactor) startSuppression(b byte) error {
	r.suppress = true
	r.protected.reset(r.protected.protector)
	if r.protected.redactImmediately() {
		if err := r.emitJSONProtection(redactedValue); err != nil {
			return err
		}
	} else {
		r.protected.append(b)
	}
	switch b {
	case '"':
		r.suppressMode = 's'
	case '{', '[':
		r.suppressMode = 'c'
		r.suppressDepth = 1
	default:
		r.suppressMode = 'v'
	}
	return nil
}

func (r *jsonStreamRedactor) consumeSuppressed(b byte) (done, reprocess bool, err error) {
	switch r.suppressMode {
	case 's':
		r.protected.append(b)
		if r.suppressEsc {
			r.suppressEsc = false
			return false, false, nil
		}
		if b == '\\' {
			r.suppressEsc = true
		} else if b == '"' {
			r.suppress = false
			return true, false, r.emitProtected()
		}
	case 'c':
		r.protected.append(b)
		if r.suppressQuote {
			if r.suppressEsc {
				r.suppressEsc = false
			} else if b == '\\' {
				r.suppressEsc = true
			} else if b == '"' {
				r.suppressQuote = false
			}
			return false, false, nil
		}
		switch b {
		case '"':
			r.suppressQuote = true
		case '{', '[':
			r.suppressDepth++
			if r.suppressDepth > maxJSONDepth {
				return false, false, errRedactionLimit
			}
		case '}', ']':
			r.suppressDepth--
			if r.suppressDepth == 0 {
				r.suppress = false
				return true, false, r.emitProtected()
			}
		}
	case 'v':
		if jsonDelimiter(b) {
			r.suppress = false
			return true, true, r.emitProtected()
		}
		r.protected.append(b)
	}
	return false, false, nil
}

func (r *jsonStreamRedactor) emitProtected() error {
	value, _, _ := r.protected.finish()
	if value == "" {
		return nil
	}
	return r.emitJSONProtection(value)
}

func (r *jsonStreamRedactor) emitJSONProtection(value string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.dst.Write(encoded)
	return err
}
