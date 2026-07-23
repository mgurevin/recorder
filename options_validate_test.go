package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type validationBodyRedactor struct{}

func (validationBodyRedactor) Redact(io.Writer, string, BodyValueProtector) (io.WriteCloser, error) {
	panic("BodyRedactor.Redact called during validation")
}

type validationBodyStore struct{}

func (validationBodyStore) NewWriter(context.Context, BodyMetadata) (BodyWriter, error) {
	panic("BodyStore.NewWriter called during validation")
}

func TestConfigValidateAcceptsSupportedConfigurations(t *testing.T) {
	t.Parallel()

	keyProvider := ProtectionKeyProvider(func(context.Context, ProtectionMode) (ProtectionKey, error) {
		return ProtectionKey{}, errors.New("must not be called during validation")
	})
	redactor := validationBodyRedactor{}
	decoder := func(io.Reader) (io.ReadCloser, error) { return nil, errors.New("must not be called during validation") }

	configs := []Config{
		{},
		DefaultConfig(),
		{BodyHashAlgorithm: "sha256"},
		{BodyHashAlgorithm: "sha1"},
		{BodyHashAlgorithm: "md5"},
		{SensitiveValueProtection: SensitiveValueProtection{Mode: ProtectionRedact}},
		{SensitiveValueProtection: SensitiveValueProtection{Mode: ProtectionEncrypt, KeyProvider: keyProvider}},
		{SensitiveValueProtection: SensitiveValueProtection{Mode: ProtectionTokenize, KeyProvider: keyProvider}},
		{ContentDecoders: map[string]ContentDecoder{"br": decoder, "x-custom": decoder}},
		{Redaction: RedactionConfig{Common: RedactionRules{
			Headers:       []string{"Authorization"},
			JSONFields:    []string{"password"},
			BodyRedactors: map[string]BodyRedactor{"text/csv": redactor},
		}}},
	}

	for index, config := range configs {
		config := config

		t.Run(testName(index), func(t *testing.T) {
			t.Parallel()

			if err := config.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestConfigValidateDoesNotExecuteRuntimeExtensions(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	config.BodyStore = validationBodyStore{}
	config.BodyCapturePolicy = func(context.Context, BodyCaptureMeta, BodyCaptureDecision) (BodyCaptureDecision, error) {
		panic("BodyCapturePolicy called during validation")
	}
	config.HeadSamplingPolicy = func(context.Context, HeadSamplingMeta) HeadSamplingDecision {
		panic("HeadSamplingPolicy called during validation")
	}
	config.OnHeadSamplingDecision = func(context.Context, HeadSamplingMeta, HeadSamplingDecision) {
		panic("OnHeadSamplingDecision called during validation")
	}
	config.RetentionPolicy = func(context.Context, *Entry) RetentionDecision {
		panic("RetentionPolicy called during validation")
	}
	config.OnInternalError = func(error) {
		panic("OnInternalError called during validation")
	}
	config.Logf = func(string, ...any) {
		panic("Logf called during validation")
	}
	config.OnEntryCompleted = func(context.Context, *Entry, EntryDisposition) {
		panic("OnEntryCompleted called during validation")
	}
	config.RedactErrorMessage = func(string) string {
		panic("RedactErrorMessage called during validation")
	}

	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestConfigValidateReportsStaticErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config Config
		want   []string
	}{
		{
			name:   "certificate hierarchy",
			config: Config{CaptureRawCertificates: true},
			want: []string{
				"Config.CaptureRawCertificates requires CaptureCertificates",
				"Config.CaptureRawCertificates requires CaptureTLS",
			},
		},
		{
			name:   "certificates require TLS",
			config: Config{CaptureCertificates: true},
			want:   []string{"Config.CaptureCertificates requires CaptureTLS"},
		},
		{
			name:   "hash algorithm",
			config: Config{BodyHashAlgorithm: "SHA-256"},
			want:   []string{`Config.BodyHashAlgorithm "SHA-256" is invalid`},
		},
		{
			name: "encryption key provider",
			config: Config{SensitiveValueProtection: SensitiveValueProtection{
				Mode: ProtectionEncrypt,
			}},
			want: []string{"KeyProvider is required"},
		},
		{
			name: "invalid protection mode",
			config: Config{SensitiveValueProtection: SensitiveValueProtection{
				Mode: ProtectionMode("mask"),
			}},
			want: []string{`Mode "mask" is invalid`},
		},
		{
			name:   "internal error mode",
			config: Config{InternalErrorMode: InternalErrorMode(42)},
			want:   []string{"Config.InternalErrorMode 42 is invalid"},
		},
		{
			name: "decoder keys and values",
			config: Config{ContentDecoders: map[string]ContentDecoder{
				"":         nil,
				"GZIP":     GzipDecoder,
				"br, gz":   GzipDecoder,
				"identity": GzipDecoder,
			}},
			want: []string{
				`ContentDecoders key ""`,
				`ContentDecoders[""] must not be nil`,
				`ContentDecoders key "GZIP"`,
				`ContentDecoders key "br, gz"`,
				`ContentDecoders key "identity"`,
			},
		},
		{
			name: "empty selectors",
			config: Config{Redaction: RedactionConfig{
				Common:  RedactionRules{Headers: []string{"Authorization", " ", " Cookie"}},
				Request: RedactionRules{JSONFields: []string{""}},
			}},
			want: []string{
				"Config.Redaction.Common.Headers[1] must not be empty",
				"Config.Redaction.Common.Headers[2] must not have surrounding whitespace",
				"Config.Redaction.Request.JSONFields[0] must not be empty",
			},
		},
		{
			name: "body redactors",
			config: Config{Redaction: RedactionConfig{Response: RedactionRules{
				BodyRedactors: map[string]BodyRedactor{
					"Text/CSV":                  validationBodyRedactor{},
					"application/json":          nil,
					"text/plain; charset=utf-8": validationBodyRedactor{},
				},
			}}},
			want: []string{
				`BodyRedactors key "Text/CSV" must be a normalized base MIME type`,
				`BodyRedactors["application/json"] must not be nil`,
				`BodyRedactors key "text/plain; charset=utf-8" must be a normalized base MIME type`,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.config.Validate()
			if err == nil {
				t.Fatal("Validate() = nil")
			}

			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate() error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestConfigValidateJoinsIndependentErrorsDeterministically(t *testing.T) {
	t.Parallel()

	config := Config{
		CaptureCertificates: true,
		BodyHashAlgorithm:   "unknown",
		InternalErrorMode:   InternalErrorMode(99),
		ContentDecoders: map[string]ContentDecoder{
			"ZSTD": nil,
			"BR":   nil,
		},
	}

	first := config.Validate()
	second := config.Validate()

	if first == nil || second == nil {
		t.Fatal("Validate() = nil")
	}

	if first.Error() != second.Error() {
		t.Fatalf("Validate() is not deterministic:\nfirst:  %s\nsecond: %s", first, second)
	}

	for _, want := range []string{
		"CaptureCertificates",
		"BodyHashAlgorithm",
		"InternalErrorMode",
		`ContentDecoders key "BR"`,
		`ContentDecoders key "ZSTD"`,
	} {
		if !strings.Contains(first.Error(), want) {
			t.Errorf("Validate() error %q does not contain %q", first, want)
		}
	}
}

func testName(index int) string {
	return fmt.Sprintf("config_%d", index)
}
