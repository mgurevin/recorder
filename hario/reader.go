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
	stream, err := NewHARStream(reader, config)
	if err != nil {
		return nil, err
	}

	entries := make([]*recorder.Entry, 0)

	for {
		entry, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return nil, nextErr
		}

		entries = append(entries, entry)
	}

	document := stream.document
	document.Log.Entries = entries

	return document, nil
}

// ReadNDJSON reads a bounded stream containing one recorder.Entry JSON object
// per physical line. Blank lines are ignored and the final line need not end
// with a newline.
func ReadNDJSON(reader io.Reader, config ReadConfig) ([]*recorder.Entry, error) {
	stream, err := NewNDJSONStream(reader, config)
	if err != nil {
		return nil, err
	}

	entries := make([]*recorder.Entry, 0)

	for {
		entry, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return nil, nextErr
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

type streamFormat uint8

const (
	streamFormatHAR streamFormat = iota
	streamFormatNDJSON
)

// EntryStream incrementally decodes validated recorder entries. Next returns
// io.EOF after the complete input, including trailing HAR metadata, has been
// validated. EntryStream does not close or otherwise own its input reader.
type EntryStream struct {
	format      streamFormat
	config      ReadConfig
	limited     *io.LimitedReader
	decoder     *json.Decoder
	buffered    *bufio.Reader
	document    *recorder.HAR
	entryCount  int
	lineNumber  int
	harLogSeen  bool
	done        bool
	terminalErr error
}

// NewHARStream prepares a bounded pull stream over one HAR 1.2 document.
func NewHARStream(reader io.Reader, config ReadConfig) (*EntryStream, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidHAR)
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	limited := &io.LimitedReader{R: reader, N: config.MaxBytes + 1}
	stream := &EntryStream{
		format:   streamFormatHAR,
		config:   config,
		limited:  limited,
		decoder:  json.NewDecoder(limited),
		document: &recorder.HAR{Log: &recorder.Log{}},
	}

	if err := stream.openHAR(); err != nil {
		return nil, stream.harError(err)
	}

	if consumed(config.MaxBytes, limited) {
		return nil, fmt.Errorf("%w: HAR exceeds %d bytes", ErrLimitExceeded, config.MaxBytes)
	}

	return stream, nil
}

// NewNDJSONStream prepares a bounded pull stream over recorder.Entry objects,
// one per physical line.
func NewNDJSONStream(reader io.Reader, config ReadConfig) (*EntryStream, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidHAR)
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	limited := &io.LimitedReader{R: reader, N: config.MaxBytes + 1}

	return &EntryStream{
		format:   streamFormatNDJSON,
		config:   config,
		limited:  limited,
		buffered: bufio.NewReader(limited),
	}, nil
}

// Next returns the next validated entry or io.EOF after complete input
// validation. After any error, subsequent calls return the same terminal error.
func (s *EntryStream) Next() (*recorder.Entry, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil entry stream", ErrInvalidHAR)
	}

	if s.done {
		if s.terminalErr != nil {
			return nil, s.terminalErr
		}

		return nil, io.EOF
	}

	var (
		entry *recorder.Entry
		err   error
	)

	switch s.format {
	case streamFormatHAR:
		entry, err = s.nextHAR()

	case streamFormatNDJSON:
		entry, err = s.nextNDJSON()
	}

	if err != nil {
		s.done = true
		if !errors.Is(err, io.EOF) {
			s.terminalErr = err
		}
	}

	return entry, err
}

