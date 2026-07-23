package recorder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BodyMetadata describes the body stream a BodyWriter is created for.
type BodyMetadata struct {
	// ExchangeID correlates the body with its immutable entry.
	ExchangeID string
	// Direction is "request" or "response".
	Direction string
	// ContentType is the observed media type, including parameters.
	ContentType string
	// SizeHint is the expected number of bytes this writer will receive
	// (derived from Content-Length, already clamped to the capture limit),
	// or 0 when unknown. Stores may use it to pre-allocate, but must treat
	// it as untrusted: the actual stream may be shorter or longer, and a
	// hostile peer can announce an absurd Content-Length.
	SizeHint int64
}

// maxPreallocBytes caps how much memory a SizeHint may pre-allocate in one
// step. A lying Content-Length can therefore waste at most this much; larger
// bodies simply grow the buffer as bytes actually arrive.
const maxPreallocBytes = 4 << 20

// BodyWriter receives the captured representation of one body stream. When
// configured, the capture pipeline may decode and/or redact bytes before
// Write; it never requires a store to rewrite already-persisted content.
// Implementations must be safe for concurrent use of Write with Bytes.
type BodyWriter interface {
	io.Writer
	// Commit finalizes and publishes the captured representation. It is
	// idempotent; Ref must remain empty until Commit succeeds.
	Commit() error
	// Abort closes the writer and discards its captured representation. It is
	// idempotent and must not publish a Ref.
	Abort() error
	// Bytes returns the bytes captured so far (used when embedding content
	// into the HAR document).
	Bytes() ([]byte, error)
	// Ref returns an opaque external reference to the stored content, or ""
	// when the content lives inline in memory.
	Ref() string
}

// BodyStore creates BodyWriter instances. Implementations receive the
// capture pipeline's processed representation, while BodyInfo counters and
// hashes continue to describe the original caller/wire stream.
// Implementations must be safe for concurrent use.
type BodyStore interface {
	NewWriter(ctx context.Context, metadata BodyMetadata) (BodyWriter, error)
}

// entryAssetReleaser is an optional BodyStore capability for releasing all
// externally stored assets referenced by an entry that will not be retained.
type entryAssetReleaser interface {
	ReleaseEntryAssets(*Entry) error
}

// MemoryBodyStore keeps captured bodies in memory. It is the default store.
type MemoryBodyStore struct{}

// NewWriter implements BodyStore. A positive SizeHint pre-sizes the buffer
// (bounded by maxPreallocBytes) so growth re-copies are avoided for bodies
// with a truthful Content-Length; a wrong hint costs at most one bounded
// allocation and never breaks the capture.
func (MemoryBodyStore) NewWriter(_ context.Context, meta BodyMetadata) (BodyWriter, error) {
	w := &memoryBodyWriter{}

	if hint := meta.SizeHint; hint > 0 {
		if hint > maxPreallocBytes {
			hint = maxPreallocBytes
		}

		w.buf.Grow(int(hint))
	}

	return w, nil
}

type memoryBodyWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	committed bool
	aborted   bool
}

func (w *memoryBodyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.committed || w.aborted {
		return 0, os.ErrClosed
	}

	return w.buf.Write(p)
}

func (w *memoryBodyWriter) Commit() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.aborted {
		return errors.New("recorder: commit aborted memory body writer")
	}

	w.committed = true

	return nil
}

func (w *memoryBodyWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.committed || w.aborted {
		return nil
	}

	w.aborted = true
	w.buf.Reset()

	return nil
}

func (w *memoryBodyWriter) Bytes() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]byte(nil), w.buf.Bytes()...), nil
}

func (w *memoryBodyWriter) Ref() string { return "" }

const (
	defaultFileBodyMaxBytes   = int64(1 << 30)
	defaultFileBodyMaxFiles   = 10_000
	defaultFileBodyPartialTTL = time.Hour
	fileBodyRefPrefix         = "filebody:v1:"
)

