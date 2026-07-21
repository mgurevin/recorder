package recorder

import "log"

// reportInternalError applies the shared, fail-contained Transport and
// AsyncRecorder reporting policy.
func reportInternalError(mode InternalErrorMode, onError func(error), logf func(string, ...any), err error) {
	if err == nil {
		return
	}

	if mode == InternalErrorLog {
		if logf != nil {
			callSafely(func() { logf("recorder: %v", err) })
		} else {
			log.Printf("recorder: %v", err)
		}
	}

	if onError != nil {
		callSafely(func() { onError(err) })
	}
}
