package recorder

import "testing"

func TestRecommendedConfigsLogInternalErrors(t *testing.T) {
	t.Parallel()

	if got := DefaultConfig().InternalErrorMode; got != InternalErrorLog {
		t.Fatalf("DefaultConfig InternalErrorMode = %d, want InternalErrorLog", got)
	}

	if got := DefaultAsyncRecorderConfig().InternalErrorMode; got != InternalErrorLog {
		t.Fatalf("DefaultAsyncRecorderConfig InternalErrorMode = %d, want InternalErrorLog", got)
	}
}

func TestZeroValueConfigsKeepInternalErrorsSilent(t *testing.T) {
	t.Parallel()

	if got := (Config{}).InternalErrorMode; got != InternalErrorIgnore {
		t.Fatalf("Config zero-value InternalErrorMode = %d, want InternalErrorIgnore", got)
	}

	if got := (AsyncRecorderConfig{}).InternalErrorMode; got != InternalErrorIgnore {
		t.Fatalf("AsyncRecorderConfig zero-value InternalErrorMode = %d, want InternalErrorIgnore", got)
	}
}
