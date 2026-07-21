package recorder

import (
	"context"
	"net/http"
)

type requestCommentContextKey struct{}

// WithRequestComment returns a context carrying a caller-supplied annotation
// for each physical HTTP exchange started under that context. Recorder stores
// the value verbatim in the standard HAR entry.comment field; callers must not
// include secrets or other data that should be redacted.
//
// Redirect requests inherit the comment through their context. A later call
// replaces the earlier value, and an empty comment omits the HAR field.
func WithRequestComment(ctx context.Context, comment string) context.Context {
	return context.WithValue(ctx, requestCommentContextKey{}, comment)
}

// RequestWithComment clones req with a context carrying a HAR entry comment.
// It returns nil when req is nil and never mutates the supplied request.
func RequestWithComment(req *http.Request, comment string) *http.Request {
	if req == nil {
		return nil
	}

	return req.WithContext(WithRequestComment(req.Context(), comment))
}

func requestCommentFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	comment, _ := ctx.Value(requestCommentContextKey{}).(string)

	return comment
}
