package hario

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/mgurevin/recorder"
)

const (
	defaultMaxBytes      = 256 << 20
	defaultMaxEntries    = 100_000
	defaultMaxEntryBytes = 16 << 20
)

var (
	// ErrInvalidHAR reports malformed JSON or an invalid HAR/entry structure.
	ErrInvalidHAR = errors.New("hario: invalid capture")
	// ErrLimitExceeded reports that a configured input bound was exceeded.
	ErrLimitExceeded = errors.New("hario: input limit exceeded")
)

// ReadConfig bounds untrusted capture input.
type ReadConfig struct {
	MaxBytes      int64
	MaxEntries    int
	MaxEntryBytes int64
}

// DefaultReadConfig returns bounded defaults suitable for local test fixtures.
func DefaultReadConfig() ReadConfig {
	return ReadConfig{
		MaxBytes:      defaultMaxBytes,
		MaxEntries:    defaultMaxEntries,
		MaxEntryBytes: defaultMaxEntryBytes,
	}
}

// ReadHAR reads one bounded HAR 1.2 JSON document. Unknown JSON fields are
// ignored so third-party HAR extensions remain accepted.
func ReadHAR(reader io.Reader, config ReadConfig) (*recorder.HAR, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidHAR)
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	limited := &io.LimitedReader{R: reader, N: config.MaxBytes + 1}
	decoder := json.NewDecoder(limited)

	var document recorder.HAR

	decodeErr := decoder.Decode(&document)

	if consumed(config.MaxBytes, limited) {
		return nil, fmt.Errorf("%w: HAR exceeds %d bytes", ErrLimitExceeded, config.MaxBytes)
	}

	if decodeErr != nil {
		return nil, fmt.Errorf("%w: decode HAR: %v", ErrInvalidHAR, decodeErr)
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: trailing JSON document", ErrInvalidHAR)
		}

		return nil, fmt.Errorf("%w: trailing content: %v", ErrInvalidHAR, err)
	}

	if consumed(config.MaxBytes, limited) {
		return nil, fmt.Errorf("%w: HAR exceeds %d bytes", ErrLimitExceeded, config.MaxBytes)
	}

	if document.Log != nil && len(document.Log.Entries) > config.MaxEntries {
		return nil, fmt.Errorf("%w: HAR contains more than %d entries", ErrLimitExceeded, config.MaxEntries)
	}

	if err := ValidateHAR(&document); err != nil {
		return nil, err
	}

	return &document, nil
}

// ReadNDJSON reads a bounded stream containing one recorder.Entry JSON object
// per physical line. Blank lines are ignored and the final line need not end
// with a newline.
func ReadNDJSON(reader io.Reader, config ReadConfig) ([]*recorder.Entry, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidHAR)
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	limited := &io.LimitedReader{R: reader, N: config.MaxBytes + 1}
	buffered := bufio.NewReader(limited)
	entries := make([]*recorder.Entry, 0)
	lineNumber := 0

	for {
		line, readErr := readBoundedLine(buffered, config.MaxEntryBytes)
		lineNumber++

		if consumed(config.MaxBytes, limited) {
			return nil, fmt.Errorf("%w: NDJSON exceeds %d bytes", ErrLimitExceeded, config.MaxBytes)
		}

		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("hario: read NDJSON line %d: %w", lineNumber, readErr)
		}

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			if len(entries) == 0 {
				trimmed = bytes.TrimPrefix(trimmed, []byte{0xef, 0xbb, 0xbf})
			}

			var entry recorder.Entry
			if err := json.Unmarshal(trimmed, &entry); err != nil {
				return nil, fmt.Errorf("%w: NDJSON line %d: %v", ErrInvalidHAR, lineNumber, err)
			}

			if len(entries) >= config.MaxEntries {
				return nil, fmt.Errorf("%w: NDJSON contains more than %d entries", ErrLimitExceeded, config.MaxEntries)
			}

			if err := validateEntry(&entry, fmt.Sprintf("entry %d at line %d", len(entries), lineNumber)); err != nil {
				return nil, err
			}

			entries = append(entries, &entry)
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	return entries, nil
}

// ValidateHAR validates the HAR container and every entry without checking
// whether captured representations are sufficient for fixture replay.
func ValidateHAR(document *recorder.HAR) error {
	if document == nil {
		return fmt.Errorf("%w: nil HAR document", ErrInvalidHAR)
	}

	if document.Log == nil {
		return fmt.Errorf("%w: log is required", ErrInvalidHAR)
	}

	if document.Log.Version != "1.2" {
		return fmt.Errorf("%w: log.version must be 1.2", ErrInvalidHAR)
	}

	if document.Log.Creator == nil || strings.TrimSpace(document.Log.Creator.Name) == "" {
		return fmt.Errorf("%w: log.creator.name is required", ErrInvalidHAR)
	}

	return ValidateEntries(document.Log.Entries)
}

