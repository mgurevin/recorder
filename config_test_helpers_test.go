package recorder

import (
	"strings"
	"time"
)

type configMutation func(*Config)

func configWith(opts ...configMutation) Config {
	config := DefaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}
	return config
}

func withConfig(v Config) configMutation { return func(config *Config) { *config = v } }
func withCaptureRequestBody(v bool) configMutation {
	return func(config *Config) { config.CaptureRequestBody = v }
}
func withCaptureResponseBody(v bool) configMutation {
	return func(config *Config) { config.CaptureResponseBody = v }
}
func withEmbedBodies(v bool) configMutation { return func(config *Config) { config.EmbedBodies = v } }
func withMaxRequestBodyBytes(v int64) configMutation {
	return func(config *Config) { config.MaxRequestBodyBytes = v }
}
func withMaxResponseBodyBytes(v int64) configMutation {
	return func(config *Config) { config.MaxResponseBodyBytes = v }
}
func withCaptureTLS(v bool) configMutation { return func(config *Config) { config.CaptureTLS = v } }
func withCaptureCertificates(v, raw bool) configMutation {
	return func(config *Config) { config.CaptureCertificates, config.CaptureRawCertificates = v, raw }
}
func withCaptureHeaders(v bool) configMutation {
	return func(config *Config) { config.CaptureHeaders = v }
}
func withCaptureCookies(v bool) configMutation {
	return func(config *Config) { config.CaptureCookies = v }
}
func withRedaction(v RedactionConfig) configMutation {
	return func(config *Config) { config.Redaction = mergeRedactionConfig(config.Redaction, v) }
}
func withSensitiveValueProtection(v SensitiveValueProtection) configMutation {
	return func(config *Config) { config.SensitiveValueProtection = v }
}
func withHashBodies(enabled bool, algorithm string) configMutation {
	return func(config *Config) { config.HashBodies, config.BodyHashAlgorithm = enabled, algorithm }
}
func withBodyStore(v BodyStore) configMutation { return func(config *Config) { config.BodyStore = v } }
func withCaptureRawTrace(v bool) configMutation {
	return func(config *Config) { config.CaptureRawTrace = v }
}
func withContentDecoder(encoding string, decoder ContentDecoder) configMutation {
	return func(config *Config) {
		if config.ContentDecoders == nil {
			config.ContentDecoders = map[string]ContentDecoder{}
		}
		config.ContentDecoders[strings.ToLower(strings.TrimSpace(encoding))] = decoder
	}
}
func withBodyCapturePolicy(v BodyCapturePolicy) configMutation {
	return func(config *Config) { config.BodyCapturePolicy = v }
}
func withHeadSamplingPolicy(v HeadSamplingPolicy) configMutation {
	return func(config *Config) { config.HeadSamplingPolicy = v }
}
func withRetentionPolicy(v RetentionPolicy) configMutation {
	return func(config *Config) { config.RetentionPolicy = v }
}
func withInternalErrorMode(v InternalErrorMode) configMutation {
	return func(config *Config) { config.InternalErrorMode = v }
}
func withOnInternalError(v func(error)) configMutation {
	return func(config *Config) { config.OnInternalError = v }
}
func withLogf(v func(string, ...any)) configMutation { return func(config *Config) { config.Logf = v } }
func withOnEntryCompleted(v OnEntryCompleted) configMutation {
	return func(config *Config) { config.OnEntryCompleted = v }
}
func withErrorRedactor(v func(string) string) configMutation {
	return func(config *Config) { config.RedactErrorMessage = v }
}

type asyncConfigMutation func(*AsyncRecorderConfig)

func asyncConfigWith(opts ...asyncConfigMutation) AsyncRecorderConfig {
	config := DefaultAsyncRecorderConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}
	return config
}
func withAsyncQueueCapacity(v int) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.QueueCapacity = v }
}
func withAsyncBackpressurePolicy(v AsyncBackpressurePolicy) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.Backpressure = v }
}
func withAsyncBlockTimeout(timeout time.Duration, fallback AsyncBackpressurePolicy) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.BlockTimeout, config.BlockTimeoutPolicy = timeout, fallback }
}
func withAsyncDropHandler(v AsyncDropHandler) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.DropHandler = v }
}
func withAsyncCloseSink(v bool) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.CloseSink = v }
}
func withAsyncErrorHandler(v func(error)) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.ErrorHandler = v }
}
func withAsyncBatchSize(v int) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.BatchSize = v }
}
func withAsyncFlushInterval(v time.Duration) asyncConfigMutation {
	return func(config *AsyncRecorderConfig) { config.FlushInterval = v }
}

type fileBodyStoreConfigMutation func(*FileBodyStoreConfig)

func fileBodyStoreConfigWith(opts ...fileBodyStoreConfigMutation) FileBodyStoreConfig {
	config := DefaultFileBodyStoreConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&config)
		}
	}
	return config
}
func withFileBodyMaxBytes(v int64) fileBodyStoreConfigMutation {
	return func(config *FileBodyStoreConfig) { config.MaxBytes = v }
}
func withFileBodyMaxFiles(v int) fileBodyStoreConfigMutation {
	return func(config *FileBodyStoreConfig) { config.MaxFiles = v }
}
func withFileBodyPartialTTL(v time.Duration) fileBodyStoreConfigMutation {
	return func(config *FileBodyStoreConfig) { config.PartialTTL = v }
}
func withFileBodySyncOnCommit(v bool) fileBodyStoreConfigMutation {
	return func(config *FileBodyStoreConfig) { config.SyncOnCommit = v }
}