func (s *EntryStream) nextNDJSON() (*recorder.Entry, error) {
	for {
		line, readErr := readBoundedLine(s.buffered, s.config.MaxEntryBytes)
		s.lineNumber++

		if consumed(s.config.MaxBytes, s.limited) {
			return nil, fmt.Errorf("%w: NDJSON exceeds %d bytes", ErrLimitExceeded, s.config.MaxBytes)
		}

		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("hario: read NDJSON line %d: %w", s.lineNumber, readErr)
		}

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if errors.Is(readErr, io.EOF) {
				return nil, io.EOF
			}

			continue
		}

		if s.entryCount == 0 {
			trimmed = bytes.TrimPrefix(trimmed, []byte{0xef, 0xbb, 0xbf})
		}

		if s.entryCount >= s.config.MaxEntries {
			return nil, fmt.Errorf("%w: NDJSON contains more than %d entries", ErrLimitExceeded, s.config.MaxEntries)
		}

		var entry recorder.Entry
		if err := json.Unmarshal(trimmed, &entry); err != nil {
			return nil, fmt.Errorf("%w: NDJSON line %d: %v", ErrInvalidHAR, s.lineNumber, err)
		}

		if err := validateEntry(
			&entry,
			fmt.Sprintf("entry %d at line %d", s.entryCount, s.lineNumber),
		); err != nil {
			return nil, err
		}

		s.entryCount++

		return &entry, nil
	}
}

func (s *EntryStream) openHAR() error {
	if err := expectDelimiter(s.decoder, '{', "HAR document"); err != nil {
		return err
	}

	for s.decoder.More() {
		name, err := decodeFieldName(s.decoder, "HAR document")
		if err != nil {
			return err
		}

		if name != "log" {
			if err := skipJSONValue(s.decoder); err != nil {
				return err
			}

			continue
		}

		if s.harLogSeen {
			return fmt.Errorf("%w: duplicate log field", ErrInvalidHAR)
		}

		s.harLogSeen = true

		if err := expectDelimiter(s.decoder, '{', "log"); err != nil {
			return err
		}

		return s.openHAREntries()
	}

	return fmt.Errorf("%w: log is required", ErrInvalidHAR)
}

func (s *EntryStream) openHAREntries() error {
	for s.decoder.More() {
		name, err := decodeFieldName(s.decoder, "log")
		if err != nil {
			return err
		}

		if name == "entries" {
			return expectDelimiter(s.decoder, '[', "log.entries")
		}

		if err := s.decodeHARLogField(name); err != nil {
			return err
		}
	}

	return fmt.Errorf("%w: log.entries is required", ErrInvalidHAR)
}

func (s *EntryStream) nextHAR() (*recorder.Entry, error) {
	if !s.decoder.More() {
		if err := s.finishHAR(); err != nil {
			return nil, s.harError(err)
		}

		return nil, io.EOF
	}

	if s.entryCount >= s.config.MaxEntries {
		return nil, fmt.Errorf("%w: HAR contains more than %d entries", ErrLimitExceeded, s.config.MaxEntries)
	}

	var encoded json.RawMessage
	if err := s.decoder.Decode(&encoded); err != nil {
		return nil, s.harError(fmt.Errorf("%w: decode HAR entry %d: %v", ErrInvalidHAR, s.entryCount, err))
	}

	if consumed(s.config.MaxBytes, s.limited) {
		return nil, fmt.Errorf("%w: HAR exceeds %d bytes", ErrLimitExceeded, s.config.MaxBytes)
	}

	if int64(len(encoded)) > s.config.MaxEntryBytes {
		return nil, fmt.Errorf("%w: HAR entry %d exceeds %d bytes", ErrLimitExceeded, s.entryCount, s.config.MaxEntryBytes)
	}

	var entry recorder.Entry
	if err := json.Unmarshal(encoded, &entry); err != nil {
		return nil, fmt.Errorf("%w: decode HAR entry %d: %v", ErrInvalidHAR, s.entryCount, err)
	}

	if err := validateEntry(&entry, fmt.Sprintf("entry %d", s.entryCount)); err != nil {
		return nil, err
	}

	s.entryCount++

	return &entry, nil
}

