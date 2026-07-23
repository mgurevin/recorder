package hartest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
)

const (
	defaultMaxRequestBodyBytes  = 4 << 20
	defaultMaxResponseBodyBytes = 32 << 20
	redactedValue               = "[REDACTED]"
)

var protectedTokenPattern = regexp.MustCompile(`REC-(?:ENC|TOK)-v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// BodyMatchMode controls whether captured request bodies select fixtures.
type BodyMatchMode string

const (
	// BodyMatchAuto compares bodies whenever either side has one.
	BodyMatchAuto BodyMatchMode = "auto"
	// BodyMatchExact always compares complete captured body representations.
	BodyMatchExact BodyMatchMode = "exact"
	// BodyMatchIgnore explicitly excludes request bodies from matching.
	BodyMatchIgnore BodyMatchMode = "ignore"
)

// MatchConfig defines deterministic request matching.
type MatchConfig struct {
	// Headers names the request headers that participate in matching.
	Headers []string
	// Body controls request-body matching.
	Body BodyMatchMode
	// MaxRequestBodyBytes bounds the live request body read before matching.
	MaxRequestBodyBytes int64
	// Normalize canonicalizes isolated live and captured request snapshots.
	Normalize RequestNormalizer
}

// RequestSnapshot is an isolated request representation supplied to a
// RequestNormalizer. Mutating it never changes the live request or source
// fixture.
type RequestSnapshot struct {
	Method  string
	URL     *url.URL
	Headers http.Header
	// Trailers contains request trailers observed before matching.
	Trailers http.Header
	// Body contains the complete bounded request representation.
	Body []byte
}

// RequestNormalizer canonicalizes one isolated request representation before
// deterministic matching. The same function receives the live request and
// fixture snapshots independently.
type RequestNormalizer func(*RequestSnapshot) error

// BodyOpener resolves opaque external BodyStore references. FileBodyStore
// implements this interface.
type BodyOpener interface {
	Open(ref string) (io.ReadCloser, error)
}

// BodyOpenerFunc adapts a function to BodyOpener.
type BodyOpenerFunc func(string) (io.ReadCloser, error)

// Open implements BodyOpener.
func (f BodyOpenerFunc) Open(ref string) (io.ReadCloser, error) { return f(ref) }

// ProtectedValueResolver resolves one complete REC-ENC-v1 or REC-TOK-v1 token.
// resolved=false leaves the protected representation unchanged.
type ProtectedValueResolver interface {
	ResolveProtectedValue(token string) (plaintext []byte, resolved bool, err error)
}

// ProtectedValueResolverFunc adapts a function to ProtectedValueResolver.
type ProtectedValueResolverFunc func(string) ([]byte, bool, error)

// ResolveProtectedValue implements ProtectedValueResolver.
func (f ProtectedValueResolverFunc) ResolveProtectedValue(token string) ([]byte, bool, error) {
	return f(token)
}

// DecryptProtectedValues returns an encrypted-token resolver backed by a
// rotation-aware recorder key resolver. Tokenized values remain unresolved.
func DecryptProtectedValues(keys recorder.ProtectionKeyResolver) ProtectedValueResolver {
	return ProtectedValueResolverFunc(func(token string) ([]byte, bool, error) {
		if !strings.HasPrefix(token, "REC-ENC-v1.") {
			return nil, false, nil
		}

		plaintext, err := recorder.DecryptProtectedValueWith(token, keys)
		if err != nil {
			return nil, false, err
		}

		return plaintext, true, nil
	})
}

// Config defines fixture matching and optional captured-representation access.
type Config struct {
	Match MatchConfig
	// MaxResponseBodyBytes bounds embedded and externally opened responses.
	MaxResponseBodyBytes int64
	// Bodies opens opaque external body-store references.
	Bodies BodyOpener
	// ProtectedValues resolves encrypted or tokenized fixture values.
	ProtectedValues ProtectedValueResolver
	// Timing optionally replays bounded recorded latency. Its zero value
	// disables timing playback.
	Timing ReplayTimingConfig
}

// ReplayTimingConfig controls coarse, deterministic playback of recorded HAR
// timing evidence. Scale=0 disables playback. A positive Scale requires a
// positive MaxDelay, which bounds the total delay applied per exchange.
type ReplayTimingConfig struct {
	// Scale multiplies recorded durations. For example, 0.1 replays at one
	// tenth of the captured latency and 1 uses the captured duration.
	Scale float64
	// MaxDelay bounds the combined response-header and response-body delay for
	// one exchange.
	MaxDelay time.Duration
}

// DefaultConfig returns strict, bounded matching defaults.
func DefaultConfig() Config {
	return Config{
		Match: MatchConfig{
			Headers:             []string{"Content-Type"},
			Body:                BodyMatchAuto,
			MaxRequestBodyBytes: defaultMaxRequestBodyBytes,
		},
		MaxResponseBodyBytes: defaultMaxResponseBodyBytes,
	}
}

type fixtureEntry struct {
	entry *recorder.Entry
}

// EntrySource supplies fixture candidates incrementally. NewStreamTransport
// validates every returned entry. hario EntryStream implements this interface
// for HAR and NDJSON captures and additionally validates the input envelope.
// EntrySource has no lifecycle method; its reader remains caller-owned.
type EntrySource interface {
	Next() (*recorder.Entry, error)
}

// Transport implements http.RoundTripper over a finite ordered entry set.
type Transport struct {
	mu        sync.Mutex
	config    Config
	entries   []fixtureEntry
	source    EntrySource
	exhausted bool
	wait      func(context.Context, time.Duration) error
}

// NewTransport validates entries and creates a network-free fixture transport.
func NewTransport(entries []*recorder.Entry, config Config) (*Transport, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}

	if err := hario.ValidateEntries(entries); err != nil {
		return nil, fmt.Errorf("hartest: validate entries: %w", err)
	}

	fixtures := make([]fixtureEntry, len(entries))
	for index, entry := range entries {
		fixtures[index] = fixtureEntry{entry: entry}
	}

	return &Transport{config: config, entries: fixtures, wait: waitContext}, nil
}

// NewStreamTransport creates a network-free fixture transport that pulls
// entries only as matching requires them. Consumed entries are released;
// unmatched entries remain available for later requests.
func NewStreamTransport(source EntrySource, config Config) (*Transport, error) {
	if source == nil {
		return nil, errors.New("hartest: entry source is required")
	}

	if err := validateConfig(config); err != nil {
		return nil, err
	}

	return &Transport{config: config, source: source, wait: waitContext}, nil
}

// RoundTrip matches and consumes one fixture. It never sends a request to a
// network or another RoundTripper. Matching reads and closes request.Body but
// does not replace or mutate the request's Body or GetBody fields.
func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("hartest: request and URL are required")
	}

	requestBody, err := readRequestBody(request, t.config.Match.MaxRequestBodyBytes)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()

	locked := true

	defer func() {
		if locked {
			t.mu.Unlock()
		}
	}()

	var candidateErr error

	nextIndex := 0

	for {
		for nextIndex < len(t.entries) {
			fixture := &t.entries[nextIndex]

			matched, err := t.matches(request, requestBody, fixture.entry)
			if err != nil {
				candidateErr = errors.Join(candidateErr, err)
				nextIndex++

				continue
			}

			if !matched {
				nextIndex++

				continue
			}

			timing := replayTimingPlan(fixture.entry, t.config.Timing)

			response, err := t.response(request, fixture.entry, timing.body)
			if err != nil {
				var recorded *RecordedError
				if !errors.As(err, &recorded) {
					return nil, err
				}

				t.removeEntry(nextIndex)
				t.mu.Unlock()

				locked = false

				if waitErr := t.wait(request.Context(), timing.beforeResponse); waitErr != nil {
					return nil, waitErr
				}

				return nil, err
			}

			t.removeEntry(nextIndex)
			t.mu.Unlock()

			locked = false

			if err := t.wait(request.Context(), timing.beforeResponse); err != nil {
				_ = response.Body.Close()

				return nil, err
			}

			return response, nil
		}

		loaded, err := t.loadNextEntry()
		if err != nil {
			return nil, err
		}

		if !loaded {
			break
		}
	}

	if candidateErr != nil {
		return nil, fmt.Errorf("hartest: no safely matchable exchange for %s %s: %w", request.Method, safeURL(request.URL), candidateErr)
	}

	return nil, fmt.Errorf("hartest: no matching exchange for %s %s", request.Method, safeURL(request.URL))
}

// Verify drains a lazy source, validates its trailing input, and reports whether
// every fixture was consumed exactly once. Call it after all client requests
// have completed.
func (t *Transport) Verify() error {
	if t == nil {
		return errors.New("hartest: nil transport")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	unused := len(t.entries)
	for {
		loaded, err := t.loadNextEntry()
		if err != nil {
			return err
		}

		if !loaded {
			break
		}

		unused++
		t.entries = t.entries[:len(t.entries)-1]
	}

	if unused > 0 {
		return fmt.Errorf("hartest: %d fixture exchanges were not consumed", unused)
	}

	return nil
}

func (t *Transport) loadNextEntry() (bool, error) {
	if t.source == nil || t.exhausted {
		return false, nil
	}

	entry, err := t.source.Next()
	if errors.Is(err, io.EOF) {
		t.exhausted = true

		return false, nil
	}

	if err != nil {
		t.exhausted = true

		return false, fmt.Errorf("hartest: read fixture entry: %w", err)
	}

	if err := hario.ValidateEntries([]*recorder.Entry{entry}); err != nil {
		t.exhausted = true

		return false, fmt.Errorf("hartest: validate streamed entry: %w", err)
	}

	t.entries = append(t.entries, fixtureEntry{entry: entry})

	return true, nil
}

func (t *Transport) removeEntry(index int) {
	copy(t.entries[index:], t.entries[index+1:])
	t.entries[len(t.entries)-1] = fixtureEntry{}
	t.entries = t.entries[:len(t.entries)-1]
}

func validateConfig(config Config) error {
	switch config.Match.Body {
	case BodyMatchAuto, BodyMatchExact, BodyMatchIgnore:

	default:
		return fmt.Errorf("hartest: unsupported body match mode %q", config.Match.Body)
	}

	if config.Match.MaxRequestBodyBytes <= 0 {
		return errors.New("hartest: MaxRequestBodyBytes must be positive")
	}

	if config.MaxResponseBodyBytes <= 0 {
		return errors.New("hartest: MaxResponseBodyBytes must be positive")
	}

	switch {
	case math.IsNaN(config.Timing.Scale) || math.IsInf(config.Timing.Scale, 0) || config.Timing.Scale < 0:
		return errors.New("hartest: Timing.Scale must be finite and non-negative")

	case config.Timing.Scale == 0 && config.Timing.MaxDelay != 0:
		return errors.New("hartest: Timing.MaxDelay requires a positive Scale")

	case config.Timing.Scale > 0 && config.Timing.MaxDelay <= 0:
		return errors.New("hartest: positive Timing.Scale requires a positive MaxDelay")
	}

	for _, name := range config.Match.Headers {
		if strings.TrimSpace(name) == "" {
			return errors.New("hartest: matched header names must not be empty")
		}
	}

	return nil
}

type replayTiming struct {
	beforeResponse time.Duration
	body           time.Duration
}

func replayTimingPlan(entry *recorder.Entry, config ReplayTimingConfig) replayTiming {
	if config.Scale == 0 || entry == nil || entry.Timings == nil {
		return replayTiming{}
	}

	timings := entry.Timings
	beforeMilliseconds := positiveMilliseconds(
		timings.Blocked,
		timings.DNS,
		timings.Connect,
		timings.Send,
		timings.Wait,
	)
	before := scaledDelay(beforeMilliseconds, config.Scale, config.MaxDelay)
	remaining := config.MaxDelay - before
	body := scaledDelay(positiveMilliseconds(timings.Receive), config.Scale, remaining)

	return replayTiming{beforeResponse: before, body: body}
}

func positiveMilliseconds(values ...float64) float64 {
	var total float64

	for _, value := range values {
		if value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			total += value
		}
	}

	return total
}

func scaledDelay(milliseconds, scale float64, limit time.Duration) time.Duration {
	if milliseconds <= 0 || scale <= 0 || limit <= 0 {
		return 0
	}

	scaledMilliseconds := milliseconds * scale
	limitMilliseconds := float64(limit) / float64(time.Millisecond)

	if scaledMilliseconds >= limitMilliseconds {
		return limit
	}

	return time.Duration(scaledMilliseconds * float64(time.Millisecond))
}

func proportionalDelay(total time.Duration, completed, size int) time.Duration {
	if total <= 0 || completed <= 0 || size <= 0 {
		return 0
	}

	if completed >= size {
		return total
	}

	return time.Duration(float64(total) * (float64(completed) / float64(size)))
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

func readRequestBody(request *http.Request, max int64) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}

	limited := io.LimitReader(request.Body, max+1)
	body, readErr := io.ReadAll(limited)
	closeErr := request.Body.Close()

	if int64(len(body)) > max {
		return nil, fmt.Errorf("hartest: request body exceeds %d bytes", max)
	}

	if readErr != nil {
		return nil, fmt.Errorf("hartest: read request body: %w", readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("hartest: close request body: %w", closeErr)
	}

	return body, nil
}

func (t *Transport) matches(request *http.Request, requestBody []byte, entry *recorder.Entry) (bool, error) {
	fixtureURL, err := url.Parse(entry.Request.URL)
	if err != nil {
		return false, errors.New("invalid fixture URL")
	}

	var fixtureBody []byte
	if t.config.Match.Body == BodyMatchIgnore {
		fixtureBody = nil
	} else {
		var present bool

		fixtureBody, present, err = t.requestBody(entry)
		if err != nil {
			return false, err
		}

		if t.config.Match.Body == BodyMatchAuto && !present && len(requestBody) == 0 {
			fixtureBody = nil
		}
	}

	actual := &RequestSnapshot{
		Method:   request.Method,
		URL:      cloneURL(request.URL),
		Headers:  request.Header.Clone(),
		Trailers: request.Trailer.Clone(),
		Body:     bytes.Clone(requestBody),
	}
	expected := &RequestSnapshot{
		Method:   entry.Request.Method,
		URL:      fixtureURL,
		Headers:  headerFromPairs(entry.Request.Headers),
		Trailers: requestTrailers(entry),
		Body:     bytes.Clone(fixtureBody),
	}

	if normalizer := t.config.Match.Normalize; normalizer != nil {
		if err := normalizer(actual); err != nil {
			return false, errors.New("live request normalization failed")
		}

		if err := normalizer(expected); err != nil {
			return false, errors.New("fixture request normalization failed")
		}

		if actual.URL == nil || expected.URL == nil {
			return false, errors.New("request normalizer removed a URL")
		}
	}

	if actual.Method != expected.Method ||
		!strings.EqualFold(actual.URL.Scheme, expected.URL.Scheme) ||
		!strings.EqualFold(actual.URL.Host, expected.URL.Host) ||
		actual.URL.EscapedPath() != expected.URL.EscapedPath() {
		return false, nil
	}

	matched, err := t.matchQuery(actual.URL.Query(), expected.URL.Query())
	if err != nil || !matched {
		return matched, err
	}

	matched, err = t.matchHeaders(actual.Headers, expected.Headers)
	if err != nil || !matched {
		return matched, err
	}

	matched, err = t.matchTrailers(actual.Trailers, expected.Trailers)
	if err != nil || !matched {
		return matched, err
	}

	if t.config.Match.Body == BodyMatchIgnore {
		return true, nil
	}

	return bytes.Equal(actual.Body, expected.Body), nil
}

func (t *Transport) matchQuery(actual, expected url.Values) (bool, error) {
	if len(actual) != len(expected) {
		return false, nil
	}

	for name, expectedValues := range expected {
		actualValues, ok := actual[name]
		if !ok || len(actualValues) != len(expectedValues) {
			return false, nil
		}

		resolved := make([]string, len(expectedValues))
		for index, value := range expectedValues {
			value, unresolved, err := t.resolveString(value)
			if err != nil {
				return false, errors.New("query protected-value resolution failed")
			}

			if unresolved || strings.Contains(value, redactedValue) {
				return false, fmt.Errorf("query parameter %q cannot be matched because its fixture value is unresolved", name)
			}

			resolved[index] = value
		}

		sort.Strings(actualValues)
		sort.Strings(resolved)

		if !equalStrings(actualValues, resolved) {
			return false, nil
		}
	}

	return true, nil
}

func (t *Transport) matchHeaders(actual, expected http.Header) (bool, error) {
	for _, name := range t.config.Match.Headers {
		expectedValues := expected.Values(name)

		actualValues := actual.Values(name)
		if len(expectedValues) != len(actualValues) {
			return false, nil
		}

		for index, value := range expectedValues {
			resolved, unresolved, err := t.resolveString(value)
			if err != nil {
				return false, fmt.Errorf("matched header %q protected-value resolution failed", http.CanonicalHeaderKey(name))
			}

			if unresolved || strings.Contains(resolved, redactedValue) {
				return false, fmt.Errorf("matched header %q has an unresolved fixture value", http.CanonicalHeaderKey(name))
			}

			if resolved != actualValues[index] {
				return false, nil
			}
		}
	}

	return true, nil
}

func (t *Transport) matchTrailers(actual, expected http.Header) (bool, error) {
	if len(actual) != len(expected) {
		return false, nil
	}

	for name, expectedValues := range expected {
		actualValues := actual.Values(name)
		if len(actualValues) != len(expectedValues) {
			return false, nil
		}

		for index, value := range expectedValues {
			resolved, unresolved, err := t.resolveString(value)
			if err != nil {
				return false, fmt.Errorf("request trailer %q protected-value resolution failed", http.CanonicalHeaderKey(name))
			}

			if unresolved || strings.Contains(resolved, redactedValue) {
				return false, fmt.Errorf("request trailer %q has an unresolved fixture value", http.CanonicalHeaderKey(name))
			}

			if resolved != actualValues[index] {
				return false, nil
			}
		}
	}

	return true, nil
}

func cloneURL(source *url.URL) *url.URL {
	if source == nil {
		return nil
	}

	clone := *source

	return &clone
}

func requestTrailers(entry *recorder.Entry) http.Header {
	if entry.Recorder == nil {
		return make(http.Header)
	}

	return headerFromPairs(entry.Recorder.RequestTrailers)
}

func (t *Transport) requestBody(entry *recorder.Entry) ([]byte, bool, error) {
	info := bodyInfo(entry, true)
	if info != nil {
		if info.Truncated || info.ClosedEarly || !info.Complete {
			return nil, info.Present, errors.New("request fixture body is incomplete")
		}

		if info.Present && info.CapturedBytes < info.TotalBytes {
			return nil, true, errors.New("request fixture body was not fully captured")
		}
	}

	var (
		body    []byte
		present bool
		err     error
	)

	if info != nil && info.Store != "" {
		body, err = t.openBody(info.Store)
		present = true
	} else if entry.Request.PostData != nil {
		body = []byte(entry.Request.PostData.Text)
		present = true

		if entry.Recorder != nil && entry.Recorder.RequestBodyEncoding == "base64" {
			body, err = base64.StdEncoding.Strict().DecodeString(entry.Request.PostData.Text)
		}
	} else if info != nil && info.Present {
		return nil, true, errors.New("request fixture body was not captured")
	}

	if err != nil {
		return nil, present, fmt.Errorf("request fixture body: %w", err)
	}

	body, unresolved, err := t.resolveBody(body, contentType(entry.Request.Headers))
	if err != nil {
		return nil, present, errors.New("request fixture body protected-value resolution failed")
	}

	if unresolved || bytes.Contains(body, []byte(redactedValue)) {
		return nil, present, errors.New("request fixture body contains unresolved protected values")
	}

	return body, present, nil
}

func (t *Transport) response(
	request *http.Request,
	entry *recorder.Entry,
	bodyDelay time.Duration,
) (*http.Response, error) {
	if entry.Response.Status == 0 {
		if entry.Recorder != nil && entry.Recorder.Error != nil {
			return nil, &RecordedError{
				Phase:    entry.Recorder.Error.Phase,
				Message:  entry.Recorder.Error.Message,
				TimedOut: entry.Recorder.Error.Timeout,
			}
		}

		return nil, errors.New("hartest: fixture has no HTTP response")
	}

	body, err := t.responseBody(entry)
	if err != nil {
		return nil, err
	}

	headers, err := t.responseHeaders(entry.Response.Headers)
	if err != nil {
		clear(body)
		return nil, err
	}

	if entry.Recorder != nil && entry.Recorder.ResponseBodyDecoded {
		headers.Del("Content-Encoding")
		headers.Del("Content-Length")
	}

	protoMajor, protoMinor := protocolVersion(entry.Response.HTTPVersion)
	trailers := make(http.Header)
	response := &http.Response{
		StatusCode:    entry.Response.Status,
		Status:        strconv.Itoa(entry.Response.Status) + " " + entry.Response.StatusText,
		Proto:         entry.Response.HTTPVersion,
		ProtoMajor:    protoMajor,
		ProtoMinor:    protoMinor,
		Header:        headers,
		ContentLength: int64(len(body)),
		Request:       request,
		Trailer:       trailers,
	}

	var bodyErr error
	if entry.Recorder != nil && entry.Recorder.ResponseBody != nil && entry.Recorder.ResponseBody.ReadError != "" {
		bodyErr = errors.New("hartest: recorded response body read error")
	}

	var recordedTrailers []recorder.NameValuePair
	if entry.Recorder != nil {
		recordedTrailers = entry.Recorder.ResponseTrailers
	}

	response.Body = &fixtureBody{
		reader:   bytes.NewReader(body),
		bytes:    body,
		readErr:  bodyErr,
		trailers: trailers,
		recorded: recordedTrailers,
		resolver: t,
		context:  request.Context(),
		delay:    bodyDelay,
		wait:     t.wait,
	}

	return response, nil
}

func (t *Transport) responseBody(entry *recorder.Entry) ([]byte, error) {
	info := bodyInfo(entry, false)
	if info != nil {
		if info.Truncated {
			return nil, errors.New("hartest: response fixture body is truncated")
		}

		if info.Present && info.CapturedBytes < info.TotalBytes && info.ReadError == "" {
			return nil, errors.New("hartest: response fixture body was not fully captured")
		}

		if info.Present && info.Store == "" && entry.Response.Content.Text == "" {
			return nil, errors.New("hartest: response fixture body was not captured")
		}
	}

	if info != nil && info.Store != "" {
		body, err := t.openBody(info.Store)
		if err != nil {
			return nil, fmt.Errorf("hartest: response fixture body: %w", err)
		}

		resolved, _, err := t.resolveBody(body, entry.Response.Content.MimeType)
		if err != nil {
			return nil, errors.New("hartest: response body protected-value resolution failed")
		}

		return resolved, nil
	}

	text := entry.Response.Content.Text

	var (
		body []byte
		err  error
	)
	if entry.Response.Content.Encoding == "base64" {
		body, err = base64.StdEncoding.Strict().DecodeString(text)
	} else {
		body = []byte(text)
	}

	if err != nil {
		return nil, fmt.Errorf("hartest: decode response body: %w", err)
	}

	if int64(len(body)) > t.config.MaxResponseBodyBytes {
		return nil, fmt.Errorf("hartest: response fixture body exceeds %d bytes", t.config.MaxResponseBodyBytes)
	}

	body, _, err = t.resolveBody(body, entry.Response.Content.MimeType)
	if err != nil {
		return nil, errors.New("hartest: response body protected-value resolution failed")
	}

	return body, nil
}

func (t *Transport) openBody(ref string) ([]byte, error) {
	if t.config.Bodies == nil {
		return nil, errors.New("external body requires BodyOpener")
	}

	reader, err := t.config.Bodies.Open(ref)
	if err != nil {
		return nil, err
	}

	body, readErr := io.ReadAll(io.LimitReader(reader, t.config.MaxResponseBodyBytes+1))
	closeErr := reader.Close()

	if readErr != nil {
		return nil, readErr
	}

	if closeErr != nil {
		return nil, closeErr
	}

	if int64(len(body)) > t.config.MaxResponseBodyBytes {
		return nil, fmt.Errorf("external body exceeds %d bytes", t.config.MaxResponseBodyBytes)
	}

	return body, nil
}

func (t *Transport) responseHeaders(pairs []recorder.NameValuePair) (http.Header, error) {
	headers := make(http.Header)

	for _, pair := range pairs {
		value, _, err := t.resolveString(pair.Value)
		if err != nil {
			return nil, fmt.Errorf("hartest: response header %q protected-value resolution failed", pair.Name)
		}

		headers.Add(pair.Name, value)
	}

	return headers, nil
}

func (t *Transport) resolveString(value string) (string, bool, error) {
	resolved, unresolved, err := t.resolveBytes([]byte(value), false)

	return string(resolved), unresolved, err
}

func (t *Transport) resolveBody(body []byte, mimeType string) ([]byte, bool, error) {
	jsonBody := strings.Contains(strings.ToLower(strings.Split(mimeType, ";")[0]), "json")

	return t.resolveBytes(body, jsonBody)
}

func (t *Transport) resolveBytes(input []byte, jsonBody bool) ([]byte, bool, error) {
	matches := protectedTokenPattern.FindAllIndex(input, -1)
	if len(matches) == 0 {
		return input, false, nil
	}

	if t.config.ProtectedValues == nil {
		return input, true, nil
	}

	var output bytes.Buffer

	unresolved := false
	offset := 0

	for _, match := range matches {
		start, end := match[0], match[1]
		token := string(input[start:end])

		plaintext, ok, err := t.config.ProtectedValues.ResolveProtectedValue(token)
		if err != nil {
			clear(plaintext)
			return nil, false, err
		}

		replaceStart, replaceEnd := start, end
		if ok && jsonBody && start > 0 && end < len(input) && input[start-1] == '"' && input[end] == '"' && json.Valid(plaintext) {
			replaceStart--
			replaceEnd++
		}

		_, _ = output.Write(input[offset:replaceStart])
		if ok {
			_, _ = output.Write(plaintext)
		} else {
			unresolved = true
			_, _ = output.Write(input[start:end])
		}

		clear(plaintext)

		offset = replaceEnd
	}

	_, _ = output.Write(input[offset:])

	return output.Bytes(), unresolved, nil
}

func headerFromPairs(pairs []recorder.NameValuePair) http.Header {
	headers := make(http.Header)
	for _, pair := range pairs {
		headers.Add(pair.Name, pair.Value)
	}

	return headers
}

func contentType(pairs []recorder.NameValuePair) string {
	return headerFromPairs(pairs).Get("Content-Type")
}

func bodyInfo(entry *recorder.Entry, request bool) *recorder.BodyInfo {
	if entry.Recorder == nil {
		return nil
	}

	if request {
		return entry.Recorder.RequestBody
	}

	return entry.Recorder.ResponseBody
}

func safeURL(value *url.URL) string {
	return value.Scheme + "://" + value.Host + value.EscapedPath()
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}

	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}

func protocolVersion(protocol string) (int, int) {
	switch protocol {
	case "HTTP/1.0":
		return 1, 0

	case "HTTP/1.1":
		return 1, 1

	case "HTTP/2", "HTTP/2.0":
		return 2, 0

	case "HTTP/3", "HTTP/3.0":
		return 3, 0

	default:
		return 0, 0
	}
}

// RecordedError represents a transport failure captured before an HTTP
// response existed. It preserves only evidence recorded by recorder and does
// not manufacture a concrete network or TLS error whose fields and unwrap
// chain are absent from the fixture.
type RecordedError struct {
	// Phase identifies the HTTP lifecycle phase in which the recorded failure
	// occurred.
	Phase string

	// Message is the sanitized failure message stored in the fixture.
	Message string

	// TimedOut reports whether the recorded failure satisfied net.Error's
	// timeout contract.
	TimedOut bool
}

// Error implements error.
func (e *RecordedError) Error() string {
	if e.Message == "" {
		return "hartest: recorded HTTP failure"
	}

	return "hartest: recorded HTTP failure: " + e.Message
}

// Timeout reports the recorded timeout classification.
func (e *RecordedError) Timeout() bool { return e.TimedOut }

type fixtureBody struct {
	reader   *bytes.Reader
	bytes    []byte
	readErr  error
	trailers http.Header
	recorded []recorder.NameValuePair
	resolver *Transport
	closed   bool
	context  context.Context
	delay    time.Duration
	waited   time.Duration
	wait     func(context.Context, time.Duration) error
}

func (b *fixtureBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}

	if len(p) > 0 && b.reader.Len() > 0 {
		readSize := min(len(p), b.reader.Len())
		completed := len(b.bytes) - b.reader.Len() + readSize
		target := proportionalDelay(b.delay, completed, len(b.bytes))

		if delay := target - b.waited; delay > 0 {
			if waitErr := b.wait(b.context, delay); waitErr != nil {
				return 0, waitErr
			}

			b.waited = target
		}
	}

	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.publishTrailers()

		if b.readErr != nil {
			err = b.readErr
			b.readErr = nil
		}
	}

	return n, err
}

func (b *fixtureBody) Close() error {
	if b.closed {
		return nil
	}

	b.closed = true
	b.publishTrailers()
	clear(b.bytes)

	return nil
}

func (b *fixtureBody) publishTrailers() {
	if b.recorded == nil {
		return
	}

	for _, pair := range b.recorded {
		value, _, err := b.resolver.resolveString(pair.Value)
		if err == nil {
			b.trailers.Add(pair.Name, value)
		}
	}

	b.recorded = nil
}
