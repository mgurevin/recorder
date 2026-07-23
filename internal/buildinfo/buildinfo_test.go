package buildinfo

import "testing"

func TestInjectedVersionTakesPrecedence(t *testing.T) {
	previous := injectedVersion
	injectedVersion = "v1.2.3"

	t.Cleanup(func() {
		injectedVersion = previous
	})

	if got := Version(); got != "v1.2.3" {
		t.Fatalf("Version() = %q, want v1.2.3", got)
	}
}
