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
	Redact(dst io.Writer, contentType string, protector BodyValueProtector) (io.WriteCloser, error)
}

// BodyValueProtector creates bounded streaming values that apply the
// transport's configured sensitive-value mode and token format. It is scoped
// to one body writer and must not be retained after that writer closes.
type BodyValueProtector interface {
	NewValue() BodyValue
}

// BodyValue accepts one selected plaintext value in chunks. FinishTo writes its
// protected representation and records exactly one replacement. Encryption
// buffers only up to the configured value limit, tokenization streams through
// HMAC, and failures or oversized values fail closed to [REDACTED]. FinishTo
// may be repeated to write the same result, but a BodyValue must not receive
// more plaintext after its first FinishTo call.
type BodyValue interface {
	io.Writer
	FinishTo(io.Writer) error
}

type bodyRedactorFunc func(io.Writer, string, BodyValueProtector) (io.WriteCloser, error)

func (f bodyRedactorFunc) Redact(dst io.Writer, contentType string, protector BodyValueProtector) (io.WriteCloser, error) {
	return f(dst, contentType, protector)
}

type writerOnly struct{ io.Writer }

type failedBodyRedactor struct{ err error }

func (w *failedBodyRedactor) Write([]byte) (int, error) { return 0, w.err }
func (w *failedBodyRedactor) Close() error              { return w.err }

type protectionAwareBodyRedactorWriter struct {
	inner     io.WriteCloser
	protector *bodyValueProtector
}

func (w *protectionAwareBodyRedactorWriter) Write(p []byte) (int, error) {
	return w.inner.Write(p)
}

func (w *protectionAwareBodyRedactorWriter) Close() error {
	return w.inner.Close()
}

func (w *protectionAwareBodyRedactorWriter) bodyRedactionReport() (ProtectionCounts, int64) {
	return w.protector.protectionReport()
}

func (w *protectionAwareBodyRedactorWriter) bodyProtectionFailure() (error, int64) {
	return w.protector.protectionFailure()
}

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

func (w *safeBodyRedactorWriter) bodyRedactionReport() (protection ProtectionCounts, replacements int64) {
	if reporter, ok := w.inner.(interface {
		bodyRedactionReport() (ProtectionCounts, int64)
	}); ok {
		return reporter.bodyRedactionReport()
	}

	return ProtectionCounts{}, 0
}

func (w *safeBodyRedactorWriter) bodyProtectionFailure() (err error, count int64) {
	reporter, ok := w.inner.(interface{ bodyProtectionFailure() (error, int64) })
	if !ok {
		return nil, 0
	}

	defer func() {
		if recover() != nil {
			err, count = nil, 0
		}
	}()

	err, count = reporter.bodyProtectionFailure()

	return err, count
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
		info := BodyRedactionInfo{Kind: w.kind, Outcome: BodyRedactionUnchanged}
		w.mu.Lock()
		writeFailed := w.writeErr != nil
		w.mu.Unlock()

		if writeFailed || w.closeErr != nil {
			info.Outcome = BodyRedactionFailed
		} else if reporter, ok := w.inner.(interface {
			bodyRedactionReport() (ProtectionCounts, int64)
		}); ok {
			protection, replacements := reporter.bodyRedactionReport()
			if replacements < 0 {
				replacements = 0
			}

			if !protectionCountsEmpty(protection) {
				copy := cloneProtectionCounts(protection)
				info.Protection = &copy
			}

			if replacements > 0 {
				info.Replacements = &replacements
				info.Outcome = BodyRedactionRedacted
			} else {
				info.Replacements = &replacements
				info.Outcome = BodyRedactionUnchanged
			}
		}

		if reporter, ok := w.inner.(interface{ bodyProtectionFailure() (error, int64) }); ok {
			err, count := reporter.bodyProtectionFailure()
			w.audit.addProtectionFailure(w.direction, err, count)
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
		return bodyRedactorFunc(func(dst io.Writer, contentType string, protector BodyValueProtector) (io.WriteCloser, error) {
			w := newMultipartStreamRedactor(dst, contentType, red.query, builtinBodyValueProtector(protector))
			if w.err != nil {
				return nil, w.err
			}

			return w, nil
		}), "builtin:multipart"

	case isFormMime(contentType) && len(red.query) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string, protector BodyValueProtector) (io.WriteCloser, error) {
			return newFormStreamRedactor(dst, red.query, builtinBodyValueProtector(protector)), nil
		}), "builtin:form"

	case isJSONMime(contentType) && len(red.jsonFields) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string, protector BodyValueProtector) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, red.jsonFields, builtinBodyValueProtector(protector)), nil
		}), "builtin:json"

	case isXMLMime(contentType) && len(red.xmlElements) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string, protector BodyValueProtector) (io.WriteCloser, error) {
			return newXMLStreamRedactor(dst, red.xmlElements, builtinBodyValueProtector(protector)), nil
		}), "builtin:xml"

	case !isFormMime(contentType) && !isMultipartFormMime(contentType) &&
		!isJSONMime(contentType) && !isXMLMime(contentType) &&
		(len(red.jsonFields) > 0 || len(red.xmlElements) > 0):
		return bodyRedactorFunc(func(dst io.Writer, _ string, protector BodyValueProtector) (io.WriteCloser, error) {
			return &sniffingBodyRedactor{dst: dst, red: red, protector: builtinBodyValueProtector(protector)}, nil
		}), "builtin:sniff"

	default:
		return nil, ""
	}
}

func builtinBodyValueProtector(protector BodyValueProtector) *bodyValueProtector {
	return protector.(*bodyValueProtector)
}

func newBodyStreamRedactor(dst io.Writer, contentType string, red *redactor) io.WriteCloser {
	selected, kind := selectBodyRedactor(contentType, red)
	if selected == nil {
		return nil
	}

	inner, err := openBodyRedactor(selected, writerOnly{dst}, contentType, red.protector)
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

func openBodyRedactor(redactor BodyRedactor, dst io.Writer, contentType string, protector *sensitiveValueProtector) (writer io.WriteCloser, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			writer = nil
			err = fmt.Errorf("recorder: panic in BodyRedactor.Redact: %v", recovered)
		}
	}()

	session := newBodyValueProtector(protector)

	inner, openErr := redactor.Redact(dst, contentType, session)
	if openErr != nil || inner == nil {
		return inner, openErr
	}

	return &protectionAwareBodyRedactorWriter{inner: inner, protector: session}, nil
}

func bodyStreamRedactionEnabled(contentType string, red *redactor) bool {
	selected, _ := selectBodyRedactor(contentType, red)
	return selected != nil
}
