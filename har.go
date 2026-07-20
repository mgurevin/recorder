package recorder

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	harVersion     = "1.2"
	creatorName    = "recorder"
	creatorVersion = "0.1.0"
	// harTimeFormat is ISO 8601 with millisecond precision as mandated by the
	// HAR 1.2 specification for startedDateTime.
	harTimeFormat = "2006-01-02T15:04:05.000Z07:00"
)

// HAR is the top-level HAR 1.2 document.
type HAR struct {
	Log *Log `json:"log"`
}

// Log is the HAR 1.2 log object.
type Log struct {
	Version string   `json:"version"`
	Creator *Creator `json:"creator"`
	Entries []*Entry `json:"entries"`
	Comment string   `json:"comment,omitempty"`
}

// Creator identifies the producing application.
type Creator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Entry is a single HAR entry: one physical HTTP exchange. Fields whose JSON
// name starts with "_" are application extensions per HAR 1.2; removing every
// "_" field leaves a valid plain HAR 1.2 entry.
//
// Entries produced by Transport are immutable snapshots: neither the
// Transport nor the built-in recorders mutate an Entry after it has been
// handed to Recorder.Record.
type Entry struct {
	StartedDateTime string    `json:"startedDateTime"`
	Time            float64   `json:"time"`
	Request         *Request  `json:"request"`
	Response        *Response `json:"response"`
	Cache           *Cache    `json:"cache"`
	Timings         *Timings  `json:"timings"`
	ServerIPAddress string    `json:"serverIPAddress,omitempty"`
	Connection      string    `json:"connection,omitempty"`
	Comment         string    `json:"comment,omitempty"`

	// Extensions.
	TraceID                  string                  `json:"_traceId,omitempty"`
	ExchangeID               string                  `json:"_exchangeId,omitempty"`
	RedirectIndex            *int                    `json:"_redirectIndex,omitempty"`
	State                    string                  `json:"_state,omitempty"`
	Error                    *ErrorInfo              `json:"_error,omitempty"`
	Network                  *NetworkInfo            `json:"_network,omitempty"`
	TLS                      *TLSInfo                `json:"_tls,omitempty"`
	Expect100                *Expect100Info          `json:"_expect100,omitempty"`
	Informational            []InformationalResponse `json:"_informational,omitempty"`
	RequestBody              *BodyInfo               `json:"_requestBody,omitempty"`
	ResponseBody             *BodyInfo               `json:"_responseBody,omitempty"`
	RequestTrailers          []NameValuePair         `json:"_requestTrailers,omitempty"`
	ResponseTrailers         []NameValuePair         `json:"_responseTrailers,omitempty"`
	RequestTransferEncoding  []string                `json:"_requestTransferEncoding,omitempty"`
	ResponseTransferEncoding []string                `json:"_responseTransferEncoding,omitempty"`
	RawTrace                 []TraceEvent            `json:"_trace,omitempty"`

	// started orders entries without re-parsing StartedDateTime.
	started time.Time
}

// StartTime returns the request start time used for ordering entries.
func (e *Entry) StartTime() time.Time {
	if !e.started.IsZero() {
		return e.started
	}
	if t, err := time.Parse(time.RFC3339, e.StartedDateTime); err == nil {
		return t
	}
	return time.Time{}
}

// NameValuePair is the HAR record for headers and query parameters.
type NameValuePair struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
}

// Cookie is the HAR cookie record.
type Cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Path     string `json:"path,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Expires  string `json:"expires,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
}

// Request is the HAR request record.
type Request struct {
	Method      string          `json:"method"`
	URL         string          `json:"url"`
	HTTPVersion string          `json:"httpVersion"`
	Cookies     []Cookie        `json:"cookies"`
	Headers     []NameValuePair `json:"headers"`
	QueryString []NameValuePair `json:"queryString"`
	PostData    *PostData       `json:"postData,omitempty"`
	// HeadersSize is -1: the exact serialized header bytes written to the
	// wire (including headers added internally by http.Transport) are not
	// observable at the RoundTripper layer.
	HeadersSize int64  `json:"headersSize"`
	BodySize    int64  `json:"bodySize"`
	Comment     string `json:"comment,omitempty"`
}

// PostData is the HAR postData record. "_encoding" is an extension marking
// Base64-encoded binary request bodies (plain HAR has no encoding field on
// postData).
type PostData struct {
	MimeType string      `json:"mimeType"`
	Params   []PostParam `json:"params,omitempty"`
	Text     string      `json:"text,omitempty"`
	Encoding string      `json:"_encoding,omitempty"`
	Comment  string      `json:"comment,omitempty"`
}

