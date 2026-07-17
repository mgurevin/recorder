package recorder

import "io"

// responseBodyRecorder wraps resp.Body. It records exactly the bytes the
// caller reads (no double read, no extra buffering beyond the capture
// writer), passes bytes, EOF and errors through unchanged, and finalizes the
// exchange record when the stream ends:
//
//   - Read returning io.EOF     -> record complete
//   - Read returning an error   -> record failed (phase read_response_body)
//   - Close before EOF          -> record closed early
//
// The finalize path runs at most once (guarded by the exchange), so a Close
// following EOF only adds the Close error, if any.
type responseBodyRecorder struct {
	rc io.ReadCloser
	bc *bodyCapture
	ex *exchange
}

func (r *responseBodyRecorder) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.bc.observe(p[:n])
		r.ex.setState(StateResponseBodyStreaming)
	}
	switch {
	case err == io.EOF:
		r.bc.finishComplete()
		r.ex.finalizeComplete()
	case err != nil:
		r.bc.fail(err)
		r.ex.finalizeBodyReadError(err)
	}
	return n, err
}

func (r *responseBodyRecorder) Close() error {
	err := r.rc.Close()
	r.bc.closed(err)
	r.ex.finalizeClosed()
	return err
}
