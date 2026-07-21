package recorder

import "strings"

// InternalErrorMode controls how recorder-internal failures (body store
// errors, protection/key failures, recorder panics) are reported. Protection
// failures are aggregated per exchange direction to avoid log storms. The
// wrapped HTTP call is never retried or altered by an internal error.
type InternalErrorMode int

const (
	// InternalErrorIgnore drops internal errors after reporting them through
	// Options.OnInternalError (when set). The HTTP call is never affected.
	// This is the default.
	InternalErrorIgnore InternalErrorMode = iota

	// InternalErrorLog behaves like InternalErrorIgnore but additionally
	// writes the error using Options.Logf (or the standard log package when
	// Logf is nil).
	InternalErrorLog
)

// Options configures a Transport. The zero value disables every optional
// content/metadata capture; finalized entries still contain the core exchange
// lifecycle and body byte/completion accounting. Use DefaultOptions (applied
// automatically by NewTransport) for sensible production defaults.
//
// Options must not be mutated after the Transport has served its first
// request.
type Options struct {
	// CaptureRequestBody enables storing request body content. Body size and
	// completion state are always tracked, even when this is false.
	CaptureRequestBody bool
	// CaptureResponseBody enables storing response body content.
	CaptureResponseBody bool
	// EmbedBodies controls whether captured body content is embedded into
	// the HAR document (postData.text / content.text). When false, bodies
	// are still captured into the BodyStore and the record keeps sizes,
	// hashes, truncation state and the store reference
	// ("_requestBody"/"_responseBody".store) — the intended production
	// setting together with FileBodyStore, keeping HAR documents small
	// while body content stays retrievable. Like the Capture* flags, the
	// zero value disables embedding; WithEmbedBodies enables it explicitly.
	EmbedBodies bool
	// MaxRequestBodyBytes limits how many request body bytes are stored.
	// Values <= 0 mean unlimited. The total size keeps being counted after
	// the limit is reached; only content capture stops.
	MaxRequestBodyBytes int64
	// MaxResponseBodyBytes is the response-side equivalent of
	// MaxRequestBodyBytes.
	MaxResponseBodyBytes int64
	// CaptureTLS enables the "_tls" extension built from tls.ConnectionState.
	CaptureTLS bool
	// CaptureCertificates includes peer certificate details in "_tls".
	CaptureCertificates bool
	// CaptureRawCertificates additionally embeds each certificate's raw DER
	// bytes as Base64. Off by default because of size.
	CaptureRawCertificates bool
	// CaptureHeaders enables recording request/response headers and trailers.
	CaptureHeaders bool
	// CaptureCookies enables recording parsed cookies.
	CaptureCookies bool

	// Redaction contains global common and direction-specific selector rules
	// plus trusted custom body redactors. WithRedaction adds to this baseline;
	// request contexts may add more through WithRequestRedaction.
	Redaction RedactionConfig

	// SensitiveValueProtection controls whether values selected by built-in
	// redaction rules are removed, reversibly encrypted, or deterministically
	// tokenized. The zero value redacts with [REDACTED]. Protection failures
	// always fail closed to [REDACTED].
	SensitiveValueProtection SensitiveValueProtection

	// HashBodies enables hashing of body streams. The hash covers every byte
	// that actually flowed, including bytes beyond the capture limit.
	HashBodies bool
	// BodyHashAlgorithm selects the hash: "sha256" (default), "sha1" or
	// "md5". Unknown values fall back to sha256.
	BodyHashAlgorithm string

	// CaptureRawTrace stores every raw httptrace event under "_trace". Event
	// details pass through RedactErrorMessage before export.
	CaptureRawTrace bool

	// ContentDecoders maps Content-Encoding tokens (lower-case) to decoders
	// used by the capture and HAR-building pipeline. With body redaction
	// configured, supported encodings are decoded and redacted
	// while streaming into the BodyStore; otherwise a fully captured body
	// may be decoded when embedded in the HAR. DefaultOptions installs the
	// stdlib-only set (gzip, x-gzip, deflate); WithContentDecoder registers
	// additional ones such as brotli or zstd — see ContentDecoder for
	// ready-to-paste recipes. Decoding never touches bytes read by the caller.
	ContentDecoders map[string]ContentDecoder

	// BodyCapturePolicy optionally overrides capture, embedding, hashing,
	// limits, and body-redactor selection for each request and response body.
	// Policy errors and panics fail closed to metadata-only recording.
	BodyCapturePolicy BodyCapturePolicy

	// BodyStore provides storage for captured body bytes. Nil means
	// MemoryBodyStore.
	BodyStore BodyStore

	// InternalErrorMode selects the internal error policy. See the constants.
	InternalErrorMode InternalErrorMode
	// OnInternalError, when set, receives every recorder-internal error.
	OnInternalError func(error)
	// Logf is used by InternalErrorLog. Nil falls back to log.Printf.
	Logf func(format string, args ...any)

	// OnEntryCompleted, when set, is invoked with the request context and the
	// finished entry every time an exchange is finalized (after
	// Transport.Recorder.Record).
	OnEntryCompleted OnEntryCompleted

	// RedactErrorMessage, when set, is applied to every error message and raw
	// trace detail before export (they can contain URLs or credentials).
	RedactErrorMessage func(string) string
}

