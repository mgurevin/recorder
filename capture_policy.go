package recorder

import (
	"context"
	"fmt"
	"net/http"
)

// BodyDirection identifies which side of an exchange a capture decision
// applies to.
type BodyDirection string

const (
	RequestBody  BodyDirection = "request"
	ResponseBody BodyDirection = "response"
)

// BodyCaptureMeta is the immutable, non-body metadata supplied to a
// BodyCapturePolicy. It excludes query values, headers, and cookies.
// StatusCode is zero for request decisions; Path is URL-escaped.
type BodyCaptureMeta struct {
	Direction       BodyDirection
	Method          string
	Scheme          string
	Host            string
	Path            string
	StatusCode      int
	ContentType     string
	ContentEncoding string
	ContentLength   int64
	TraceID         string
	RedirectIndex   int
}

// BodyCaptureDecision controls capture for one request or response body.
// RedactorOverride, when non-nil, replaces the redactor selected by the
// Transport and request-scoped RedactionConfig for this body only. Nil keeps
// the existing selection; it does not disable redaction.
type BodyCaptureDecision struct {
	Capture          bool
	Embed            bool
	Hash             bool
	MaxBodyBytes     int64
	RedactorOverride BodyRedactor
}

// BodyCapturePolicy decides how one body is recorded. The defaults argument
// reflects the transport's capture Config; RedactorOverride starts nil
// because RedactionConfig selection remains active unless explicitly
// overridden. Implementations may be called concurrently and must not retain
// or mutate HTTP objects.
type BodyCapturePolicy func(context.Context, BodyCaptureMeta, BodyCaptureDecision) (BodyCaptureDecision, error)

func normalizeCaptureDecision(d BodyCaptureDecision) BodyCaptureDecision {
	if !d.Capture {
		d.Embed = false
		d.RedactorOverride = nil
	}

	return d
}

func decideBodyCapture(ctx context.Context, policy BodyCapturePolicy, meta BodyCaptureMeta, defaults BodyCaptureDecision) (decision BodyCaptureDecision, err error) {
	defaults = normalizeCaptureDecision(defaults)
	if policy == nil {
		return defaults, nil
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			decision = BodyCaptureDecision{}
			err = fmt.Errorf("recorder: panic in BodyCapturePolicy: %v", recovered)
		}
	}()

	decision, err = policy(ctx, meta, defaults)
	if err != nil {
		return BodyCaptureDecision{}, fmt.Errorf("recorder: body capture policy: %w", err)
	}

	return normalizeCaptureDecision(decision), nil
}

func requestCaptureMeta(req *http.Request, traceID string, redirectIndex int) BodyCaptureMeta {
	meta := BodyCaptureMeta{
		Direction:     RequestBody,
		Method:        req.Method,
		ContentLength: req.ContentLength,
		TraceID:       traceID,
		RedirectIndex: redirectIndex,
	}
	if req.URL != nil {
		meta.Scheme = req.URL.Scheme
		meta.Host = req.URL.Host

		meta.Path = req.URL.EscapedPath()
		if meta.Path == "" {
			meta.Path = "/"
		}
	}

	meta.ContentType = req.Header.Get("Content-Type")
	meta.ContentEncoding = req.Header.Get("Content-Encoding")

	return meta
}

func responseCaptureMeta(req *http.Request, resp *http.Response, traceID string, redirectIndex int) BodyCaptureMeta {
	meta := requestCaptureMeta(req, traceID, redirectIndex)
	meta.Direction = ResponseBody
	meta.StatusCode = resp.StatusCode
	meta.ContentType = resp.Header.Get("Content-Type")
	meta.ContentEncoding = resp.Header.Get("Content-Encoding")
	meta.ContentLength = resp.ContentLength

	return meta
}
