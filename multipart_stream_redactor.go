package recorder

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"
)

const (
	maxMultipartHeaderBytes   = 64 << 10
	maxMultipartPreambleBytes = 4 << 10
	maxMultipartBoundaryBytes = 70
	multipartFeedChunkBytes   = 32 << 10
	multipartCompactBytes     = 4 << 10
)

var errMalformedMultipart = errors.New("recorder: malformed multipart body")

type multipartState byte

const (
	multipartPreamble multipartState = iota
	multipartHeaders
	multipartBody
	multipartEpilogue
)

// multipartStreamRedactor preserves preamble, delimiters, unmatched parts,
// and unmatched headers byte-for-byte. It buffers one part header block and
// a boundary-sized lookbehind; matched part bodies are discarded as they
// stream.
type multipartStreamRedactor struct {
	dst        io.Writer
	fields     map[string]struct{}
	boundary   []byte
	marker     []byte
	lineMarker []byte
	headerFold []byte
	state      multipartState
	pending    []byte
	suppress   bool
	err        error
	protected  protectedValueBuffer
}

func newMultipartStreamRedactor(dst io.Writer, mimeType string, fields map[string]struct{}, protectors ...*bodyValueProtector) *multipartStreamRedactor {
	r := &multipartStreamRedactor{dst: dst, fields: fields, state: multipartPreamble}

	protector := newBodyValueProtector(nil)
	if len(protectors) > 0 && protectors[0] != nil {
		protector = protectors[0]
	}

	r.protected.reset(protector)

	mt, params, err := mime.ParseMediaType(mimeType)

	boundary := params["boundary"]
	if err != nil || !strings.EqualFold(mt, "multipart/form-data") || !validMultipartBoundary(boundary) {
		r.err = fmt.Errorf("%w: invalid or missing boundary", errMalformedMultipart)
		return r
	}

	r.boundary = []byte(boundary)
	r.marker = []byte("--" + boundary)
	r.lineMarker = append([]byte("\r\n"), r.marker...)

	return r
}

func validMultipartBoundary(boundary string) bool {
	if len(boundary) == 0 || len(boundary) > maxMultipartBoundaryBytes || boundary[len(boundary)-1] == ' ' {
		return false
	}

	for i := 0; i < len(boundary); i++ {
		b := boundary[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
			continue
		}

		if !strings.ContainsRune("'()+_,-./:=? ", rune(b)) {
			return false
		}
	}

	return true
}

func (r *multipartStreamRedactor) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	written := 0

	for len(p) > 0 {
		take := min(len(p), multipartFeedChunkBytes)

		r.pending = append(r.pending, p[:take]...)
		if err := r.process(false); err != nil {
			r.err = err
			return written, err
		}

		written += take
		p = p[take:]
	}

	return written, nil
}

func (r *multipartStreamRedactor) Close() error {
	if r.err != nil {
		return r.err
	}

	if err := r.process(true); err != nil {
		r.err = err
		return err
	}

	switch r.state {
	case multipartBody:
		if !r.suppress {
			_, r.err = r.dst.Write(r.pending)
		} else {
			r.protected.appendBytes(r.pending)
			r.err = r.emitProtected()
		}

		r.pending = nil
		if r.err == nil {
			r.err = errMalformedMultipart
		}

	case multipartEpilogue:
		_, r.err = r.dst.Write(r.pending)
		r.pending = nil

	default:
		r.err = errMalformedMultipart
	}

	return r.err
}

func (r *multipartStreamRedactor) process(final bool) error {
	for {
		switch r.state {
		case multipartPreamble:
			progress, err := r.processPreamble(final)
			if err != nil || !progress {
				return err
			}

		case multipartHeaders:
			progress, err := r.processHeaders()
			if err != nil || !progress {
				return err
			}

		case multipartBody:
			progress, err := r.processBody(final)
			if err != nil || !progress {
				return err
			}

		case multipartEpilogue:
			if len(r.pending) > 0 {
				_, err := r.dst.Write(r.pending)
				r.pending = nil

				return err
			}

			return nil
		}
	}
}

func (r *multipartStreamRedactor) processPreamble(final bool) (bool, error) {
	positions := []int{}
	if bytes.HasPrefix(r.pending, r.marker) {
		positions = append(positions, 0)
	}

	if i := bytes.Index(r.pending, r.lineMarker); i >= 0 {
		positions = append(positions, i+2)
	}

	if len(positions) == 0 {
		if len(r.pending) > maxMultipartPreambleBytes || final {
			return false, errMalformedMultipart
		}

		return false, nil
	}

	pos := positions[0]

	end, closing, status := r.delimiterEnd(pos, final)
	if status == delimiterNeedMore {
		if len(r.pending) > maxMultipartPreambleBytes+maxMultipartBoundaryBytes+8 {
			return false, errMalformedMultipart
		}

		return false, nil
	}

	if status == delimiterInvalid {
		return false, errMalformedMultipart
	}

	if _, err := r.dst.Write(r.pending[:end]); err != nil {
		return false, err
	}

	r.consumePending(end)

	if closing {
		r.state = multipartEpilogue
	} else {
		r.state = multipartHeaders
	}

	return true, nil
}

