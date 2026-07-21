package recorder

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
)

func newAsyncRecorderForTest(sink Recorder, opts ...asyncConfigMutation) (*AsyncRecorder, error) {
	return NewAsyncRecorder(sink, asyncConfigWith(opts...))
}

func mustFileBodyStore(t *testing.T, dir string, opts ...fileBodyStoreConfigMutation) *FileBodyStore {
	t.Helper()

	store, err := NewFileBodyStore(dir, fileBodyStoreConfigWith(opts...))
	if err != nil {
		t.Fatalf("NewFileBodyStore: %v", err)
	}

	return store
}

func readBodyAsset(t *testing.T, store *FileBodyStore, ref string) []byte {
	t.Helper()

	r, err := store.Open(ref)
	if err != nil {
		t.Fatalf("open body asset: %v", err)
	}

	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("close body asset: %v", err)
		}
	}()

	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body asset: %v", err)
	}

	return b
}

// testWrite turns fixture/server write failures into immediate test failures.
// It deliberately panics so it is also safe to use from HTTP handler goroutines.
func testWrite(w io.Writer, p []byte) {
	if _, err := w.Write(p); err != nil {
		panic(fmt.Errorf("test write: %w", err))
	}
}

func testWriteString(w io.Writer, s string) {
	if _, err := io.WriteString(w, s); err != nil {
		panic(fmt.Errorf("test write string: %w", err))
	}
}

func testCopy(dst io.Writer, src io.Reader) {
	if _, err := io.Copy(dst, src); err != nil {
		panic(fmt.Errorf("test copy: %w", err))
	}
}

func testClose(c io.Closer) {
	if err := c.Close(); err != nil {
		panic(fmt.Errorf("test close: %w", err))
	}
}

func testServe(srv *http.Server, ln net.Listener) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(fmt.Errorf("test server: %w", err))
	}
}
