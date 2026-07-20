package recorder

import (
	"bytes"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const redactedValue = "[REDACTED]"

// redactor applies the configured rules to recorded copies during body
// capture and entry construction. It never mutates live http.Request /
// http.Response objects. Immutable after construction, safe for concurrent
// use.
type redactor struct {
	headers     map[string]struct{}
	query       map[string]struct{}
	cookies     map[string]struct{}
	jsonFields  map[string]struct{}
	xmlElements map[string]struct{}
	errFn       func(string) string
}

func newRedactor(o *Options) *redactor {
	return &redactor{
		headers:     lowerSet(o.RedactHeaders),
		query:       lowerSet(o.RedactQueryParameters),
		cookies:     lowerSet(o.RedactCookies),
		jsonFields:  lowerSet(o.RedactJSONFields),
		xmlElements: lowerSet(o.RedactXMLElements),
		errFn:       o.RedactErrorMessage,
	}
}

func lowerSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[strings.ToLower(n)] = struct{}{}
	}
	return set
}

func (r *redactor) headerRedacted(name string) bool {
	_, ok := r.headers[strings.ToLower(name)]
	return ok
}

func (r *redactor) queryRedacted(name string) bool {
	_, ok := r.query[strings.ToLower(name)]
	return ok
}

// cookieRedacted reports whether a cookie value must be hidden: either the
// cookie name is listed, or the header that carried it (Cookie / Set-Cookie)
// is itself redacted.
func (r *redactor) cookieRedacted(name, carrierHeader string) bool {
	if _, ok := r.cookies[strings.ToLower(name)]; ok {
		return true
	}
	return r.headerRedacted(carrierHeader)
}

func (r *redactor) jsonFieldRedacted(name string) bool {
	_, ok := r.jsonFields[strings.ToLower(name)]
	return ok
}

// headerPairs converts a header map into sorted HAR pairs, applying header
// redaction. hostValue, when non-empty, is prepended as a "Host" pair (Go
// keeps the Host header outside http.Header).
func (r *redactor) headerPairs(h http.Header, hostValue string) []NameValuePair {
	pairs := make([]NameValuePair, 0, len(h)+1)
	if hostValue != "" {
		pairs = append(pairs, NameValuePair{Name: "Host", Value: hostValue})
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if r.headerRedacted(name) {
			pairs = append(pairs, NameValuePair{Name: name, Value: redactedValue})
			continue
		}
		for _, v := range h[name] {
			pairs = append(pairs, NameValuePair{Name: name, Value: v})
		}
	}
	return pairs
}

// redactPairs applies header redaction to an already-ordered pair list (the
// wire-order header fields reported by httptrace.WroteHeaderField).
func (r *redactor) redactPairs(pairs []NameValuePair) []NameValuePair {
	out := make([]NameValuePair, len(pairs))
	for i, p := range pairs {
		if r.headerRedacted(p.Name) {
			p.Value = redactedValue
		}
		out[i] = p
	}
	return out
}

// queryPairs parses a raw query string preserving parameter order and
// duplicates (url.Values would lose both), applying query redaction.
func (r *redactor) queryPairs(rawQuery string) []NameValuePair {
	pairs := []NameValuePair{}
	if rawQuery == "" {
		return pairs
	}
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		name := k
		if u, err := url.QueryUnescape(k); err == nil {
			name = u
		}
		value := v
		if u, err := url.QueryUnescape(v); err == nil {
			value = u
		}
		if r.queryRedacted(name) {
			value = redactedValue
		}
		pairs = append(pairs, NameValuePair{Name: name, Value: value})
	}
	return pairs
}

// redactURL renders a URL with redacted userinfo password and query values.
// The original URL is never mutated.
func (r *redactor) redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	cp := *u
	if cp.User != nil {
		if _, has := cp.User.Password(); has {
			cp.User = url.UserPassword(cp.User.Username(), redactedValue)
		}
	}
	if cp.RawQuery != "" && len(r.query) > 0 {
		var b strings.Builder
		for i, part := range strings.Split(cp.RawQuery, "&") {
			if i > 0 {
				b.WriteByte('&')
			}
			k, _, hasEq := strings.Cut(part, "=")
			name := k
			if uq, err := url.QueryUnescape(k); err == nil {
				name = uq
			}
			if hasEq && r.queryRedacted(name) {
				b.WriteString(k)
				b.WriteByte('=')
				b.WriteString(url.QueryEscape(redactedValue))
			} else {
				b.WriteString(part)
			}
		}
		cp.RawQuery = b.String()
	}
	return cp.String()
}