func (r *multipartStreamRedactor) processHeaders() (bool, error) {
	i := bytes.Index(r.pending, []byte("\r\n\r\n"))
	if i < 0 {
		if len(r.pending) > maxMultipartHeaderBytes {
			return false, errRedactionLimit
		}

		return false, nil
	}

	end := i + 4
	block := r.pending[:end]

	rewritten, matched, nested, err := parseMultipartHeadersReportedWithScratch(
		block,
		r.fields,
		r.protected.session,
		&r.headerFold,
	)
	if err != nil {
		return false, err
	}

	if nested && !matched {
		return false, fmt.Errorf("%w: nested multipart in unmatched part", errMalformedMultipart)
	}

	if _, err := r.dst.Write(rewritten); err != nil {
		return false, err
	}

	r.consumePending(end)

	r.suppress = matched
	if matched {
		r.protected.reset(r.protected.session)

		if r.protected.redactImmediately() {
			if _, err := io.WriteString(r.dst, redactedValue); err != nil {
				return false, err
			}
		}
	}

	r.state = multipartBody

	return true, nil
}

func (r *multipartStreamRedactor) processBody(final bool) (bool, error) {
	for {
		i := bytes.Index(r.pending, r.lineMarker)
		if i < 0 {
			keep := len(r.lineMarker) - 1

			if final {
				return false, nil
			}

			if len(r.pending) <= keep {
				return false, nil
			}

			emit := len(r.pending) - keep
			if !r.suppress {
				if _, err := r.dst.Write(r.pending[:emit]); err != nil {
					return false, err
				}
			} else {
				r.protected.appendBytes(r.pending[:emit])
			}

			r.pending = append(r.pending[:0], r.pending[emit:]...)

			return false, nil
		}

		pos := i + 2

		end, closing, status := r.delimiterEnd(pos, final)
		if status == delimiterNeedMore {
			if i > 0 {
				if !r.suppress {
					if _, err := r.dst.Write(r.pending[:i]); err != nil {
						return false, err
					}
				} else {
					r.protected.appendBytes(r.pending[:i])
				}

				r.pending = append(r.pending[:0], r.pending[i:]...)
			}

			return false, nil
		}

		if status == delimiterInvalid {
			emit := i + 2
			if !r.suppress {
				if _, err := r.dst.Write(r.pending[:emit]); err != nil {
					return false, err
				}
			}

			r.pending = append(r.pending[:0], r.pending[emit:]...)

			continue
		}

		if !r.suppress {
			if _, err := r.dst.Write(r.pending[:i]); err != nil {
				return false, err
			}
		}

		if r.suppress {
			r.protected.appendBytes(r.pending[:i])

			if err := r.emitProtected(); err != nil {
				return false, err
			}
		}

		if _, err := r.dst.Write(r.pending[i:end]); err != nil {
			return false, err
		}

		r.consumePending(end)

		r.suppress = false
		if closing {
			r.state = multipartEpilogue
		} else {
			r.state = multipartHeaders
		}

		return true, nil
	}
}

func (r *multipartStreamRedactor) consumePending(n int) {
	remaining := len(r.pending) - n
	if remaining <= multipartCompactBytes {
		copy(r.pending, r.pending[n:])
		r.pending = r.pending[:remaining]

		return
	}

	r.pending = r.pending[n:]
}

func (r *multipartStreamRedactor) emitProtected() error {
	value, _, _ := r.protected.finish()
	if value == "" {
		return nil
	}

	_, err := io.WriteString(r.dst, value)

	return err
}

type delimiterStatus byte

const (
	delimiterNeedMore delimiterStatus = iota
	delimiterInvalid
	delimiterValid
)

func (r *multipartStreamRedactor) delimiterEnd(pos int, final bool) (end int, closing bool, status delimiterStatus) {
	i := pos + len(r.marker)
	if len(r.pending) < i {
		return 0, false, delimiterNeedMore
	}

	if len(r.pending) == i+1 && r.pending[i] == '-' {
		return 0, false, delimiterNeedMore
	}

	if len(r.pending) >= i+2 && bytes.Equal(r.pending[i:i+2], []byte("--")) {
		closing = true
		i += 2
	}

	for i < len(r.pending) && (r.pending[i] == ' ' || r.pending[i] == '\t') {
		i++
	}

	if len(r.pending) >= i+2 && bytes.Equal(r.pending[i:i+2], []byte("\r\n")) {
		return i + 2, closing, delimiterValid
	}

	if closing && final && i == len(r.pending) {
		return i, true, delimiterValid
	}

	if i == len(r.pending) || (i+1 == len(r.pending) && r.pending[i] == '\r') {
		return 0, false, delimiterNeedMore
	}

	return 0, false, delimiterInvalid
}

