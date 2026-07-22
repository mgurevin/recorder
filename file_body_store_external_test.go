package recorder_test

import (
	"context"
	"io"
	"testing"

	recorder "github.com/mgurevin/recorder"
)

func TestFileBodyWriterCommitPublishAndRelease(t *testing.T) {
	store := newFileBodyStore(t, recorder.DefaultFileBodyStoreConfig())

	writer, err := store.NewWriter(context.Background(), recorder.BodyMetadata{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if ref := writer.Ref(); ref != "" {
		t.Fatalf("Ref before Commit = %q", ref)
	}

	if _, err := io.WriteString(writer, "captured"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := writer.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := writer.Commit(); err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	ref := writer.Ref()
	if ref == "" || string(readBodyAsset(t, store, ref)) != "captured" {
		t.Fatalf("published ref = %q", ref)
	}

	stats := store.Stats()
	if stats.PartialFiles != 0 || stats.CommittedFiles != 1 || stats.CommittedBytes != 8 || stats.CommittedTotal != 1 {
		t.Fatalf("stats after commit = %+v", stats)
	}

	if err := store.Release(ref); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := store.Release(ref); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	stats = store.Stats()
	if stats.CommittedFiles != 0 || stats.CommittedBytes != 0 || stats.ReleasedTotal != 1 {
		t.Fatalf("stats after release = %+v", stats)
	}
}

func TestFileBodyWriterAbortReleasesQuota(t *testing.T) {
	config := recorder.DefaultFileBodyStoreConfig()
	config.MaxBytes = 4
	config.MaxFiles = 1
	store := newFileBodyStore(t, config)

	writer, err := store.NewWriter(context.Background(), recorder.BodyMetadata{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if _, err := io.WriteString(writer, "four"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := io.WriteString(writer, "x"); err == nil {
		t.Fatal("write beyond byte quota succeeded")
	}

	if err := writer.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	if err := writer.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}

	stats := store.Stats()
	if stats.PartialFiles != 0 || stats.PartialBytes != 0 || stats.AbortedTotal != 1 || stats.QuotaRejected != 1 {
		t.Fatalf("stats after abort = %+v", stats)
	}

	if _, err := store.NewWriter(context.Background(), recorder.BodyMetadata{}); err != nil {
		t.Fatalf("quota not reusable after Abort: %v", err)
	}
}

func newFileBodyStore(t *testing.T, config recorder.FileBodyStoreConfig) *recorder.FileBodyStore {
	t.Helper()

	store, err := recorder.NewFileBodyStore(t.TempDir(), config)
	if err != nil {
		t.Fatalf("NewFileBodyStore: %v", err)
	}

	return store
}

func readBodyAsset(t *testing.T, store *recorder.FileBodyStore, ref string) []byte {
	t.Helper()

	asset, err := store.Open(ref)
	if err != nil {
		t.Fatalf("open body asset: %v", err)
	}

	defer func() { _ = asset.Close() }()

	data, err := io.ReadAll(asset)
	if err != nil {
		t.Fatalf("read body asset: %v", err)
	}

	return data
}
