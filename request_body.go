package recorder

import "io"

// requestBodyRecorder tees the caller-supplied request body while the
// transport streams it. Read and Close semantics of the wrapped body are
// passed through unchanged; only the bytes the transport actually read are
// recorded.
type requestBodyRecorder struct {
	rc io.ReadCloser
	bc *bodyCapture
	ex *exchange
}

func (r *requestBodyRecorder) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.bc.observe(p[:n])
		r.ex.setState(StateRequestBodyStreaming)
	}

	switch {
	case err == io.EOF:
		r.bc.finishComplete()

	case err != nil:
		r.bc.fail(err)
	}

	return n, err
}

func (r *requestBodyRecorder) Close() error {
	err := r.rc.Close()
	r.bc.closed(err)

	return err
}