// redactURLString applies URL redaction to absolute or relative URL text.
// Malformed values are preserved because rewriting an unparseable Location
// header could misrepresent the response.
func (r *redactor) redactURLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return r.redactURL(u)
}

// urlBearingHeader reports header names whose values are URLs that can carry
// query parameters, so query redaction must reach them: Location and
// Content-Location on responses, and Referer on redirected requests — Go's
// client forwards the previous hop's URL including its query string, which
// would otherwise bypass RedactQueryParameters.
func urlBearingHeader(name string) bool {
	return strings.EqualFold(name, "Location") ||
		strings.EqualFold(name, "Content-Location") ||
		strings.EqualFold(name, "Referer")
}

// sanitizeURLHeaders applies URL redaction to URL-bearing header values in an
// already-redacted pair list (the list is owned by the caller and safe to
// mutate). Values that are fully redacted stay untouched.
func (r *redactor) sanitizeURLHeaders(pairs []NameValuePair) []NameValuePair {
	for i := range pairs {
		if pairs[i].Value != redactedValue && urlBearingHeader(pairs[i].Name) {
			pairs[i].Value = r.redactURLString(pairs[i].Value)
		}
	}
	return pairs
}

// responseHeaderPairs sanitizes URL-bearing values so query redaction cannot
// be bypassed through the response header list.
func (r *redactor) responseHeaderPairs(h http.Header) []NameValuePair {
	return r.sanitizeURLHeaders(r.headerPairs(h, ""))
}

// redactJSONBody uses the same bounded streaming transformer used by body
// capture. Non-redacted bytes are emitted exactly as received.
func (r *redactor) redactJSONBody(b []byte) []byte {
	if len(r.jsonFields) == 0 || len(b) == 0 {
		return b
	}
	var out bytes.Buffer
	s := newJSONStreamRedactor(&out, r.jsonFields)
	if _, err := s.Write(b); err != nil || s.Close() != nil {
		return []byte(`"[REDACTED]"`)
	}
	return out.Bytes()
}

func (r *redactor) xmlElementRedacted(local string) bool {
	_, ok := r.xmlElements[strings.ToLower(local)]
	return ok
}

// redactStructuredBody applies field-level redaction appropriate for the
// body's media type. Generic/incorrect media types use the same bounded
// prefix sniffer as live body capture.
func (r *redactor) redactStructuredBody(mimeType string, b []byte) []byte {
	var out bytes.Buffer
	s := newBodyStreamRedactor(&out, mimeType, r)
	if s == nil {
		return b
	}
	if _, err := s.Write(b); err != nil || s.Close() != nil {
		if isJSONMime(mimeType) {
			return []byte(`"[REDACTED]"`)
		}
		if isFormMime(mimeType) {
			return []byte(formRedactedValue)
		}
		if isMultipartFormMime(mimeType) {
			return []byte(redactedValue)
		}
		return []byte(redactedValue)
	}
	return out.Bytes()
}

// redactXMLBody replaces the subtree inside matching elements with
// [REDACTED]. Elements match by case-insensitive local name, ignoring any
// namespace prefix, so "Password" covers <Password>, <wsse:Password>, and
// <ns2:PASSWORD> alike.
//
// The document is never re-encoded. Attribute values are NOT redacted.
func (r *redactor) redactXMLBody(b []byte) []byte {
	if len(r.xmlElements) == 0 || len(b) == 0 {
		return b
	}
	var out bytes.Buffer
	s := newXMLStreamRedactor(&out, r.xmlElements)
	if _, err := s.Write(b); err != nil || s.Close() != nil {
		return []byte(redactedValue)
	}
	return out.Bytes()
}

// redactError filters an error message through the configured redactor.
func (r *redactor) redactError(msg string) string {
	if r.errFn != nil {
		return r.errFn(msg)
	}
	return msg
}

// traceEvents returns an owned copy whose details pass through the same
// application-supplied sanitizer used for recorded error strings. Raw
// httptrace callbacks can embed dial/TLS/write errors and must not bypass the
// central redaction policy merely because CaptureRawTrace is opt-in.
func (r *redactor) traceEvents(events []TraceEvent) []TraceEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]TraceEvent, len(events))
	copy(out, events)
	for i := range out {
		out[i].Detail = r.redactError(out[i].Detail)
	}
	return out
}
