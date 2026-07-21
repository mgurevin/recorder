package recorder

// InternalErrorMode controls how recorder-internal failures (body store
// errors, protection/key failures, recorder panics) are reported. Protection
// failures are aggregated per exchange direction to avoid log storms. The
// wrapped HTTP call is never retried or altered by an internal error.
type InternalErrorMode int

const (
	// InternalErrorIgnore drops internal errors after reporting them through
	// Config.OnInternalError (when set). The HTTP call is never affected.
	// This is the default.
	InternalErrorIgnore InternalErrorMode = iota

	// InternalErrorLog behaves like InternalErrorIgnore but additionally
	// writes the error using Config.Logf (or the standard log package when
	// Logf is nil).
	InternalErrorLog
)

// Config configures a Transport. The zero value disables every optional
// content/metadata capture; finalized entries still contain the core exchange
// lifecycle and body byte/completion accounting. Pass DefaultConfig explicitly
// to NewTransport for sensible production defaults.
//
// Config is copied by NewTransport. Maps, callbacks, policies, stores and other
// referenced values must be treated as immutable after construction.
type Config struct {
	// CaptureRequestBody enables storing request body content. Body size and
	// completion state are always tracked, even when this is false.
	CaptureRequestBody bool
	// CaptureResponseBody enables storing response body content.
	CaptureResponseBody bool
	// EmbedBodies controls whether captured body content is embedded into
	// the HAR document (postData.text / content.text). When false, bodies
	// are still captured into the BodyStore and the record keeps sizes,
	// hashes, truncation state and the store reference
	// (_recorder.requestBody/responseBody.store) — the intended production
	// setting together with FileBodyStore, keeping HAR documents small
	// while body content stays retrievable. Like the Capture* flags, the
	// zero value disables embedding; set EmbedBodies to enable it.
	EmbedBodies bool
	// MaxRequestBodyBytes limits how many request body bytes are stored.
	// Values <= 0 mean unlimited. The total size keeps being counted after
	// the limit is reached; only content capture stops.
	MaxRequestBodyBytes int64
	// MaxResponseBodyBytes is the response-side equivalent of
	// MaxRequestBodyBytes.
	MaxResponseBodyBytes int64
	// CaptureTLS enables _recorder.tls built from tls.ConnectionState.
	CaptureTLS bool
	// CaptureCertificates includes peer certificate details in _recorder.tls.
	CaptureCertificates bool
	// CaptureRawCertificates additionally embeds each certificate's raw DER
	// bytes as Base64. Off by default because of size.
	CaptureRawCertificates bool
	// CaptureHeaders enables recording request/response headers and trailers.
	CaptureHeaders bool
	// CaptureCookies enables recording parsed cookies.
	CaptureCookies bool

	// Redaction contains global common and direction-specific selector rules
	// plus trusted custom body redactors. Assign the baseline directly;
	// request contexts may add more through WithRequestRedaction.
	Redaction RedactionConfig

	// SensitiveValueProtection controls whether values selected by configured
	// redaction rules and body redactors are removed, reversibly encrypted, or
	// deterministically tokenized. The zero value redacts with [REDACTED].
	// Protection failures always fail closed to [REDACTED].
	SensitiveValueProtection SensitiveValueProtection

	// HashBodies enables hashing of body streams. The hash covers every byte
	// that actually flowed, including bytes beyond the capture limit.
	HashBodies bool
	// BodyHashAlgorithm selects the hash: "sha256" (default), "sha1" or
	// "md5". Unknown values fall back to sha256.
	BodyHashAlgorithm string

	// CaptureRawTrace stores every raw httptrace event under _recorder.trace. Event
	// details pass through RedactErrorMessage before export.
	CaptureRawTrace bool

	// ContentDecoders maps Content-Encoding tokens (lower-case) to decoders
	// used by the capture and HAR-building pipeline. With body redaction
	// configured, supported encodings are decoded and redacted
	// while streaming into the BodyStore; otherwise a fully captured body
	// may be decoded when embedded in the HAR. DefaultConfig installs the
	// stdlib-only set (gzip, x-gzip, deflate); add entries to the map for
	// encodings such as brotli or zstd — see ContentDecoder for
	// ready-to-paste recipes. Decoding never touches bytes read by the caller.
	ContentDecoders map[string]ContentDecoder

	// BodyCapturePolicy optionally overrides capture, embedding, hashing,
	// limits, and body-redactor selection for each request and response body.
	// Policy errors and panics fail closed to metadata-only recording.
	BodyCapturePolicy BodyCapturePolicy

	// HeadSamplingPolicy decides before exchange instrumentation whether to
	// record fully, retain metadata only, or bypass recording entirely.
	HeadSamplingPolicy HeadSamplingPolicy
	// RetentionPolicy decides whether a finalized entry reaches Recorder. The
	// completion callback still runs first; capture cost has already been paid.
	RetentionPolicy RetentionPolicy

	// BodyStore provides transactional storage for captured body bytes. Writers
	// commit on body finalization and abort on retry or processing/storage
	// failure. Nil means MemoryBodyStore.
	BodyStore BodyStore

	// InternalErrorMode selects the internal error policy. See the constants.
	InternalErrorMode InternalErrorMode
	// OnInternalError, when set, receives every recorder-internal error.
	OnInternalError func(error)
	// Logf is used by InternalErrorLog. Nil falls back to log.Printf.
	Logf func(format string, args ...any)

	// OnEntryCompleted, when set, is invoked with the request context and the
	// finished entry every time an instrumented exchange is finalized, before
	// retention and Recorder.Record. The entry and body assets are borrowed for
	// the callback duration; retaining ownership requires a Recorder.
	OnEntryCompleted OnEntryCompleted

	// RedactErrorMessage, when set, is applied to every error message and raw
	// trace detail before export (they can contain URLs or credentials).
	RedactErrorMessage func(string) string
}

// DefaultConfig returns the recommended production-safe configuration. Body
// streams are counted for lifecycle and size metadata, but their
// content is neither stored, embedded nor hashed by default. TLS metadata,
// headers and cookies remain enabled, with DefaultRedactedHeaders applied.
func DefaultConfig() Config {
	return Config{
		MaxRequestBodyBytes:  1 << 20,
		MaxResponseBodyBytes: 1 << 20,
		CaptureTLS:           true,
		CaptureCertificates:  true,
		CaptureHeaders:       true,
		CaptureCookies:       true,
		Redaction: RedactionConfig{Common: RedactionRules{
			Headers: DefaultRedactedHeaders(),
		}},
		BodyHashAlgorithm: "sha256",
		ContentDecoders:   defaultContentDecoders(),
	}
}

// DefaultRedactedHeaders returns the headers redacted by default.
func DefaultRedactedHeaders() []string {
	return []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-API-Key"}
}