// FileBodyStoreConfig defines managed disk capacity and commit durability.
type FileBodyStoreConfig struct {
	// MaxBytes bounds committed and partial bytes owned by the store.
	MaxBytes int64
	// MaxFiles bounds committed and partial files owned by the store.
	MaxFiles int
	// PartialTTL controls startup cleanup of abandoned partial files. Zero
	// removes every partial during startup; negative values are invalid.
	PartialTTL time.Duration
	// SyncOnCommit fsyncs the file and containing directory during publish. It
	// does not make the separately recorded Entry crash-durable.
	SyncOnCommit bool
	// MaintenanceMode opens an existing store without creating directories,
	// changing permissions, or recovering partial files. NewWriter is disabled;
	// Open, Release, Reconcile, and Stats remain available. This makes dry-run
	// reconciliation observational while preserving the normal constructor's
	// path and reference validation.
	MaintenanceMode bool
}

// DefaultFileBodyStoreConfig returns bounded managed-store defaults.
func DefaultFileBodyStoreConfig() FileBodyStoreConfig {
	return FileBodyStoreConfig{MaxBytes: defaultFileBodyMaxBytes, MaxFiles: defaultFileBodyMaxFiles, PartialTTL: defaultFileBodyPartialTTL}
}

// FileBodyStoreStats is an atomic point-in-time lifecycle and capacity
// snapshot. Totals are process-lifetime counters except RecoveredPartials,
// which includes startup recovery.
type FileBodyStoreStats struct {
	MaxBytes          int64
	MaxFiles          int
	PartialBytes      int64
	CommittedBytes    int64
	PartialFiles      int
	CommittedFiles    int
	CommittedTotal    uint64
	AbortedTotal      uint64
	ReleasedTotal     uint64
	QuotaRejected     uint64
	RecoveredPartials uint64
	WriteFailures     uint64
	CommitFailures    uint64
	AbortFailures     uint64
	ReleaseFailures   uint64
}

// ReconcileResult reports an authoritative live-reference reconciliation.
type ReconcileResult struct {
	Scanned       int
	Released      int
	ReleasedBytes int64
}

// FileBodyStore spools processed body representations into a bounded managed
// directory. Writers use partial files and publish opaque references only
// after an atomic commit. Committed assets remain until explicitly released or
// removed by an authoritative reconciliation. A root must be owned by one
// process; FileBodyStore does not provide cross-process locking.
type FileBodyStore struct {
	mu           sync.Mutex
	root         string
	partialDir   string
	assetDir     string
	maxBytes     int64
	maxFiles     int
	partialTTL   time.Duration
	syncOnCommit bool
	maintenance  bool
	stats        FileBodyStoreStats
}

// NewFileBodyStore opens a managed body store rooted at dir. Config must
// specify positive byte and file limits; DefaultFileBodyStoreConfig supplies
// the bounded baseline of 1 GiB, 10,000 files, and a one-hour partial TTL.
func NewFileBodyStore(dir string, config FileBodyStoreConfig) (*FileBodyStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("recorder: file body store directory is required")
	}

	if config.MaxBytes <= 0 || config.MaxFiles <= 0 || config.PartialTTL < 0 {
		return nil, errors.New("recorder: invalid file body store limits")
	}

	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("recorder: resolve body store directory: %w", err)
	}

	s := &FileBodyStore{
		root:         root,
		partialDir:   filepath.Join(root, "partial"),
		assetDir:     filepath.Join(root, "assets"),
		maxBytes:     config.MaxBytes,
		maxFiles:     config.MaxFiles,
		partialTTL:   config.PartialTTL,
		syncOnCommit: config.SyncOnCommit,
		maintenance:  config.MaintenanceMode,
	}
	if err := s.initialize(); err != nil {
		return nil, err
	}

	return s, nil
}

