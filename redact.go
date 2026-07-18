package recorder

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const redactedValue = "[REDACTED]"

// redactor applies the configured redaction rules while HAR structures are
// built. It never mutates live http.Request / http.Response objects — only
// the recorded copies. Immutable after construction, safe for concurrent use.
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

// redactJSONBody replaces configured field values without re-encoding the
// document. The input is validated first, then only the byte ranges occupied
// by matching values are spliced out. Every other byte — whitespace, key
// order, duplicate keys, number spelling and string escapes included — stays
// exactly as received. Invalid JSON passes through unchanged.
func (r *redactor) redactJSONBody(b []byte) []byte {
	if len(r.jsonFields) == 0 || !json.Valid(b) {
		return b
	}
	p := jsonSplicer{b: b, red: r}
	p.value(0, true)
	if len(p.edits) == 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	last := 0
	for _, edit := range p.edits {
		out = append(out, b[last:edit.start]...)
		out = append(out, `"[REDACTED]"`...)
		last = edit.end
	}
	return append(out, b[last:]...)
}

type jsonEdit struct{ start, end int }

type jsonSplicer struct {
	b     []byte
	red   *redactor
	edits []jsonEdit
}

func (p *jsonSplicer) whitespace(i int) int {
	for i < len(p.b) {
		switch p.b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// value returns the first byte after a JSON value. json.Valid has already
// proved the grammar, so this scanner only needs to find structural bounds.
func (p *jsonSplicer) value(i int, collect bool) int {
	i = p.whitespace(i)
	switch p.b[i] {
	case '{':
		return p.object(i, collect)
	case '[':
		return p.array(i, collect)
	case '"':
		return p.stringEnd(i)
	default:
		for i < len(p.b) && !strings.ContainsRune(" \t\r\n,]}", rune(p.b[i])) {
			i++
		}
		return i
	}
}

func (p *jsonSplicer) stringEnd(i int) int {
	for i++; ; i++ {
		if p.b[i] == '\\' {
			i++
		} else if p.b[i] == '"' {
			return i + 1
		}
	}
}

func (p *jsonSplicer) object(i int, collect bool) int {
	i = p.whitespace(i + 1)
	if p.b[i] == '}' {
		return i + 1
	}
	for {
		keyStart := i
		keyEnd := p.stringEnd(i)
		var key string
		if collect {
			_ = json.Unmarshal(p.b[keyStart:keyEnd], &key)
		}
		i = p.whitespace(keyEnd)
		i = p.whitespace(i + 1) // colon
		valueStart := i
		valueEnd := p.value(i, collect && !p.red.jsonFieldRedacted(key))
		if collect && p.red.jsonFieldRedacted(key) {
			p.edits = append(p.edits, jsonEdit{valueStart, valueEnd})
		}
		i = p.whitespace(valueEnd)
		if p.b[i] == '}' {
			return i + 1
		}
		i = p.whitespace(i + 1) // comma
	}
}

func (p *jsonSplicer) array(i int, collect bool) int {
	i = p.whitespace(i + 1)
	if p.b[i] == ']' {
		return i + 1
	}
	for {
		i = p.whitespace(p.value(i, collect))
		if p.b[i] == ']' {
			return i + 1
		}
		i = p.whitespace(i + 1) // comma
	}
}

func (r *redactor) xmlElementRedacted(local string) bool {
	_, ok := r.xmlElements[strings.ToLower(local)]
	return ok
}

// redactStructuredBody applies field-level redaction appropriate for the
// body's media type. It also recognizes well-formed JSON/XML sent under a
// generic or incorrect content type — common on raw-file hosts. Sniffing is
// gated by configured rules and full syntax validation, so arbitrary text or
// binary bodies pass through unchanged.
func (r *redactor) redactStructuredBody(mimeType string, b []byte) []byte {
	switch {
	case isJSONMime(mimeType):
		return r.redactJSONBody(b)
	case isXMLMime(mimeType):
		return r.redactXMLBody(b)
	case len(r.jsonFields) > 0 && json.Valid(b):
		return r.redactJSONBody(b)
	case len(r.xmlElements) > 0 && isWellFormedXML(b):
		return r.redactXMLBody(b)
	}
	return b
}

func isWellFormedXML(b []byte) bool {
	b = bytes.TrimSpace(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}))
	if len(b) == 0 || b[0] != '<' {
		return false
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	sawElement := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return sawElement
		}
		if err != nil {
			return false
		}
		if _, ok := tok.(xml.StartElement); ok {
			sawElement = true
		}
	}
}

// redactXMLBody replaces the character data inside matching elements with
// [REDACTED]. Elements match by case-insensitive local name, ignoring any
// namespace prefix, so "Password" covers <Password>, <wsse:Password>, and
// <ns2:PASSWORD> alike; the whole subtree of a matched element is redacted.
//
// The document is never re-encoded: matched text ranges are spliced out of
// the original bytes (namespace declarations, prefixes, formatting, and
// attribute layout all survive verbatim — essential for SOAP, which Go's
// encoding/xml cannot round-trip faithfully). Attribute values are NOT
// redacted; secrets carried in attributes need RedactErrorMessage-style
// custom handling upstream. On any parse problem the original bytes are
// returned unchanged, keeping the HAR structurally valid either way.
func (r *redactor) redactXMLBody(b []byte) []byte {
	if len(r.xmlElements) == 0 {
		return b
	}
	type span struct{ start, end int64 }
	var spans []span
	dec := xml.NewDecoder(bytes.NewReader(b))
	depth := 0 // > 0 while inside a matched element's subtree
	var prev int64
	for {
		tok, err := dec.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return b
		}
		off := dec.InputOffset()
		switch t := tok.(type) {
		case xml.StartElement:
			if depth > 0 || r.xmlElementRedacted(t.Name.Local) {
				depth++
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
		case xml.CharData:
			// Every byte belongs to some token, so prev (the end of the
			// previous token) is exactly this token's start; the range also
			// covers CDATA wrappers and entities.
			if depth > 0 && len(bytes.TrimSpace(t)) > 0 {
				spans = append(spans, span{start: prev, end: off})
			}
		}
		prev = off
	}
	if len(spans) == 0 {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b))
	var pos int64
	for _, s := range spans {
		out.Write(b[pos:s.start])
		out.WriteString(redactedValue)
		pos = s.end
	}
	out.Write(b[pos:])
	return out.Bytes()
}

// redactError filters an error message through the configured redactor.
func (r *redactor) redactError(msg string) string {
	if r.errFn != nil {
		return r.errFn(msg)
	}
	return msg
}
