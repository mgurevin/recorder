package recorder

import (
	"context"
	"net/http"
)

// RedactionRules adds field selectors and body redactors for one side of an
// HTTP exchange. Names are matched case-insensitively. Configurations merge
// additively, so request-scoped selectors cannot remove Transport selectors.
// A BodyRedactors entry is an explicit trusted override for its MIME type.
type RedactionRules struct {
	// Headers lists header names whose values are protected. A cookie is also
	// protected when its Cookie or Set-Cookie carrier header is selected.
	Headers []string
	// QueryParameters protects URL/query-string values, URL-encoded form
	// fields, and matching multipart field/file payloads and filenames.
	QueryParameters []string
	// Cookies lists parsed cookie names whose values are protected.
	Cookies []string
	// JSONFields recursively protects matching JSON object-field values while
	// preserving unaffected source bytes.
	JSONFields []string
	// XMLElements protects matching XML element subtrees by local name;
	// namespace prefixes are ignored and attributes are not selected.
	XMLElements []string
	// BodyRedactors registers trusted streaming redactors by exact normalized
	// base MIME type. A later merged registration wins for the same type.
	BodyRedactors map[string]BodyRedactor
}

// RedactionConfig contains common and direction-specific redaction rules.
// Common rules apply to both sides. The same type configures a Transport with
// Config.Redaction and an individual request with WithRequestRedaction.
type RedactionConfig struct {
	// Common applies to request and response recordings.
	Common RedactionRules
	// Request adds rules only to request recordings.
	Request RedactionRules
	// Response adds rules only to response recordings.
	Response RedactionRules
}

type requestRedactionContextKey struct{}

// WithRequestRedaction returns a context carrying additional redaction rules.
// Repeated calls merge rules; for duplicate body-redactor MIME registrations,
// the most recent call wins. Input slices and maps are copied before storage.
func WithRequestRedaction(ctx context.Context, config RedactionConfig) context.Context {
	current := requestRedactionFromContext(ctx)
	merged := mergeRedactionConfig(current, config)

	return context.WithValue(ctx, requestRedactionContextKey{}, merged)
}

// RequestWithRedaction clones req with a context carrying additional redaction
// rules. It returns nil when req is nil and never mutates the supplied request.
func RequestWithRedaction(req *http.Request, config RedactionConfig) *http.Request {
	if req == nil {
		return nil
	}

	return req.WithContext(WithRequestRedaction(req.Context(), config))
}

func requestRedactionFromContext(ctx context.Context) RedactionConfig {
	if ctx == nil {
		return RedactionConfig{}
	}

	rules, _ := ctx.Value(requestRedactionContextKey{}).(RedactionConfig)

	return rules
}

func mergeRedactionConfig(left, right RedactionConfig) RedactionConfig {
	return RedactionConfig{
		Common:   mergeRedactionRules(left.Common, right.Common),
		Request:  mergeRedactionRules(left.Request, right.Request),
		Response: mergeRedactionRules(left.Response, right.Response),
	}
}

func effectiveRedactionRules(common, directional RedactionRules) RedactionRules {
	return mergeRedactionRules(common, directional)
}

func mergeRedactionRules(left, right RedactionRules) RedactionRules {
	return RedactionRules{
		Headers:         appendCopied(left.Headers, right.Headers),
		QueryParameters: appendCopied(left.QueryParameters, right.QueryParameters),
		Cookies:         appendCopied(left.Cookies, right.Cookies),
		JSONFields:      appendCopied(left.JSONFields, right.JSONFields),
		XMLElements:     appendCopied(left.XMLElements, right.XMLElements),
		BodyRedactors:   mergeBodyRedactors(left.BodyRedactors, right.BodyRedactors),
	}
}

func appendCopied(left, right []string) []string {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}

	out := make([]string, 0, len(left)+len(right))
	out = append(out, left...)
	out = append(out, right...)

	return out
}

func mergeBodyRedactors(left, right map[string]BodyRedactor) map[string]BodyRedactor {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}

	out := make(map[string]BodyRedactor, len(left)+len(right))
	for mediaType, redactor := range left {
		if normalized := baseMimeType(mediaType); normalized != "" && redactor != nil {
			out[normalized] = redactor
		}
	}

	for mediaType, redactor := range right {
		if normalized := baseMimeType(mediaType); normalized != "" && redactor != nil {
			out[normalized] = redactor
		}
	}

	return out
}