// ValidateEntries validates format-independent HAR entry structures.
func ValidateEntries(entries []*recorder.Entry) error {
	for index, entry := range entries {
		if err := validateEntry(entry, fmt.Sprintf("entry %d", index)); err != nil {
			return err
		}
	}

	return nil
}

func validateConfig(config ReadConfig) error {
	if config.MaxBytes <= 0 || config.MaxEntries <= 0 || config.MaxEntryBytes <= 0 {
		return fmt.Errorf("%w: read limits must be positive", ErrInvalidHAR)
	}

	if config.MaxEntryBytes > config.MaxBytes {
		return fmt.Errorf("%w: MaxEntryBytes exceeds MaxBytes", ErrInvalidHAR)
	}

	return nil
}

func consumed(max int64, limited *io.LimitedReader) bool {
	return max+1-limited.N > max
}

func readBoundedLine(reader *bufio.Reader, max int64) ([]byte, error) {
	var line bytes.Buffer

	for {
		fragment, err := reader.ReadSlice('\n')
		if int64(line.Len()+len(fragment)) > max {
			return nil, fmt.Errorf("%w: NDJSON entry exceeds %d bytes", ErrLimitExceeded, max)
		}

		_, _ = line.Write(fragment)

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}

		return bytes.TrimSuffix(line.Bytes(), []byte{'\n'}), err
	}
}

func validateEntry(entry *recorder.Entry, path string) error {
	if entry == nil {
		return fmt.Errorf("%w: %s is nil", ErrInvalidHAR, path)
	}

	if _, err := time.Parse(time.RFC3339, entry.StartedDateTime); err != nil {
		return fmt.Errorf("%w: %s.startedDateTime is invalid", ErrInvalidHAR, path)
	}

	if entry.Time < 0 {
		return fmt.Errorf("%w: %s.time must not be negative", ErrInvalidHAR, path)
	}

	if entry.Request == nil {
		return fmt.Errorf("%w: %s.request is required", ErrInvalidHAR, path)
	}

	if strings.TrimSpace(entry.Request.Method) == "" {
		return fmt.Errorf("%w: %s.request.method is required", ErrInvalidHAR, path)
	}

	parsedURL, err := url.Parse(entry.Request.URL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("%w: %s.request.url must be absolute", ErrInvalidHAR, path)
	}

	if entry.Response == nil {
		return fmt.Errorf("%w: %s.response is required", ErrInvalidHAR, path)
	}

	if entry.Response.Status < 0 || entry.Response.Status > 999 {
		return fmt.Errorf("%w: %s.response.status is invalid", ErrInvalidHAR, path)
	}

	if entry.Response.Content == nil {
		return fmt.Errorf("%w: %s.response.content is required", ErrInvalidHAR, path)
	}

	if entry.Timings == nil {
		return fmt.Errorf("%w: %s.timings is required", ErrInvalidHAR, path)
	}

	timings := []struct {
		name  string
		value float64
	}{
		{"blocked", entry.Timings.Blocked},
		{"dns", entry.Timings.DNS},
		{"connect", entry.Timings.Connect},
		{"send", entry.Timings.Send},
		{"wait", entry.Timings.Wait},
		{"receive", entry.Timings.Receive},
		{"ssl", entry.Timings.SSL},
	}
	for _, timing := range timings {
		if timing.value < 0 && timing.value != -1 {
			return fmt.Errorf("%w: %s.timings.%s must be -1 or non-negative", ErrInvalidHAR, path, timing.name)
		}
	}

	if entry.Recorder != nil {
		if entry.Recorder.SchemaVersion != recorder.RecorderExtensionVersion {
			return fmt.Errorf("%w: %s._recorder.schemaVersion is unsupported", ErrInvalidHAR, path)
		}

		if err := validateBodyInfo(entry.Recorder.RequestBody, path+"._recorder.requestBody"); err != nil {
			return err
		}

		if err := validateBodyInfo(entry.Recorder.ResponseBody, path+"._recorder.responseBody"); err != nil {
			return err
		}
	}

	return nil
}

func validateBodyInfo(info *recorder.BodyInfo, path string) error {
	if info == nil {
		return nil
	}

	if info.CapturedBytes < 0 || info.TotalBytes < 0 || info.CapturedBytes > info.TotalBytes {
		return fmt.Errorf("%w: %s byte counters are inconsistent", ErrInvalidHAR, path)
	}

	if info.Complete && info.ClosedEarly {
		return fmt.Errorf("%w: %s cannot be complete and closed early", ErrInvalidHAR, path)
	}

	if info.Truncated && info.CapturedBytes >= info.TotalBytes {
		return fmt.Errorf("%w: %s truncated counters are inconsistent", ErrInvalidHAR, path)
	}

	return nil
}
