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

func selectBodyRedactor(contentType string, red *redactor) BodyRedactor {
	if red == nil {
		return nil
	}
	if custom := red.bodyRedactors[baseMimeType(contentType)]; custom != nil {
		return custom
	}
	switch {
	case isMultipartFormMime(contentType) && len(red.query) > 0:
		return bodyRedactorFunc(func(dst io.Writer, contentType string) (io.WriteCloser, error) {
			w := newMultipartStreamRedactor(dst, contentType, red.query)
			if w.err != nil {
				return nil, w.err
			}
			return w, nil
		})
	case isFormMime(contentType) && len(red.query) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return newFormStreamRedactor(dst, red.query), nil
		})
	case isJSONMime(contentType) && len(red.jsonFields) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return newJSONStreamRedactor(dst, red.jsonFields), nil
		})
	case isXMLMime(contentType) && len(red.xmlElements) > 0:
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return newXMLStreamRedactor(dst, red.xmlElements), nil
		})
	case !isFormMime(contentType) && !isMultipartFormMime(contentType) &&
		!isJSONMime(contentType) && !isXMLMime(contentType) &&
		(len(red.jsonFields) > 0 || len(red.xmlElements) > 0):
		return bodyRedactorFunc(func(dst io.Writer, _ string) (io.WriteCloser, error) {
			return &sniffingBodyRedactor{dst: dst, red: red}, nil
		})
	default:
		return nil
	}
}

func newBodyStreamRedactor(dst io.Writer, contentType string, red *redactor) io.WriteCloser {
	selected := selectBodyRedactor(contentType, red)
	if selected == nil {
		return nil
	}
	inner, err := openBodyRedactor(selected, writerOnly{dst}, contentType)
	if err != nil {
		return &failedBodyRedactor{err: err}
	}
	if inner == nil {
		return &failedBodyRedactor{err: fmt.Errorf("recorder: body redactor returned a nil writer")}
	}
	return &safeBodyRedactorWriter{inner: inner}
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
	return selectBodyRedactor(contentType, red) != nil
}