// NewWriter implements BodyStore.
func (s *FileBodyStore) NewWriter(_ context.Context, _ BodyMetadata) (BodyWriter, error) {
	if s == nil {
		return nil, errors.New("recorder: nil file body store")
	}

	if s.maintenance {
		return nil, errors.New("recorder: file body store is open in maintenance mode")
	}

	s.mu.Lock()
	if s.stats.PartialFiles+s.stats.CommittedFiles >= s.maxFiles {
		s.stats.QuotaRejected++
		s.mu.Unlock()

		return nil, errors.New("recorder: file body store file quota exceeded")
	}

	s.stats.PartialFiles++
	s.mu.Unlock()

	id := newID()
	partialPath := filepath.Join(s.partialDir, id+".partial")
	finalPath := filepath.Join(s.assetDir, id+".body")

	f, err := os.OpenFile(partialPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.mu.Lock()
		s.stats.PartialFiles--
		s.mu.Unlock()

		return nil, fmt.Errorf("recorder: create body file: %w", err)
	}

	return &fileBodyWriter{
		store:       s,
		f:           f,
		partialPath: partialPath,
		finalPath:   finalPath,
		ref:         fileBodyRefPrefix + id,
	}, nil
}

type fileBodyWriter struct {
	mu          sync.Mutex
	store       *FileBodyStore
	f           *os.File
	partialPath string
	finalPath   string
	ref         string
	written     int64
	committed   bool
	aborted     bool
}

func (w *fileBodyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.committed || w.aborted {
		return 0, os.ErrClosed
	}

	if err := w.store.reserveBytes(int64(len(p))); err != nil {
		return 0, err
	}

	n, err := w.f.Write(p)

	w.written += int64(n)
	if n < len(p) {
		w.store.releasePartialBytes(int64(len(p) - n))

		if err == nil {
			err = io.ErrShortWrite
		}
	}

	if err != nil {
		w.store.recordWriteFailure()
	}

	return n, err
}

func (w *fileBodyWriter) Commit() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.committed {
		return nil
	}

	if w.aborted {
		return errors.New("recorder: commit aborted file body writer")
	}

	if w.store.syncOnCommit {
		if err := w.f.Sync(); err != nil {
			w.store.recordCommitFailure()
			_ = w.abortLocked()

			return fmt.Errorf("recorder: sync body file: %w", err)
		}
	}

	if err := w.f.Close(); err != nil {
		w.store.recordCommitFailure()
		_ = w.abortLocked()

		return fmt.Errorf("recorder: close body file: %w", err)
	}

	if err := os.Rename(w.partialPath, w.finalPath); err != nil {
		w.store.recordCommitFailure()
		_ = w.abortLocked()

		return fmt.Errorf("recorder: publish body file: %w", err)
	}

	if w.store.syncOnCommit {
		if err := syncDirectory(w.store.assetDir); err != nil {
			w.store.recordCommitFailure()
			_ = os.Remove(w.finalPath)
			_ = w.abortLocked()

			return fmt.Errorf("recorder: sync body asset directory: %w", err)
		}
	}

	w.committed = true
	w.store.commitFile(w.written)

	return nil
}

func (w *fileBodyWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.abortLocked()
}

func (w *fileBodyWriter) abortLocked() error {
	if w.aborted || w.committed {
		return nil
	}

	w.aborted = true
	closeErr := w.f.Close()

	removeErr := os.Remove(w.partialPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}

	w.store.abortFile(w.written)

	if closeErr != nil {
		w.store.recordAbortFailure()

		return closeErr
	}

	if removeErr != nil {
		w.store.recordAbortFailure()
	}

	return removeErr
}

func (w *fileBodyWriter) Bytes() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	path := w.partialPath
	if w.committed {
		path = w.finalPath
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("recorder: read body file: %w", err)
	}

	return b, nil
}

func (w *fileBodyWriter) Ref() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.committed {
		return ""
	}

	return w.ref
}

func (s *FileBodyStore) initialize() error {
	if s.maintenance {
		for _, dir := range []string{s.root, s.partialDir, s.assetDir} {
			info, err := os.Lstat(dir)
			if err != nil {
				return fmt.Errorf("recorder: open body store for maintenance: %w", err)
			}

			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return errors.New("recorder: maintenance body store paths must be directories without symbolic links")
			}
		}
	} else {
		if err := s.initializeDirectories(); err != nil {
			return err
		}
	}

	return s.scanExisting()
}

