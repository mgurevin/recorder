package recorder

import (
	"fmt"
	"io"
	"sync"
)

// BodyRedactor creates a streaming transform for one captured body. Redact
// may be called concurrently for different bodies; each returned writer is
// owned by one body and each input byte passes through it once. Close must
// flush parser state but must not close dst.
type BodyRedactor interface {
	Redact(dst io.Writer, contentType string) (io.WriteCloser, error)
}

// BodyRedactionReport is an optional result exposed by a writer returned from
// BodyRedactor.Redact. It must not contain field names or original values.
type BodyRedactionReport struct {
	Replacements int64
	Protection   ProtectionCounts
}

// BodyRedactionReporter may be implemented by body-redactor writers to make
// replacement counts available in Entry.Redaction. It is queried after Close.
type BodyRedactionReporter interface {
	BodyRedactionReport() BodyRedactionReport
}

type bodyRedactorFunc func(io.Writer, string) (io.WriteCloser, error)

func (f bodyRedactorFunc) Redact(dst io.Writer, contentType string) (io.WriteCloser, error) {
	return f(dst, contentType)
}

type writerOnly struct{ io.Writer }

type failedBodyRedactor struct{ err error }

func (w *failedBodyRedactor) Write([]byte) (int, error) { return 0, w.err }
func (w *failedBodyRedactor) Close() error              { return w.err }

// safeBodyRedactorWriter normalizes custom and built-in writers to the same
// panic, short-write, and exactly-once Close behavior.
type safeBodyRedactorWriter struct {
	inner     io.WriteCloser
	closeOnce sync.Once
	closeErr  error
}

func (w *safeBodyRedactorWriter) Write(p []byte) (n int, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			n = 0
			err = fmt.Errorf("recorder: panic in body redactor Write: %v", recovered)
		}
	}()
	n, err = w.inner.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *safeBodyRedactorWriter) Close() error {
	w.closeOnce.Do(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				w.closeErr = fmt.Errorf("recorder: panic in body redactor Close: %v", recovered)
			}
		}()
		w.closeErr = w.inner.Close()
	})
	return w.closeErr
}

func (w *safeBodyRedactorWriter) BodyRedactionReport() BodyRedactionReport {
	report, _ := w.bodyRedactionReport()
	return report
}

func (w *safeBodyRedactorWriter) bodyRedactionReport() (report BodyRedactionReport, available bool) {
	reporter, ok := w.inner.(BodyRedactionReporter)
	if !ok {
		return BodyRedactionReport{}, false
	}
	defer func() {
		if recover() != nil {
			report = BodyRedactionReport{}
			available = false
		}
	}()
	return reporter.BodyRedactionReport(), true
}

type auditedBodyRedactorWriter struct {
	inner     io.WriteCloser
	audit     *redactionAudit
	direction BodyDirection
	kind      string
	mu        sync.Mutex
	writeErr  error
	closeOnce sync.Once
	closeErr  error
}

func (w *auditedBodyRedactorWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if err != nil {
		w.mu.Lock()
		if w.writeErr == nil {
			w.writeErr = err
		}
		w.mu.Unlock()
	}
	return n, err
}

func (w *auditedBodyRedactorWriter) Close() error {
	w.closeOnce.Do(func() {
		w.closeErr = w.inner.Close()
		info := BodyRedactionInfo{Kind: w.kind, Outcome: BodyRedactionProcessed}
		w.mu.Lock()
		writeFailed := w.writeErr != nil
		w.mu.Unlock()
		if writeFailed || w.closeErr != nil {
			info.Outcome = BodyRedactionFailed
		} else if reporter, ok := w.inner.(interface {
			bodyRedactionReport() (BodyRedactionReport, bool)
		}); ok {
			if report, available := reporter.bodyRedactionReport(); available {
				replacements := report.Replacements
				if replacements < 0 {
					replacements = 0
				}
				info.Replacements = &replacements
				if !protectionCountsEmpty(report.Protection) {
					protection := cloneProtectionCounts(report.Protection)
					info.Protection = &protection
				}
				if replacements > 0 {
					info.Outcome = BodyRedactionRedacted
				} else {
					info.Outcome = BodyRedactionUnchanged
				}
			}
		}
		w.audit.setBody(w.direction, info)
	})
	return w.closeErr
}

func selectBodyRedactor(contentType string, red *redactor) (BodyRedactor, string) {
	if red == nil {
		return nil, ""
	}
	if custom := red.bodyRedactors[baseMimeType(contentType)]; custom != nil {
		return custom, "custom"
	}
	switch {
	case isMultipartFormMime(contentType) && len(red.query) > 0:
		return bodyRedactorFunc(func(dst io.Writer, contentType string) (io.WriteCloser, error) {
			w := newMultipartStreamRedactor(dst, contentType, red.query, red.protector)
			if w.err != nil {
				return nil, w.err
			}
			return w, nil
		}), "builtin:multipart"
	case isFormMime(contentType) && len(red.query) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return newFormStreamRedactor(dst, red.query, red.protector), nil
		}), "builtin:form"
	case isJSONMime(contentType) && len(red.jsonFields) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, red.jsonFields, red.protector), nil
		}), "builtin:json"
	case isXMLMime(contentType) && len(red.xmlElements) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return newXMLStreamRedactor(dst, red.xmlElements, red.protector), nil
		}), "builtin:xml"
	case !isFormMime(contentType) && !isMultipartFormMime(contentType) &&
		!isJSONMime(contentType) && !isXMLMime(contentType) &&
		(len(red.jsonFields) > 0 || len(red.xmlElements) > 0):
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return &sniffingBodyRedactor{dst: dst, red: red}, nil
		}), "builtin:sniff"
	default:
		return nil, ""
	}
}

func newBodyStreamRedactor(dst io.Writer, contentType string, red *redactor) io.WriteCloser {
	selected, kind := selectBodyRedactor(contentType, red)
	if selected == nil {
		return nil
	}
	inner, err := openBodyRedactor(selected, writerOnly{dst}, contentType)
	if err != nil {
		inner = &failedBodyRedactor{err: err}
	} else if inner == nil {
		inner = &failedBodyRedactor{err: fmt.Errorf("recorder: body redactor returned a nil writer")}
	} else {
		inner = &safeBodyRedactorWriter{inner: inner}
	}
	if red.audit != nil {
		return &auditedBodyRedactorWriter{inner: inner, audit: red.audit, direction: red.direction, kind: kind}
	}
	return inner
}

func openBodyRedactor(redactor BodyRedactor, dst io.Writer, contentType string) (writer io.WriteCloser, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			writer = nil
			err = fmt.Errorf("recorder: panic in BodyRedactor.Redact: %v", recovered)
		}
	}()
	return redactor.Redact(dst, contentType)
}

func bodyStreamRedactionEnabled(contentType string, red *redactor) bool {
	selected, _ := selectBodyRedactor(contentType, red)
	return selected != nil
}