func parseMultipartHeaders(block []byte, fields map[string]struct{}, protectors ...*sensitiveValueProtector) ([]byte, bool, bool, error) {
	protector := newSensitiveValueProtector(SensitiveValueProtection{})
	if len(protectors) > 0 && protectors[0] != nil {
		protector = protectors[0]
	}

	return parseMultipartHeadersReported(block, fields, newBodyValueProtector(protector))
}

func parseMultipartHeadersReported(block []byte, fields map[string]struct{}, protector BodyValueProtector) ([]byte, bool, bool, error) {
	return parseMultipartHeadersReportedWithScratch(block, fields, protector, nil)
}

type multipartHeaderInfo struct {
	disposition           []byte
	contentType           []byte
	dispositionValueStart int
	dispositionValueEnd   int
}

func parseMultipartHeadersReportedWithScratch(
	block []byte,
	fields map[string]struct{},
	protector BodyValueProtector,
	fold *[]byte,
) ([]byte, bool, bool, error) {
	info, err := scanMultipartHeaders(block)
	if err != nil {
		return nil, false, false, err
	}

	name, hasFilename, simple := parseSimpleFormData(info.disposition)
	if simple {
		matched := multipartFieldMatches(fields, name, fold)

		nested, err := multipartContentTypeIsNested(info.contentType)
		if err != nil {
			return nil, false, false, err
		}

		if !matched || !hasFilename {
			return block, matched, nested, nil
		}
	}

	dispType, params, err := mime.ParseMediaType(string(info.disposition))
	if err != nil || !strings.EqualFold(dispType, "form-data") || params["name"] == "" {
		return nil, false, false, fmt.Errorf("%w: invalid content-disposition", errMalformedMultipart)
	}

	matched := multipartFieldMatches(fields, []byte(params["name"]), fold)

	nested, err := multipartContentTypeIsNested(info.contentType)
	if err != nil {
		return nil, false, false, err
	}

	if !matched || params["filename"] == "" {
		return block, matched, nested, nil
	}

	value := protector.NewValue()
	if _, err := io.WriteString(value, params["filename"]); err != nil {
		return nil, false, false, fmt.Errorf("%w: protect filename: %v", errMalformedMultipart, err)
	}

	params["filename"] = value.Finish()

	formatted := mime.FormatMediaType(dispType, params)
	if formatted == "" {
		return nil, false, false, fmt.Errorf("%w: cannot rewrite content-disposition", errMalformedMultipart)
	}

	rewritten := make([]byte, 0, len(block)-len(info.disposition)+len(formatted))
	rewritten = append(rewritten, block[:info.dispositionValueStart]...)
	rewritten = append(rewritten, formatted...)
	rewritten = append(rewritten, block[info.dispositionValueEnd:]...)

	return rewritten, matched, nested, nil
}

func scanMultipartHeaders(block []byte) (multipartHeaderInfo, error) {
	if len(block) < 4 || !bytes.HasSuffix(block, []byte("\r\n\r\n")) {
		return multipartHeaderInfo{}, fmt.Errorf("%w: incomplete part headers", errMalformedMultipart)
	}

	var info multipartHeaderInfo

	contentEnd := len(block) - 4
	for start := 0; start < contentEnd; {
		lineEnd := contentEnd
		if relative := bytes.Index(block[start:contentEnd], []byte("\r\n")); relative >= 0 {
			lineEnd = start + relative
		}

		line := block[start:lineEnd]
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			return multipartHeaderInfo{}, fmt.Errorf("%w: folded or empty part header", errMalformedMultipart)
		}

		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return multipartHeaderInfo{}, fmt.Errorf("%w: invalid part header", errMalformedMultipart)
		}

		name := bytes.TrimSpace(line[:colon])
		value := bytes.TrimSpace(line[colon+1:])

		switch {
		case bytes.EqualFold(name, []byte("content-disposition")):
			if info.disposition != nil {
				return multipartHeaderInfo{}, fmt.Errorf("%w: duplicate content-disposition", errMalformedMultipart)
			}

			valueStart := start + colon + 1
			for valueStart < lineEnd && (block[valueStart] == ' ' || block[valueStart] == '\t') {
				valueStart++
			}

			info.disposition = value
			info.dispositionValueStart = valueStart
			info.dispositionValueEnd = lineEnd

		case bytes.EqualFold(name, []byte("content-type")):
			if info.contentType != nil {
				return multipartHeaderInfo{}, fmt.Errorf("%w: duplicate content-type", errMalformedMultipart)
			}

			info.contentType = value
		}

		start = lineEnd + 2
	}

	if info.disposition == nil {
		return multipartHeaderInfo{}, fmt.Errorf("%w: missing content-disposition", errMalformedMultipart)
	}

	return info, nil
}

