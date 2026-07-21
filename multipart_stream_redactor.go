package recorder

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
)

const (
	maxMultipartHeaderBytes   = 64 << 10
	maxMultipartPreambleBytes = 4 << 10
	maxMultipartBoundaryBytes = 70
	multipartFeedChunkBytes   = 32 << 10
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
	dst          io.Writer
	fields       map[string]struct{}
	boundary     []byte
	marker       []byte
	state        multipartState
	pending      []byte
	suppress     bool
	err          error
	replacements int64
	protected    protectedValueBuffer
}

func (r *multipartStreamRedactor) BodyRedactionReport() BodyRedactionReport {
	return BodyRedactionReport{Replacements: r.replacements, Protection: r.protected.protectionReport()}
}

func (r *multipartStreamRedactor) bodyProtectionFailure() (error, int64) {
	return r.protected.protectionFailure()
}

func newMultipartStreamRedactor(dst io.Writer, mimeType string, fields map[string]struct{}, protectors ...*sensitiveValueProtector) *multipartStreamRedactor {
	r := &multipartStreamRedactor{dst: dst, fields: fields, state: multipartPreamble}

	protector := newSensitiveValueProtector(SensitiveValueProtection{})
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
			r.protected.append(r.pending...)
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

	lineMarker := append([]byte("\r\n"), r.marker...)
	if i := bytes.Index(r.pending, lineMarker); i >= 0 {
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

	r.pending = r.pending[end:]
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

	rewritten, matched, nested, err := parseMultipartHeadersReported(block, r.fields, r.protected.protector,
		func(mode ProtectionMode, reason string, err error) {
			r.protected.record(mode, reason)
			r.protected.recordFailure(err)
		})
	if err != nil {
		return false, err
	}

	if nested && !matched {
		return false, fmt.Errorf("%w: nested multipart in unmatched part", errMalformedMultipart)
	}

	if _, err := r.dst.Write(rewritten); err != nil {
		return false, err
	}

	r.pending = r.pending[end:]

	r.suppress = matched
	if matched {
		r.replacements++
		r.protected.reset(r.protected.protector)

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
	needle := append([]byte("\r\n"), r.marker...)
	for {
		i := bytes.Index(r.pending, needle)
		if i < 0 {
			keep := len(needle) - 1

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
				r.protected.append(r.pending[:emit]...)
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
					r.protected.append(r.pending[:i]...)
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
			r.protected.append(r.pending[:i]...)

			if err := r.emitProtected(); err != nil {
				return false, err
			}
		}

		if _, err := r.dst.Write(r.pending[i:end]); err != nil {
			return false, err
		}

		r.pending = r.pending[end:]

		r.suppress = false
		if closing {
			r.state = multipartEpilogue
		} else {
			r.state = multipartHeaders
		}

		return true, nil
	}
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

	return parseMultipartHeadersReported(block, fields, protector, nil)
}

func parseMultipartHeadersReported(block []byte, fields map[string]struct{}, protector *sensitiveValueProtector, report func(ProtectionMode, string, error)) ([]byte, bool, bool, error) {
	lines := bytes.Split(block[:len(block)-4], []byte("\r\n"))
	cdIndex := -1

	var (
		disposition string
		contentType string
	)

	for i, line := range lines {
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			return nil, false, false, fmt.Errorf("%w: folded or empty part header", errMalformedMultipart)
		}

		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			return nil, false, false, fmt.Errorf("%w: invalid part header", errMalformedMultipart)
		}

		name := strings.ToLower(string(bytes.TrimSpace(line[:colon])))
		value := strings.TrimSpace(string(line[colon+1:]))

		switch name {
		case "content-disposition":
			if cdIndex >= 0 {
				return nil, false, false, fmt.Errorf("%w: duplicate content-disposition", errMalformedMultipart)
			}

			cdIndex, disposition = i, value

		case "content-type":
			if contentType != "" {
				return nil, false, false, fmt.Errorf("%w: duplicate content-type", errMalformedMultipart)
			}

			contentType = value
		}
	}

	if cdIndex < 0 {
		return nil, false, false, fmt.Errorf("%w: missing content-disposition", errMalformedMultipart)
	}

	dispType, params, err := mime.ParseMediaType(disposition)
	if err != nil || !strings.EqualFold(dispType, "form-data") || params["name"] == "" {
		return nil, false, false, fmt.Errorf("%w: invalid content-disposition", errMalformedMultipart)
	}

	_, matched := fields[strings.ToLower(params["name"])]
	nested := false

	if contentType != "" {
		if mt, _, parseErr := mime.ParseMediaType(contentType); parseErr != nil {
			return nil, false, false, fmt.Errorf("%w: invalid part content-type", errMalformedMultipart)
		} else {
			nested = strings.HasPrefix(strings.ToLower(mt), "multipart/")
		}
	}

	if !matched || params["filename"] == "" {
		return block, matched, nested, nil
	}

	protectedFilename, mode, reason, protectionErr := protector.protectWithError([]byte(params["filename"]))
	if report != nil {
		report(mode, reason, protectionErr)
	}

	params["filename"] = protectedFilename

	formatted := mime.FormatMediaType(dispType, params)
	if formatted == "" {
		return nil, false, false, fmt.Errorf("%w: cannot rewrite content-disposition", errMalformedMultipart)
	}

	line := lines[cdIndex]
	colon := bytes.IndexByte(line, ':')

	spaceEnd := colon + 1
	for spaceEnd < len(line) && (line[spaceEnd] == ' ' || line[spaceEnd] == '\t') {
		spaceEnd++
	}

	lines[cdIndex] = append(append([]byte(nil), line[:spaceEnd]...), formatted...)

	return append(bytes.Join(lines, []byte("\r\n")), []byte("\r\n\r\n")...), matched, nested, nil
}
