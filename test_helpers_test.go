package recorder

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

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
