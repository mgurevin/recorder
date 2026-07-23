package recorder

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type commitFailBodyWriter struct {
	aborted bool
}

func (w *commitFailBodyWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *commitFailBodyWriter) Commit() error               { return errors.New("commit failed") }
func (w *commitFailBodyWriter) Abort() error {
	w.aborted = true

	return nil
}
func (w *commitFailBodyWriter) Bytes() ([]byte, error) { return nil, nil }
func (w *commitFailBodyWriter) Ref() string            { return "" }

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func TestRedactingBodyWriterAbortsUnderlyingWriterWhenCommitFails(t *testing.T) {
	underlying := &commitFailBodyWriter{}
	buffered := bufio.NewWriter(underlying)
	w := &redactingBodyWriter{
		BodyWriter: underlying,
		redactor:   nopWriteCloser{Writer: buffered},
		buf:        buffered,
	}

	if err := w.Commit(); err == nil {
		t.Fatal("Commit succeeded")
	}

	if !underlying.aborted {
		t.Fatal("underlying writer was not aborted after failed commit")
	}
}

func TestBodyCaptureResetAbortsPreviousFile(t *testing.T) {
	store := mustFileBodyStore(t, t.TempDir())
	capture := newBodyCapture(context.Background(), store, BodyMetadata{}, "", true, 1024, "", false, nil, nil, nil)
	capture.observe([]byte("first attempt"))

	if stats := store.Stats(); stats.PartialFiles != 1 {
		t.Fatalf("before reset stats = %+v", stats)
	}

	capture.reset()

	if stats := store.Stats(); stats.PartialFiles != 0 || stats.AbortedTotal != 1 {
		t.Fatalf("after reset stats = %+v", stats)
	}

	capture.observe([]byte("final attempt"))
	capture.finishComplete()

	if stats := store.Stats(); stats.CommittedFiles != 1 || stats.PartialFiles != 0 {
		t.Fatalf("after final attempt stats = %+v", stats)
	}
}

func TestFileBodyStoreRecoversPartialsAndPreservesAssets(t *testing.T) {
	root := t.TempDir()
	partialDir := filepath.Join(root, "partial")
	assetDir := filepath.Join(root, "assets")

	if err := os.MkdirAll(partialDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(assetDir, 0o700); err != nil {
		t.Fatal(err)
	}

	partial := filepath.Join(partialDir, newID()+".partial")
	asset := filepath.Join(assetDir, newID()+".body")

	if err := os.WriteFile(partial, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(asset, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := mustFileBodyStore(t, root, withFileBodyPartialTTL(0))

	stats := store.Stats()
	if stats.RecoveredPartials != 1 || stats.PartialFiles != 0 || stats.CommittedFiles != 1 {
		t.Fatalf("recovery stats = %+v", stats)
	}

	if _, err := os.Stat(asset); err != nil {
		t.Fatalf("committed asset removed during recovery: %v", err)
	}
}

func TestFileBodyStoreMaintenanceModeHasNoStartupSideEffects(t *testing.T) {
	root := t.TempDir()
	partialDir := filepath.Join(root, "partial")
	assetDir := filepath.Join(root, "assets")

	if err := os.MkdirAll(partialDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}

	partial := filepath.Join(partialDir, newID()+".partial")
	if err := os.WriteFile(partial, []byte("in progress"), 0o600); err != nil {
		t.Fatal(err)
	}

	config := DefaultFileBodyStoreConfig()
	config.MaintenanceMode = true

	store, err := NewFileBodyStore(root, config)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("maintenance open removed partial: %v", err)
	}

	if mode := fileMode(t, partialDir).Perm(); mode != 0o755 {
		t.Fatalf("partial directory mode = %o", mode)
	}

	if stats := store.Stats(); stats.PartialFiles != 1 || stats.RecoveredPartials != 0 {
		t.Fatalf("maintenance stats = %+v", stats)
	}

	if _, err := store.NewWriter(context.Background(), BodyMetadata{}); err == nil {
		t.Fatal("maintenance store created a writer")
	}
}

func TestFileBodyStoreReconcileUsesAuthoritativeRefs(t *testing.T) {
	store := mustFileBodyStore(t, t.TempDir())

	refs := make([]string, 2)
	for i := range refs {
		w, err := store.NewWriter(context.Background(), BodyMetadata{})
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}

		if _, err := io.WriteString(w, "asset"); err != nil {
			t.Fatalf("Write: %v", err)
		}

		if err := w.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		refs[i] = w.Ref()
	}

	dryRun, err := store.Reconcile(refs[:1], 0, true)
	if err != nil {
		t.Fatalf("dry-run Reconcile: %v", err)
	}

	if dryRun.Scanned != 2 || dryRun.Released != 1 || store.Stats().CommittedFiles != 2 {
		t.Fatalf("dry-run result=%+v stats=%+v", dryRun, store.Stats())
	}

	result, err := store.Reconcile(refs[:1], 0, false)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if result.Released != 1 || result.ReleasedBytes != 5 || store.Stats().CommittedFiles != 1 {
		t.Fatalf("result=%+v stats=%+v", result, store.Stats())
	}

	if _, err := store.Open(refs[1]); err == nil {
		t.Fatal("reconciled asset remained readable")
	}
}

func TestFileBodyStoreRejectsInvalidReferencesAndConfiguration(t *testing.T) {
	for _, ref := range []string{"", "../secret", fileBodyRefPrefix + "xyz"} {
		store := mustFileBodyStore(t, t.TempDir())
		if _, err := store.Open(ref); err == nil {
			t.Errorf("Open(%q) succeeded", ref)
		}

		if err := store.Release(ref); err == nil {
			t.Errorf("Release(%q) succeeded", ref)
		}
	}

	for _, opts := range [][]fileBodyStoreConfigMutation{
		{withFileBodyMaxBytes(0)},
		{withFileBodyMaxFiles(0)},
		{withFileBodyPartialTTL(-time.Second)},
	} {
		if _, err := NewFileBodyStore(t.TempDir(), fileBodyStoreConfigWith(opts...)); err == nil {
			t.Fatal("NewFileBodyStore accepted invalid configuration")
		}
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.Mode()
}