func (s *FileBodyStore) initializeDirectories() error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("recorder: create body store directory: %w", err)
	}

	if err := os.MkdirAll(s.partialDir, 0o700); err != nil {
		return fmt.Errorf("recorder: create body partial directory: %w", err)
	}

	if err := os.MkdirAll(s.assetDir, 0o700); err != nil {
		return fmt.Errorf("recorder: create body asset directory: %w", err)
	}

	for _, dir := range []string{s.root, s.partialDir, s.assetDir} {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("recorder: secure body store directory: %w", err)
		}
	}

	return nil
}

func (s *FileBodyStore) scanExisting() error {
	now := time.Now()

	partials, err := os.ReadDir(s.partialDir)
	if err != nil {
		return fmt.Errorf("recorder: scan body partial directory: %w", err)
	}

	for _, entry := range partials {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".partial") {
			continue
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("recorder: symlink found in body partial directory")
		}

		path := filepath.Join(s.partialDir, entry.Name())

		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("recorder: stat partial body asset: %w", infoErr)
		}

		if !s.maintenance && (s.partialTTL == 0 || now.Sub(info.ModTime()) >= s.partialTTL) {
			if removeErr := os.Remove(path); removeErr != nil {
				return fmt.Errorf("recorder: recover partial body asset: %w", removeErr)
			}

			s.stats.RecoveredPartials++

			continue
		}

		s.stats.PartialFiles++
		s.stats.PartialBytes += info.Size()
	}

	assets, err := os.ReadDir(s.assetDir)
	if err != nil {
		return fmt.Errorf("recorder: scan body asset directory: %w", err)
	}

	for _, entry := range assets {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".body") {
			continue
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("recorder: symlink found in body asset directory")
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("recorder: stat body asset: %w", infoErr)
		}

		s.stats.CommittedFiles++
		s.stats.CommittedBytes += info.Size()
	}

	if s.stats.PartialBytes+s.stats.CommittedBytes > s.maxBytes ||
		s.stats.PartialFiles+s.stats.CommittedFiles > s.maxFiles {
		return errors.New("recorder: existing body assets exceed configured quota")
	}

	return nil
}

func (s *FileBodyStore) reserveBytes(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stats.PartialBytes+s.stats.CommittedBytes+n > s.maxBytes {
		s.stats.QuotaRejected++

		return errors.New("recorder: file body store byte quota exceeded")
	}

	s.stats.PartialBytes += n

	return nil
}

func (s *FileBodyStore) releasePartialBytes(n int64) {
	s.mu.Lock()
	s.stats.PartialBytes -= n
	s.mu.Unlock()
}

func (s *FileBodyStore) commitFile(size int64) {
	s.mu.Lock()
	s.stats.PartialFiles--
	s.stats.PartialBytes -= size
	s.stats.CommittedFiles++
	s.stats.CommittedBytes += size
	s.stats.CommittedTotal++
	s.mu.Unlock()
}

func (s *FileBodyStore) abortFile(size int64) {
	s.mu.Lock()
	s.stats.PartialFiles--
	s.stats.PartialBytes -= size
	s.stats.AbortedTotal++
	s.mu.Unlock()
}

func (s *FileBodyStore) recordWriteFailure() {
	s.mu.Lock()
	s.stats.WriteFailures++
	s.mu.Unlock()
}

func (s *FileBodyStore) recordCommitFailure() {
	s.mu.Lock()
	s.stats.CommitFailures++
	s.mu.Unlock()
}

func (s *FileBodyStore) recordAbortFailure() {
	s.mu.Lock()
	s.stats.AbortFailures++
	s.mu.Unlock()
}

// Stats returns an atomic point-in-time lifecycle and capacity snapshot.
func (s *FileBodyStore) Stats() FileBodyStoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := s.stats
	stats.MaxBytes = s.maxBytes
	stats.MaxFiles = s.maxFiles

	return stats
}

