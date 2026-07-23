package recorder

import (
	"errors"
	"fmt"
	"mime"
	"sort"
	"strings"
)

// Validate reports static configuration errors without performing I/O or
// invoking callbacks, policies, stores, decoders, redactors, or key providers.
// Both Config{} and DefaultConfig() are valid. Call Validate after applying
// application settings and before passing the copied configuration to
// NewTransport.
func (c Config) Validate() error {
	var errs []error

	errs = append(errs, validateCertificateConfig(c)...)
	errs = append(errs, validateHashConfig(c)...)
	errs = append(errs, validateProtectionConfig(c)...)
	errs = append(errs, validateInternalErrorConfig(c)...)
	errs = append(errs, validateContentDecoders(c.ContentDecoders)...)
	errs = append(errs, validateRedactionConfig(c.Redaction)...)

	return errors.Join(errs...)
}

func validateCertificateConfig(c Config) []error {
	var errs []error

	if c.CaptureCertificates && !c.CaptureTLS {
		errs = append(errs, errors.New("recorder: Config.CaptureCertificates requires CaptureTLS"))
	}

	if c.CaptureRawCertificates && !c.CaptureCertificates {
		errs = append(errs, errors.New("recorder: Config.CaptureRawCertificates requires CaptureCertificates"))
	}

	if c.CaptureRawCertificates && !c.CaptureTLS {
		errs = append(errs, errors.New("recorder: Config.CaptureRawCertificates requires CaptureTLS"))
	}

	return errs
}

func validateHashConfig(c Config) []error {
	switch c.BodyHashAlgorithm {
	case "", "sha256", "sha1", "md5":
		return nil

	default:
		return []error{fmt.Errorf(
			"recorder: Config.BodyHashAlgorithm %q is invalid; use sha256, sha1, or md5",
			c.BodyHashAlgorithm,
		)}
	}
}

func validateProtectionConfig(c Config) []error {
	protection := c.SensitiveValueProtection

	switch protection.Mode {
	case "", ProtectionRedact:
		return nil

	case ProtectionEncrypt, ProtectionTokenize:
		if protection.KeyProvider == nil {
			return []error{fmt.Errorf(
				"recorder: Config.SensitiveValueProtection.KeyProvider is required for mode %q",
				protection.Mode,
			)}
		}

		return nil

	default:
		return []error{fmt.Errorf(
			"recorder: Config.SensitiveValueProtection.Mode %q is invalid",
			protection.Mode,
		)}
	}
}

func validateInternalErrorConfig(c Config) []error {
	switch c.InternalErrorMode {
	case InternalErrorIgnore, InternalErrorLog:
		return nil

	default:
		return []error{fmt.Errorf(
			"recorder: Config.InternalErrorMode %d is invalid",
			c.InternalErrorMode,
		)}
	}
}

func validateContentDecoders(decoders map[string]ContentDecoder) []error {
	keys := sortedKeys(decoders)
	errs := make([]error, 0)

	for _, encoding := range keys {
		decoder := decoders[encoding]

		if !validContentCoding(encoding) {
			errs = append(errs, fmt.Errorf(
				"recorder: Config.ContentDecoders key %q must be one lower-case HTTP content-coding token",
				encoding,
			))
		}

		if decoder == nil {
			errs = append(errs, fmt.Errorf(
				"recorder: Config.ContentDecoders[%q] must not be nil",
				encoding,
			))
		}
	}

	return errs
}

func validateRedactionConfig(config RedactionConfig) []error {
	var errs []error

	errs = append(errs, validateRedactionRules("Common", config.Common)...)
	errs = append(errs, validateRedactionRules("Request", config.Request)...)
	errs = append(errs, validateRedactionRules("Response", config.Response)...)

	return errs
}

func validateRedactionRules(scope string, rules RedactionRules) []error {
	var errs []error

	errs = append(errs, validateSelectorNames(scope, "Headers", rules.Headers)...)
	errs = append(errs, validateSelectorNames(scope, "QueryParameters", rules.QueryParameters)...)
	errs = append(errs, validateSelectorNames(scope, "Cookies", rules.Cookies)...)
	errs = append(errs, validateSelectorNames(scope, "JSONFields", rules.JSONFields)...)
	errs = append(errs, validateSelectorNames(scope, "XMLElements", rules.XMLElements)...)
	errs = append(errs, validateBodyRedactors(scope, rules.BodyRedactors)...)

	return errs
}

func validateSelectorNames(scope, field string, names []string) []error {
	var errs []error

	for index, name := range names {
		switch {
		case strings.TrimSpace(name) == "":
			errs = append(errs, fmt.Errorf(
				"recorder: Config.Redaction.%s.%s[%d] must not be empty",
				scope,
				field,
				index,
			))

		case name != strings.TrimSpace(name):
			errs = append(errs, fmt.Errorf(
				"recorder: Config.Redaction.%s.%s[%d] must not have surrounding whitespace",
				scope,
				field,
				index,
			))
		}
	}

	return errs
}

func validateBodyRedactors(scope string, redactors map[string]BodyRedactor) []error {
	keys := sortedKeys(redactors)

	var errs []error

	for _, mediaType := range keys {
		redactor := redactors[mediaType]

		base, params, err := mime.ParseMediaType(mediaType)
		if err != nil || len(params) != 0 || mediaType != strings.ToLower(strings.TrimSpace(mediaType)) || base != mediaType {
			errs = append(errs, fmt.Errorf(
				"recorder: Config.Redaction.%s.BodyRedactors key %q must be a normalized base MIME type",
				scope,
				mediaType,
			))
		}

		if redactor == nil {
			errs = append(errs, fmt.Errorf(
				"recorder: Config.Redaction.%s.BodyRedactors[%q] must not be nil",
				scope,
				mediaType,
			))
		}
	}

	return errs
}

func validContentCoding(value string) bool {
	if value == "" || value == "identity" || value == "*" || value != strings.ToLower(strings.TrimSpace(value)) {
		return false
	}

	for index := 0; index < len(value); index++ {
		if !httpTokenByte(value[index]) {
			return false
		}
	}

	return true
}

func httpTokenByte(value byte) bool {
	switch {
	case value >= '0' && value <= '9':
		return true

	case value >= 'a' && value <= 'z':
		return true
	}

	return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