// DefaultOptions returns the production-safe options NewTransport starts
// from. Body streams are counted for lifecycle and size metadata, but their
// content is neither stored, embedded nor hashed by default. TLS metadata,
// headers and cookies remain enabled, with DefaultRedactedHeaders applied.
func DefaultOptions() Options {
	return Options{
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

// Option mutates Options during NewTransport.
type Option func(*Options)

// WithOptions replaces the whole Options value. Apply it first when combining
// with other Option values.
func WithOptions(o Options) Option { return func(dst *Options) { *dst = o } }

// WithCaptureRequestBody toggles request body content capture.
func WithCaptureRequestBody(v bool) Option { return func(o *Options) { o.CaptureRequestBody = v } }

// WithCaptureResponseBody toggles response body content capture.
func WithCaptureResponseBody(v bool) Option { return func(o *Options) { o.CaptureResponseBody = v } }

// WithEmbedBodies toggles embedding captured body content into the HAR
// document. See Options.EmbedBodies.
func WithEmbedBodies(v bool) Option { return func(o *Options) { o.EmbedBodies = v } }

// WithMaxRequestBodyBytes sets the request body capture limit (<= 0: unlimited).
func WithMaxRequestBodyBytes(n int64) Option { return func(o *Options) { o.MaxRequestBodyBytes = n } }

// WithMaxResponseBodyBytes sets the response body capture limit (<= 0: unlimited).
func WithMaxResponseBodyBytes(n int64) Option {
	return func(o *Options) { o.MaxResponseBodyBytes = n }
}

// WithCaptureTLS toggles the "_tls" extension.
func WithCaptureTLS(v bool) Option { return func(o *Options) { o.CaptureTLS = v } }

// WithCaptureCertificates toggles certificate detail capture; raw controls
// embedding of raw DER bytes.
func WithCaptureCertificates(v, raw bool) Option {
	return func(o *Options) { o.CaptureCertificates = v; o.CaptureRawCertificates = raw }
}

// WithCaptureHeaders toggles header capture.
func WithCaptureHeaders(v bool) Option { return func(o *Options) { o.CaptureHeaders = v } }

// WithCaptureCookies toggles cookie capture.
func WithCaptureCookies(v bool) Option { return func(o *Options) { o.CaptureCookies = v } }

// WithRedaction adds common and direction-specific rules to the Transport's
// redaction baseline. Name selectors are additive. A later custom body-redactor
// registration wins for the same normalized MIME type.
func WithRedaction(config RedactionConfig) Option {
	return func(o *Options) { o.Redaction = mergeRedactionConfig(o.Redaction, config) }
}

// WithSensitiveValueProtection configures the representation of values
// selected by built-in redaction rules.
func WithSensitiveValueProtection(config SensitiveValueProtection) Option {
	return func(o *Options) { o.SensitiveValueProtection = config }
}

// WithHashBodies configures body hashing.
func WithHashBodies(enabled bool, algorithm string) Option {
	return func(o *Options) { o.HashBodies = enabled; o.BodyHashAlgorithm = algorithm }
}

// WithBodyStore sets the storage backend for captured body bytes.
func WithBodyStore(s BodyStore) Option { return func(o *Options) { o.BodyStore = s } }

// WithCaptureRawTrace toggles the "_trace" raw event extension. Details are
// sanitized by RedactErrorMessage before export.
func WithCaptureRawTrace(v bool) Option { return func(o *Options) { o.CaptureRawTrace = v } }

// WithContentDecoder registers a decoder for a Content-Encoding token
// (case-insensitive), e.g. "br" or "zstd". See ContentDecoder for recipes.
func WithContentDecoder(encoding string, dec ContentDecoder) Option {
	return func(o *Options) {
		if o.ContentDecoders == nil {
			o.ContentDecoders = map[string]ContentDecoder{}
		}

		o.ContentDecoders[strings.ToLower(strings.TrimSpace(encoding))] = dec
	}
}

// WithBodyCapturePolicy installs a per-request/per-response body capture
// policy. Nil restores the global Options behavior.
func WithBodyCapturePolicy(policy BodyCapturePolicy) Option {
	return func(o *Options) { o.BodyCapturePolicy = policy }
}

// WithInternalErrorMode sets the internal error policy.
func WithInternalErrorMode(m InternalErrorMode) Option {
	return func(o *Options) { o.InternalErrorMode = m }
}

// WithOnInternalError sets the internal error callback.
func WithOnInternalError(fn func(error)) Option {
	return func(o *Options) { o.OnInternalError = fn }
}

// WithLogf sets the logger used by InternalErrorLog.
func WithLogf(fn func(format string, args ...any)) Option {
	return func(o *Options) { o.Logf = fn }
}

// WithOnEntryCompleted sets the per-entry completion callback.
func WithOnEntryCompleted(fn OnEntryCompleted) Option {
	return func(o *Options) { o.OnEntryCompleted = fn }
}

// WithErrorRedactor sets the error message redaction function.
func WithErrorRedactor(fn func(string) string) Option {
	return func(o *Options) { o.RedactErrorMessage = fn }
}