// PostParam is a single posted form parameter.
type PostParam struct {
	Name        string `json:"name"`
	Value       string `json:"value,omitempty"`
	FileName    string `json:"fileName,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

// Response is the HAR response record. For exchanges that failed before an
// HTTP response existed, Status is 0 and "_error" on the entry carries the
// failure detail.
type Response struct {
	Status      int             `json:"status"`
	StatusText  string          `json:"statusText"`
	HTTPVersion string          `json:"httpVersion"`
	Cookies     []Cookie        `json:"cookies"`
	Headers     []NameValuePair `json:"headers"`
	Content     *Content        `json:"content"`
	RedirectURL string          `json:"redirectURL"`
	// HeadersSize is -1: wire-level header size is not observable here.
	HeadersSize int64 `json:"headersSize"`
	// BodySize is the payload size received on the wire, or -1 when unknown
	// (body not fully read, or transparently decompressed by http.Transport
	// so the compressed wire size is no longer observable).
	BodySize int64  `json:"bodySize"`
	Comment  string `json:"comment,omitempty"`
}

// Content is the HAR content record. Size is the decoded content length when
// the decoded form is known (transparent gzip by http.Transport, or a
// configured ContentDecoder), otherwise the bytes the caller actually read.
// "_decoded" marks that Text/Size describe the decoded form rather than the
// raw wire bytes.
type Content struct {
	Size        int64  `json:"size"`
	Compression int64  `json:"compression,omitempty"`
	MimeType    string `json:"mimeType"`
	Text        string `json:"text,omitempty"`
	Encoding    string `json:"encoding,omitempty"`
	Comment     string `json:"comment,omitempty"`
	Decoded     bool   `json:"_decoded,omitempty"`
}

// Cache is the HAR cache record. This library performs no caching, so the
// object is intentionally empty (allowed by HAR 1.2).
type Cache struct{}

// Timings is the HAR timings record. All values are milliseconds; -1 means
// the phase did not apply or could not be measured (per HAR 1.2).
type Timings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
	SSL     float64 `json:"ssl"`
	Comment string  `json:"comment,omitempty"`
}

// NetworkInfo is the "_network" extension: connection-level facts observed
// through httptrace. These describe the physical connection this exchange
// used — when a proxy is in play (Proxy != ""), RemoteAddress, IPVersion and
// DNSAddresses therefore refer to the proxy, not the origin server: the
// origin is never observable client-side through a proxy. HTTP2 is true only
// when HTTP/2 was positively observed.
type NetworkInfo struct {
	DNSAddresses []string `json:"dnsAddresses,omitempty"`
	// DNSCoalesced marks that this exchange's DNS answer was shared with a
	// concurrent lookup for the same host (singleflight) rather than issued
	// on its own.
	DNSCoalesced     bool    `json:"dnsCoalesced,omitempty"`
	Network          string  `json:"network,omitempty"`
	LocalAddress     string  `json:"localAddress,omitempty"`
	RemoteAddress    string  `json:"remoteAddress,omitempty"`
	IPVersion        string  `json:"ipVersion,omitempty"`
	ConnectionReused bool    `json:"connectionReused"`
	WasIdle          bool    `json:"wasIdle"`
	IdleTimeMS       float64 `json:"idleTimeMs,omitempty"`
	// Proxy is the redacted proxy URL selected by a standard http.Transport.
	// Custom RoundTrippers may only expose the dialed host:port.
	Proxy string `json:"proxy,omitempty"`
	HTTP2 bool   `json:"http2"`
	// PutIdle reports whether the connection went back to the idle pool
	// after this exchange. Best effort: the pool return can race with entry
	// finalization, so absence means "not observed", not "did not happen".
	PutIdle *PutIdleInfo `json:"putIdle,omitempty"`
}

// PutIdleInfo is the structured outcome of the connection's return to the
// keep-alive pool.
type PutIdleInfo struct {
	Returned bool   `json:"returned"`
	Error    string `json:"error,omitempty"`
}

// Expect100Info describes an "Expect: 100-continue" handshake observed on
// this exchange.
type Expect100Info struct {
	// Waited reports that the transport paused before sending the body.
	Waited bool `json:"waited"`
	// ContinueReceived reports that the server sent 100 Continue.
	ContinueReceived bool `json:"continueReceived"`
	// WaitMS is the pause between waiting and the 100 arriving; omitted when
	// either side of the interval was not observed.
	WaitMS float64 `json:"waitMs,omitempty"`
}

// InformationalResponse is one 1xx interim response (100 Continue, 103 Early
// Hints, ...) observed before the final response. Headers are redacted with
// the same rules as final response headers.
type InformationalResponse struct {
	Status  int             `json:"status"`
	Headers []NameValuePair `json:"headers,omitempty"`
}

// TLSInfo is the "_tls" extension built from tls.ConnectionState.
type TLSInfo struct {
	Version            string     `json:"version"`
	CipherSuite        string     `json:"cipherSuite"`
	NegotiatedProtocol string     `json:"negotiatedProtocol,omitempty"`
	ServerName         string     `json:"serverName,omitempty"`
	HandshakeComplete  bool       `json:"handshakeComplete"`
	DidResume          bool       `json:"didResume"`
	OCSPStapled        bool       `json:"ocspStapled"`
	SCTCount           int        `json:"sctCount"`
	VerifiedChains     int        `json:"verifiedChains"`
	PeerCertificates   []CertInfo `json:"peerCertificates,omitempty"`
}

// CertInfo describes one peer certificate.
type CertInfo struct {
	Subject            string   `json:"subject"`
	Issuer             string   `json:"issuer"`
	SerialNumber       string   `json:"serialNumber"`
	DNSNames           []string `json:"dnsNames,omitempty"`
	IPAddresses        []string `json:"ipAddresses,omitempty"`
	NotBefore          string   `json:"notBefore"`
	NotAfter           string   `json:"notAfter"`
	PublicKeyAlgorithm string   `json:"publicKeyAlgorithm"`
	SignatureAlgorithm string   `json:"signatureAlgorithm"`
	SHA256Fingerprint  string   `json:"sha256Fingerprint"`
	RawDER             string   `json:"rawDER,omitempty"`
}

// BodyInfo is the "_requestBody" / "_responseBody" extension describing what
// happened to a body stream.
type BodyInfo struct {
	Present     bool `json:"present"`
	Complete    bool `json:"complete"`
	ClosedEarly bool `json:"closedEarly,omitempty"`
	Truncated   bool `json:"truncated,omitempty"`
	// CapturedBytes counts bytes stored for the HAR document.
	CapturedBytes int64 `json:"capturedBytes"`
	// TotalBytes counts every byte that actually flowed through the stream,
	// including bytes past the capture limit.
	TotalBytes int64 `json:"totalBytes"`
	// Hash covers TotalBytes worth of data and is only emitted when the
	// stream completed (a partial-stream hash would be misleading).
	Hash          string `json:"hash,omitempty"`
	HashAlgorithm string `json:"hashAlgorithm,omitempty"`
	ReadError     string `json:"readError,omitempty"`
	CloseError    string `json:"closeError,omitempty"`
	// Store is an external reference (e.g. a file path) when the BodyStore
	// keeps content out of memory. The referenced bytes are the captured
	// representation, which may already be decoded and/or redacted.
	Store string `json:"store,omitempty"`
}

// NewHAR builds a HAR document from finished entries. Entries are ordered by
// request start time (stable, so equal timestamps keep recording order). Only
// finalized entries ever reach a Recorder, so in-flight exchanges are by
// definition absent from the export.
func NewHAR(entries []*Entry) *HAR {
	sorted := make([]*Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].StartTime().Before(sorted[j].StartTime())
	})
	return &HAR{Log: &Log{
		Version: harVersion,
		Creator: &Creator{Name: creatorName, Version: creatorVersion},
		Entries: sorted,
	}}
}

// Write serializes the document as indented JSON. Field order follows struct
// declaration order and header lists are sorted, so output is deterministic
// for identical entries.
func (h *HAR) Write(w io.Writer) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("recorder: marshal HAR: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("recorder: write HAR: %w", err)
	}
	return nil
}

// baseMimeType strips parameters ("; charset=...") from a media type.
func baseMimeType(mimeType string) string {
	if parsed, _, err := mime.ParseMediaType(mimeType); err == nil {
		return parsed
	}
	mt, _, _ := strings.Cut(mimeType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// isTextualMime reports whether content of the given type is stored as plain
// text in HAR (subject to it also being valid UTF-8).
func isTextualMime(mimeType string) bool {
	mt := baseMimeType(mimeType)
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	if strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml") {
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/javascript",
		"application/ecmascript", "application/x-www-form-urlencoded",
		"application/x-ndjson", "application/xhtml+xml", "image/svg+xml",
		"multipart/form-data":
		return true
	}
	return false
}

func isJSONMime(mimeType string) bool {
	mt := baseMimeType(mimeType)
	return mt == "application/json" || strings.HasSuffix(mt, "+json") || mt == "application/x-ndjson"
}

// isXMLMime covers generic XML plus SOAP: SOAP 1.1 uses text/xml and
// SOAP 1.2 uses application/soap+xml (matched by the +xml suffix).
func isXMLMime(mimeType string) bool {
	mt := baseMimeType(mimeType)
	return mt == "application/xml" || mt == "text/xml" || strings.HasSuffix(mt, "+xml")
}

func isFormMime(mimeType string) bool {
	return baseMimeType(mimeType) == "application/x-www-form-urlencoded"
}

func isMultipartFormMime(mimeType string) bool {
	return baseMimeType(mimeType) == "multipart/form-data"
}

// contentText renders body bytes for HAR: plain text for textual UTF-8
// content, Base64 with encoding="base64" for everything else.
func contentText(mimeType string, b []byte) (text, encoding string) {
	if isTextualMime(mimeType) && utf8.Valid(b) {
		return string(b), ""
	}
	return base64.StdEncoding.EncodeToString(b), "base64"
}