// Open opens a committed opaque body reference for reading.
func (s *FileBodyStore) Open(ref string) (io.ReadCloser, error) {
	path, err := s.resolveRef(ref)
	if err != nil {
		return nil, err
	}

	if _, err := regularFileInfo(path); err != nil {
		return nil, fmt.Errorf("recorder: validate body asset: %w", err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("recorder: open body asset: %w", err)
	}

	return f, nil
}

// Release removes one committed body asset. Missing assets are treated as an
// idempotent success.
func (s *FileBodyStore) Release(ref string) error {
	path, err := s.resolveRef(ref)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	info, statErr := regularFileInfo(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil
	}

	if statErr != nil {
		s.stats.ReleaseFailures++

		return fmt.Errorf("recorder: stat body asset for release: %w", statErr)
	}

	if err := os.Remove(path); err != nil {
		s.stats.ReleaseFailures++

		return fmt.Errorf("recorder: release body asset: %w", err)
	}

	s.stats.CommittedFiles--
	s.stats.CommittedBytes -= info.Size()
	s.stats.ReleasedTotal++

	return nil
}

// ReleaseEntryAssets releases the request and response body references owned
// by entry. Empty and duplicate references are ignored.
func (s *FileBodyStore) ReleaseEntryAssets(entry *Entry) error {
	if entry == nil || entry.Recorder == nil {
		return nil
	}

	refs := make(map[string]struct{}, 2)
	if entry.Recorder.RequestBody != nil && entry.Recorder.RequestBody.Store != "" {
		refs[entry.Recorder.RequestBody.Store] = struct{}{}
	}

	if entry.Recorder.ResponseBody != nil && entry.Recorder.ResponseBody.Store != "" {
		refs[entry.Recorder.ResponseBody.Store] = struct{}{}
	}

	var errs []error

	for ref := range refs {
		if err := s.Release(ref); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Reconcile removes committed assets absent from the authoritative liveRefs
// set and older than grace. Dry-run reports without deleting.
func (s *FileBodyStore) Reconcile(liveRefs []string, grace time.Duration, dryRun bool) (ReconcileResult, error) {
	if grace < 0 {
		return ReconcileResult{}, errors.New("recorder: reconcile grace must not be negative")
	}

	live := make(map[string]struct{}, len(liveRefs))
	for _, ref := range liveRefs {
		if _, err := s.resolveRef(ref); err != nil {
			return ReconcileResult{}, err
		}

		live[ref] = struct{}{}
	}

	entries, err := os.ReadDir(s.assetDir)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("recorder: scan body assets for reconciliation: %w", err)
	}

	var result ReconcileResult

	now := time.Now()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".body") {
			continue
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return result, errors.New("recorder: symlink found in body asset directory")
		}

		result.Scanned++
		id := strings.TrimSuffix(entry.Name(), ".body")

		ref := fileBodyRefPrefix + id
		if _, ok := live[ref]; ok {
			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return result, fmt.Errorf("recorder: stat body asset for reconciliation: %w", infoErr)
		}

		if now.Sub(info.ModTime()) < grace {
			continue
		}

		result.Released++
		result.ReleasedBytes += info.Size()

		if !dryRun {
			if releaseErr := s.Release(ref); releaseErr != nil {
				return result, releaseErr
			}
		}
	}

	return result, nil
}

func (s *FileBodyStore) resolveRef(ref string) (string, error) {
	if !strings.HasPrefix(ref, fileBodyRefPrefix) {
		return "", errors.New("recorder: invalid file body reference")
	}

	id := strings.TrimPrefix(ref, fileBodyRefPrefix)
	if len(id) != 32 {
		return "", errors.New("recorder: invalid file body reference")
	}

	for _, char := range id {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return "", errors.New("recorder: invalid file body reference")
		}
	}

	return filepath.Join(s.assetDir, id+".body"), nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}

	syncErr := dir.Sync()
	closeErr := dir.Close()

	if syncErr != nil {
		return syncErr
	}

	return closeErr
}

func regularFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("body asset is not a regular file")
	}

	return info, nil
}