func (s *EntryStream) finishHAR() error {
	if err := expectDelimiter(s.decoder, ']', "log.entries"); err != nil {
		return err
	}

	for s.decoder.More() {
		name, err := decodeFieldName(s.decoder, "log")
		if err != nil {
			return err
		}

		if name == "entries" {
			return fmt.Errorf("%w: duplicate log.entries field", ErrInvalidHAR)
		}

		if err := s.decodeHARLogField(name); err != nil {
			return err
		}
	}

	if err := expectDelimiter(s.decoder, '}', "log"); err != nil {
		return err
	}

	for s.decoder.More() {
		name, err := decodeFieldName(s.decoder, "HAR document")
		if err != nil {
			return err
		}

		if name == "log" {
			return fmt.Errorf("%w: duplicate log field", ErrInvalidHAR)
		}

		if err := skipJSONValue(s.decoder); err != nil {
			return err
		}
	}

	if err := expectDelimiter(s.decoder, '}', "HAR document"); err != nil {
		return err
	}

	var trailing json.RawMessage
	if err := s.decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: trailing JSON document", ErrInvalidHAR)
		}

		return fmt.Errorf("%w: trailing content: %v", ErrInvalidHAR, err)
	}

	if consumed(s.config.MaxBytes, s.limited) {
		return fmt.Errorf("%w: HAR exceeds %d bytes", ErrLimitExceeded, s.config.MaxBytes)
	}

	return validateHARMetadata(s.document)
}

func (s *EntryStream) decodeHARLogField(name string) error {
	switch name {
	case "version":
		if err := s.decoder.Decode(&s.document.Log.Version); err != nil {
			return fmt.Errorf("%w: decode log.version: %v", ErrInvalidHAR, err)
		}

	case "creator":
		if err := s.decoder.Decode(&s.document.Log.Creator); err != nil {
			return fmt.Errorf("%w: decode log.creator: %v", ErrInvalidHAR, err)
		}

	case "comment":
		if err := s.decoder.Decode(&s.document.Log.Comment); err != nil {
			return fmt.Errorf("%w: decode log.comment: %v", ErrInvalidHAR, err)
		}

	default:
		return skipJSONValue(s.decoder)
	}

	return nil
}

func (s *EntryStream) harError(err error) error {
	if consumed(s.config.MaxBytes, s.limited) {
		return fmt.Errorf("%w: HAR exceeds %d bytes", ErrLimitExceeded, s.config.MaxBytes)
	}

	return err
}

func decodeFieldName(decoder *json.Decoder, path string) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", fmt.Errorf("%w: decode %s field: %v", ErrInvalidHAR, path, err)
	}

	name, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s field name is invalid", ErrInvalidHAR, path)
	}

	return name, nil
}

func expectDelimiter(decoder *json.Decoder, want json.Delim, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: decode %s: %v", ErrInvalidHAR, path, err)
	}

	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != want {
		return fmt.Errorf("%w: %s must use %q", ErrInvalidHAR, path, want)
	}

	return nil
}

func skipJSONValue(decoder *json.Decoder) error {
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%w: decode extension: %v", ErrInvalidHAR, err)
	}

	return nil
}

func validateHARMetadata(document *recorder.HAR) error {
	if document == nil || document.Log == nil {
		return fmt.Errorf("%w: log is required", ErrInvalidHAR)
	}

	if document.Log.Version != "1.2" {
		return fmt.Errorf("%w: log.version must be 1.2", ErrInvalidHAR)
	}

	if document.Log.Creator == nil || strings.TrimSpace(document.Log.Creator.Name) == "" {
		return fmt.Errorf("%w: log.creator.name is required", ErrInvalidHAR)
	}

	return nil
}

// ValidateHAR validates the HAR container and every entry without checking
// whether captured representations are sufficient for fixture replay.
func ValidateHAR(document *recorder.HAR) error {
	if document == nil {
		return fmt.Errorf("%w: nil HAR document", ErrInvalidHAR)
	}

	if err := validateHARMetadata(document); err != nil {
		return err
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
