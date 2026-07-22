package recorder

import (
	"context"
	"net/http"
	"testing"
)

func TestWithRequestCommentReplacementAndRequestClone(t *testing.T) {
	ctx := WithRequestComment(context.Background(), "first")

	ctx = WithRequestComment(ctx, "second")
	if got := requestCommentFromContext(ctx); got != "second" {
		t.Fatalf("comment = %q, want second", got)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}

	clone := RequestWithComment(request, "clone")
	if clone == request {
		t.Fatal("RequestWithComment returned the original request")
	}

	if got := requestCommentFromContext(request.Context()); got != "" {
		t.Fatalf("original request comment = %q", got)
	}

	if got := requestCommentFromContext(clone.Context()); got != "clone" {
		t.Fatalf("cloned request comment = %q", got)
	}

	if RequestWithComment(nil, "ignored") != nil {
		t.Fatal("RequestWithComment(nil) must return nil")
	}
}