func parseSimpleFormData(value []byte) (name []byte, hasFilename bool, ok bool) {
	position := skipMultipartWhitespace(value, 0)

	end := position
	for end < len(value) && value[end] != ';' {
		end++
	}

	if !bytes.EqualFold(bytes.TrimSpace(value[position:end]), []byte("form-data")) {
		return nil, false, false
	}

	position = end
	seenName := false
	seenFilename := false

	for position < len(value) {
		if value[position] != ';' {
			return nil, false, false
		}

		position = skipMultipartWhitespace(value, position+1)

		attributeStart := position
		for position < len(value) && isMIMETokenByte(value[position]) {
			position++
		}

		if attributeStart == position {
			return nil, false, false
		}

		attribute := value[attributeStart:position]

		position = skipMultipartWhitespace(value, position)
		if position >= len(value) || value[position] != '=' {
			return nil, false, false
		}

		position = skipMultipartWhitespace(value, position+1)

		parameter, next, parsed := parseSimpleMediaParameter(value, position)
		if !parsed {
			return nil, false, false
		}

		position = skipMultipartWhitespace(value, next)

		switch {
		case bytes.EqualFold(attribute, []byte("name")):
			if seenName || len(parameter) == 0 {
				return nil, false, false
			}

			name = parameter
			seenName = true

		case bytes.EqualFold(attribute, []byte("filename")):
			if seenFilename {
				return nil, false, false
			}

			hasFilename = true
			seenFilename = true

		default:
			// Unknown, extended, and continuation parameters need the standard
			// library's duplicate, decoding, and precedence rules.
			return nil, false, false
		}
	}

	return name, hasFilename, seenName
}

func parseSimpleMediaParameter(value []byte, position int) ([]byte, int, bool) {
	if position >= len(value) {
		return nil, position, false
	}

	if value[position] == '"' {
		start := position + 1

		position = start
		for position < len(value) && value[position] != '"' {
			if value[position] == '\\' || value[position] < 0x20 || value[position] >= 0x7f {
				return nil, position, false
			}

			position++
		}

		if position >= len(value) {
			return nil, position, false
		}

		return value[start:position], position + 1, true
	}

	start := position
	for position < len(value) && isMIMETokenByte(value[position]) {
		position++
	}

	if start == position {
		return nil, position, false
	}

	return value[start:position], position, true
}

func multipartFieldMatches(fields map[string]struct{}, name []byte, fold *[]byte) bool {
	hasUpper := false

	for _, b := range name {
		if b >= utf8.RuneSelf {
			_, matched := fields[strings.ToLower(string(name))]

			return matched
		}

		if b >= 'A' && b <= 'Z' {
			hasUpper = true
		}
	}

	if !hasUpper {
		_, matched := fields[string(name)]

		return matched
	}

	if fold != nil {
		folded, _ := foldASCIIName(name, (*fold)[:0])
		*fold = folded
		_, matched := fields[string(folded)]

		return matched
	}

	_, matched := fields[strings.ToLower(string(name))]

	return matched
}

func multipartContentTypeIsNested(value []byte) (bool, error) {
	if len(value) == 0 {
		return false, nil
	}

	if simpleMediaType(value) {
		return len(value) > len("multipart/") && bytes.EqualFold(value[:len("multipart/")], []byte("multipart/")), nil
	}

	mediaType, _, err := mime.ParseMediaType(string(value))
	if err != nil {
		return false, fmt.Errorf("%w: invalid part content-type", errMalformedMultipart)
	}

	return strings.HasPrefix(strings.ToLower(mediaType), "multipart/"), nil
}

func simpleMediaType(value []byte) bool {
	slash := 0

	for _, b := range value {
		if b == '/' {
			slash++
			continue
		}

		if !isMIMETokenByte(b) {
			return false
		}
	}

	return slash == 1
}

func skipMultipartWhitespace(value []byte, position int) int {
	for position < len(value) && (value[position] == ' ' || value[position] == '\t') {
		position++
	}

	return position
}

func isMIMETokenByte(b byte) bool {
	if b <= 0x20 || b >= 0x7f {
		return false
	}

	return !strings.ContainsRune(`()<>@,;:\"/[]?=`, rune(b))
}
