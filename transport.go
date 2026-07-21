// Package recorder records the full life cycle of net/http client calls —
// including calls that fail at the DNS, TCP, TLS or HTTP layer — and exports
// them as standard HAR 1.2 documents enriched with "_"-prefixed extension
// fields ("_error", "_network", "_tls", "_requestBody", "_responseBody",
// "_trace", ...). Stripping every extension field leaves a valid plain
// HAR 1.2 document.
//
// The guiding principle: record what was actually observable during the call,
// as accurately and structurally as possible, without ever changing the
// behavior the caller sees. Values that cannot be measured reliably (wire
// header sizes, compressed body sizes after transparent gzip decoding, timing
// phases that did not happen) are reported as -1 or omitted — never invented.
//
// Usage:
//
//	rec := recorder.NewMemoryRecorder()
//	client := &http.Client{
//		Transport: recorder.NewTransport(http.DefaultTransport, rec),
//	}
//	resp, err := client.Get("https://example.com/")
//	// ... consume resp.Body; the entry is finalized on EOF/Close ...
//	_ = rec.WriteHAR(os.Stdout)
//
// A response entry is not complete when RoundTrip returns: the body has not
// been read yet. The entry reaches the Recorder when the body hits EOF, is
// closed early, or fails — or immediately, when RoundTrip itself returns an
// error. A response body that is neither fully read nor closed (a caller
// bug) never finalizes; the library deliberately uses no finalizers.
package recorder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Exchange life cycle states, recorded under "_state". Only terminal states
// (completed, failed, closed_early) ever appear in exported entries, because
// entries are emitted exclusively at finalization.
const (
	StateCreated                 = "created"
	StateRequestStarted          = "request_started"
	StateRequestHeadersWritten   = "request_headers_written"
	StateRequestBodyStreaming    = "request_body_streaming"
	StateResponseHeadersReceived = "response_headers_received"
	StateResponseBodyStreaming   = "response_body_streaming"
	StateCompleted               = "completed"
	StateFailed                  = "failed"
	StateClosedEarly             = "closed_early"
)

// Transport is an http.RoundTripper that records every exchange passing
// through it. It wraps a base RoundTripper (http.DefaultTransport when Base
// is nil) and never alters the request, response, or error the caller sees.
//
// Transport is safe for concurrent use by multiple goroutines provided its
// fields are not mutated after the first request. Prefer NewTransport, which
// also applies DefaultOptions; a zero-value literal works but captures
// no entries until a Recorder or OnEntryCompleted callback is set, and
// zero-value Options retain only core lifecycle/body accounting. When a
// standard *http.Transport has a Proxy callback, NewTransport clones it once
// so the selected proxy URL can be
// observed without invoking that callback twice; configure the base before
// passing it in and close idle connections through this Transport or its
// http.Client.
type Transport struct {
	// Base is the wrapped RoundTripper; nil means http.DefaultTransport.
	Base http.RoundTripper
	// Recorder receives finalized entries; nil disables recording (the
	// OnEntryCompleted callback still fires).
	Recorder Recorder
	// Options configures capturing and redaction.
	Options Options

	initOnce      sync.Once
	red           *redactor
	respRed       *redactor
	store         BodyStore
	effectiveBase http.RoundTripper
}

// NewTransport builds a Transport wrapping base. rec may be nil, in which
// case a fresh MemoryRecorder is installed (accessible via the Recorder
// field). Options start from DefaultOptions and are adjusted by opts.
func NewTransport(base http.RoundTripper, rec Recorder, opts ...Option) *Transport {
	o := DefaultOptions()

	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	if rec == nil {
		rec = NewMemoryRecorder()
	}

	t := &Transport{Base: base, Recorder: rec, Options: o}
	t.init()

	return t
}

func (t *Transport) init() {
	t.initOnce.Do(func() {
		t.red = newRedactor(&t.Options)
		t.respRed = newRedactorWithRules(&t.Options,
			effectiveRedactionRules(t.Options.Redaction.Common, t.Options.Redaction.Response))

		t.store = t.Options.BodyStore
		if t.store == nil {
			t.store = MemoryBodyStore{}
		}

		base := t.Base
		if base == nil {
			base = http.DefaultTransport
		}

		if standard, ok := base.(*http.Transport); ok && standard.Proxy != nil {
			clone := standard.Clone()
			proxyFunc := clone.Proxy
			clone.Proxy = func(req *http.Request) (*url.URL, error) {
				proxyURL, err := proxyFunc(req)
				if observation := proxyObservationFromContext(req.Context()); observation != nil {
					observation.set(proxyURL)
				}

				return proxyURL, err
			}
			t.effectiveBase = clone
		} else {
			t.effectiveBase = base
		}
	})
}

func (t *Transport) base() http.RoundTripper {
	t.init()
	return t.effectiveBase
}

type proxyObservationKey struct{}

type proxyObservation struct {
	mu  sync.Mutex
	url *url.URL
}

func (o *proxyObservation) set(proxyURL *url.URL) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if proxyURL == nil {
		o.url = nil
		return
	}

	cp := *proxyURL
	o.url = &cp
}

func (o *proxyObservation) get() *url.URL {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.url == nil {
		return nil
	}

	cp := *o.url

	return &cp
}

func proxyObservationFromContext(ctx context.Context) *proxyObservation {
	observation, _ := ctx.Value(proxyObservationKey{}).(*proxyObservation)
	return observation
}

// CloseIdleConnections forwards to the wrapped transport when it supports it,
// keeping http.Client.CloseIdleConnections working through the wrapper.
func (t *Transport) CloseIdleConnections() {
	if ci, ok := t.base().(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper. The caller's request object is
// never mutated: recording hooks are attached to a clone. The returned
// response and error are exactly what the base transport produced, except
// that resp.Body is wrapped to observe the caller's reads.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.init()
	ex := t.newExchange(req)

	var err error

	ex.reqDecision, err = decideBodyCapture(req.Context(), t.Options.BodyCapturePolicy,
		requestCaptureMeta(req, ex.traceID, ex.redirectIndex),
		BodyCaptureDecision{
			Capture:      t.Options.CaptureRequestBody,
			Embed:        t.Options.EmbedBodies,
			Hash:         t.Options.HashBodies,
			MaxBodyBytes: t.Options.MaxRequestBodyBytes,
		})
	if err != nil {
		t.internalError(err)
	}

	proxySeen := &proxyObservation{}
	ctx := context.WithValue(req.Context(), proxyObservationKey{}, proxySeen)
	ctx = httptrace.WithClientTrace(ctx, ex.trace.clientTrace())
	creq := req.Clone(ctx)
	ex.req = creq

	if creq.Body != nil && creq.Body != http.NoBody {
		ex.reqCap = t.newCapture(ctx, ex.id, "request", creq.Header.Get("Content-Type"), creq.Header.Get("Content-Encoding"),
			ex.reqDecision, creq.ContentLength, ex.red)
		ex.reqCap.setExpected(creq.ContentLength)

		creq.Body = &requestBodyRecorder{rc: creq.Body, bc: ex.reqCap, ex: ex}
		if orig := creq.GetBody; orig != nil {
			bc, exRef := ex.reqCap, ex
			creq.GetBody = func() (io.ReadCloser, error) {
				rc, err := orig()
				if err != nil {
					return nil, err
				}
				// The transport is replaying the body (internal retry):
				// restart the capture so the record reflects the bytes of
				// the attempt that actually went out.
				bc.reset()

				return &requestBodyRecorder{rc: rc, bc: bc, ex: exRef}, nil
			}
		}
	}

	ex.setState(StateRequestStarted)

	resp, err := t.base().RoundTrip(creq)

	ex.detectProxy(proxySeen.get(), ex.trace.dialTarget())

	if err != nil {
		ex.finalizeTransportError(err)
		return resp, err
	}

	ex.onResponse(resp)

	ex.respDecision, err = decideBodyCapture(ex.ctx, t.Options.BodyCapturePolicy,
		responseCaptureMeta(creq, resp, ex.traceID, ex.redirectIndex),
		BodyCaptureDecision{
			Capture:      t.Options.CaptureResponseBody,
			Embed:        t.Options.EmbedBodies,
			Hash:         t.Options.HashBodies,
			MaxBodyBytes: t.Options.MaxResponseBodyBytes,
		})
	if err != nil {
		t.internalError(err)
	}

	if resp.Body == nil {
		// RoundTripper contract requires a non-nil body, but be tolerant of
		// sloppy custom transports.
		resp.Body = http.NoBody
	}

	ex.respCap = t.newCapture(ctx, ex.id, "response", resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"),
		ex.respDecision, resp.ContentLength, ex.respRed)
	resp.Body = &responseBodyRecorder{rc: resp.Body, bc: ex.respCap, ex: ex}

	if responseHasNoBody(creq, resp) {
		// Nothing will ever be read; finalize now so callers that (legally)
		// never touch the empty body still produce an entry.
		ex.respCap.finishComplete()
		ex.finalizeComplete()
	}

	return resp, nil
}

// detectProxy records the redacted URL selected by a standard http.Transport
// without evaluating its potentially stateful Proxy callback a second time.
// Custom RoundTrippers cannot expose that selection; for those, a differing
// dial target remains a host:port fallback.
func (ex *exchange) detectProxy(proxyURL *url.URL, dialed string) {
	if proxyURL != nil {
		ex.hasProxy = true
		ex.proxyURL = ex.red.redactURL(proxyURL)

		return
	}

	if dialed == "" || ex.req == nil || ex.req.URL == nil {
		return
	}

	originPort := ex.req.URL.Port()
	if originPort == "" {
		switch ex.req.URL.Scheme {
		case "http":
			originPort = "80"

		case "https":
			originPort = "443"
		}
	}

	originAddr := net.JoinHostPort(ex.req.URL.Hostname(), originPort)
	if !strings.EqualFold(dialed, originAddr) {
		ex.hasProxy = true
		ex.proxyURL = dialed
	}
}

func (t *Transport) newCapture(ctx context.Context, exchangeID, direction, contentType, contentEncoding string,
	decision BodyCaptureDecision, contentLength int64, red *redactor,
) *bodyCapture {
	// Derive the store's pre-allocation hint from Content-Length: never
	// beyond what the capture limit allows, never negative, and left at 0
	// (unknown) when no length was announced. The hint is advisory only —
	// a stream that disagrees with it just grows or stops normally.
	sizeHint := contentLength
	if sizeHint < 0 {
		sizeHint = 0
	}

	if decision.MaxBodyBytes > 0 && sizeHint > decision.MaxBodyBytes {
		sizeHint = decision.MaxBodyBytes
	}

	meta := BodyMetadata{
		ExchangeID:  exchangeID,
		Direction:   direction,
		ContentType: contentType,
		SizeHint:    sizeHint,
	}

	var decoder ContentDecoder

	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	if enc != "" && enc != "identity" && !strings.Contains(enc, ",") {
		decoder = t.Options.ContentDecoders[enc]
	}

	if decision.RedactorOverride != nil {
		red = red.withBodyRedactor(contentType, decision.RedactorOverride)
	}

	return newBodyCapture(ctx, t.store, meta,
		contentEncoding, decision.Capture, decision.MaxBodyBytes, t.Options.BodyHashAlgorithm, decision.Hash, red, decoder, t.internalError)
}

// internalError applies the configured internal error policy. It never
// panics and never touches the HTTP flow.
func (t *Transport) internalError(err error) {
	// Error reporting is deliberately best-effort. Both hooks are supplied by
	// callers and must not be able to turn a recorder failure into an HTTP
	// failure of their own.
	if t.Options.InternalErrorMode == InternalErrorLog {
		if t.Options.Logf != nil {
			callSafely(func() { t.Options.Logf("recorder: %v", err) })
		} else {
			log.Printf("recorder: %v", err)
		}
	}

	if t.Options.OnInternalError != nil {
		callSafely(func() { t.Options.OnInternalError(err) })
	}
}

func callSafely(fn func()) {
	defer func() { _ = recover() }()

	fn()
}

// responseHasNoBody reports whether the response can never carry body bytes,
// so the exchange can be finalized at RoundTrip time.
func responseHasNoBody(req *http.Request, resp *http.Response) bool {
	if req.Method == http.MethodHead {
		return true
	}

	if resp.StatusCode < 200 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return true
	}
	// ContentLength 0 means an explicit zero-length body (unknown is -1).
	return resp.ContentLength == 0 && len(resp.TransferEncoding) == 0
}

// respSnapshot freezes the response metadata at RoundTrip time so later
// header mutations by the caller cannot race with entry building.
type respSnapshot struct {
	present          bool
	status           int
	statusText       string
	proto            string
	headers          http.Header
	cookies          []*http.Cookie
	contentLength    int64
	transferEncoding []string
	uncompressed     bool
	tls              *tls.ConnectionState
}

// exchange tracks one physical HTTP exchange from RoundTrip entry to
// finalization.
type exchange struct {
	t       *Transport
	red     *redactor
	respRed *redactor
	audit   *redactionAudit
	ctx     context.Context

	id            string
	traceID       string
	redirectIndex int
	hasTraceState bool
	hasProxy      bool
	proxyURL      string

	start time.Time
	trace *traceCollector

	req     *http.Request // the clone handed to the base transport
	reqCap  *bodyCapture
	respCap *bodyCapture

	reqDecision  BodyCaptureDecision
	respDecision BodyCaptureDecision

	mu       sync.Mutex
	state    string
	done     bool
	resp     *http.Response
	respSnap respSnapshot

	finalizeOnce sync.Once
	finish       time.Time
}

func (t *Transport) newExchange(req *http.Request) *exchange {
	audit := &redactionAudit{}
	hints := requestRedactionFromContext(req.Context())
	ex := &exchange{
		t: t,
		red: t.red.withRules(
			effectiveRedactionRules(hints.Common, hints.Request),
		).withAudit(audit, RequestBody),
		respRed: t.respRed.withRules(
			effectiveRedactionRules(hints.Common, hints.Response),
		).withAudit(audit, ResponseBody),
		audit: audit,
		ctx:   req.Context(),
		id:    newID(),
		start: time.Now(),
		trace: newTraceCollector(t.Options.CaptureRawTrace),
		state: StateCreated,
	}

	ex.trace.notify = ex.setState
	if ts := traceStateFromContext(req.Context()); ts != nil {
		ex.traceID = ts.id
		ex.redirectIndex = int(ts.seq.Add(1) - 1)
		ex.hasTraceState = true
	} else {
		ex.traceID = newID()
	}

	return ex
}

// setState advances the life cycle state; terminal states set by finalization
// are never overwritten.
func (ex *exchange) setState(s string) {
	ex.mu.Lock()
	defer ex.mu.Unlock()

	if !ex.done {
		ex.state = s
	}
}

func (ex *exchange) markDone(state string) {
	ex.mu.Lock()
	defer ex.mu.Unlock()

	ex.state = state
	ex.done = true
}

// onResponse snapshots response metadata the moment the base transport
// returns it.
func (ex *exchange) onResponse(resp *http.Response) {
	ex.mu.Lock()
	defer ex.mu.Unlock()

	ex.state = StateResponseHeadersReceived
	ex.resp = resp
	ex.respSnap = respSnapshot{
		present:          true,
		status:           resp.StatusCode,
		statusText:       statusText(resp),
		proto:            resp.Proto,
		headers:          resp.Header.Clone(),
		cookies:          resp.Cookies(),
		contentLength:    resp.ContentLength,
		transferEncoding: append([]string(nil), resp.TransferEncoding...),
		uncompressed:     resp.Uncompressed,
		tls:              resp.TLS,
	}
}

// contextErr returns the request context's Err(), nil while it is live.
func (ex *exchange) contextErr() error {
	if ex.ctx == nil {
		return nil
	}

	return ex.ctx.Err()
}

// contextCause returns context.Cause when the request context has been
// canceled, nil otherwise.
func (ex *exchange) contextCause() error {
	if ex.ctx == nil || ex.ctx.Err() == nil {
		return nil
	}

	return context.Cause(ex.ctx)
}

// finalizeTransportError finalizes an exchange whose RoundTrip failed without
// producing a response. Recorder failures are reported separately and never
// replace the transport error returned to the caller.
func (ex *exchange) finalizeTransportError(err error) {
	ex.finalizeOnce.Do(func() {
		ex.finish = time.Now()
		ex.markDone(StateFailed)
		v := ex.trace.view()

		var reqBodyErr error
		if ex.reqCap != nil {
			reqBodyErr = ex.reqCap.readError()
		}

		ctxErr := ex.contextErr()
		phase := classifyPhase(err, v, ex.hasProxy, false, ctxErr != nil, reqBodyErr)
		info := newErrorInfo(err, phase, ex.red, ctxErr, ex.contextCause())
		ex.emit(info)
	})
}

// finalizeComplete finalizes after the response body reached EOF (or was
// known-empty at RoundTrip time).
func (ex *exchange) finalizeComplete() {
	ex.finalizeOnce.Do(func() {
		ex.finish = time.Now()
		ex.markDone(StateCompleted)
		ex.emit(nil)
	})
}

// finalizeBodyReadError finalizes after a response body read failed.
func (ex *exchange) finalizeBodyReadError(err error) {
	ex.finalizeOnce.Do(func() {
		ex.finish = time.Now()
		ex.markDone(StateFailed)
		info := newErrorInfo(err, PhaseReadResponseBody, ex.red, ex.contextErr(), ex.contextCause())
		ex.emit(info)
	})
}

// finalizeClosed finalizes after the caller closed the body before EOF.
func (ex *exchange) finalizeClosed() {
	ex.finalizeOnce.Do(func() {
		ex.finish = time.Now()

		state := StateClosedEarly
		if ex.respCap != nil && ex.respCap.isComplete() {
			state = StateCompleted
		}

		ex.markDone(state)
		ex.emit(nil)
	})
}

// emit builds the entry and hands it to the recorder and callback. Panics in
// recorder code are contained so they cannot break the HTTP call.
func (ex *exchange) emit(errInfo *ErrorInfo) {
	defer func() {
		if p := recover(); p != nil {
			ex.t.internalError(fmt.Errorf("recorder: panic while recording entry: %v", p))
		}
	}()

	entry := ex.buildEntry(errInfo)
	if ex.t.Recorder != nil {
		ex.t.Recorder.Record(entry)
	}

	if ex.t.Options.OnEntryCompleted != nil {
		ex.t.Options.OnEntryCompleted(ex.ctx, entry)
	}
}

// buildEntry assembles the immutable HAR entry snapshot.
func (ex *exchange) buildEntry(errInfo *ErrorInfo) *Entry {
	v := ex.trace.view()

	ex.mu.Lock()
	state := ex.state
	snap := ex.respSnap

	var trailers http.Header
	if ex.resp != nil && len(ex.resp.Trailer) > 0 {
		trailers = ex.resp.Trailer.Clone()
	}
	ex.mu.Unlock()

	proto := ex.httpVersion(v, snap)

	e := &Entry{
		StartedDateTime: ex.start.UTC().Format(harTimeFormat),
		Time:            durMS(ex.finish.Sub(ex.start)),
		Cache:           &Cache{},
		TraceID:         ex.traceID,
		ExchangeID:      ex.id,
		State:           state,
		Error:           errInfo,
		started:         ex.start,
	}
	if ex.hasTraceState {
		idx := ex.redirectIndex
		e.RedirectIndex = &idx
	}

	e.Request = ex.buildRequest(v, proto)

	e.Response = ex.buildResponse(snap)
	if !v.wait100.IsZero() || !v.got100.IsZero() {
		e.Expect100 = &Expect100Info{
			Waited:           !v.wait100.IsZero(),
			ContinueReceived: !v.got100.IsZero(),
		}
		if ms := msBetween(v.wait100, v.got100); ms >= 0 {
			e.Expect100.WaitMS = ms
		}
	}

	for _, ir := range v.info1xx {
		rec := InformationalResponse{Status: ir.code}
		if ex.t.Options.CaptureHeaders && len(ir.header) > 0 {
			rec.Headers = ex.respRed.responseHeaderPairs(ir.header)
		}

		e.Informational = append(e.Informational, rec)
	}

	e.Timings = computeTimings(v, ex.start, ex.finish)
	// serverIPAddress means the origin server's IP (HAR: result of DNS
	// resolution). Through a proxy the TCP peer is the proxy and the origin
	// IP is never observable client-side, so the field is omitted (the proxy
	// address stays available under "_network"). It is also only written
	// when the peer address really is an IP.
	if v.remoteAddr != "" && !ex.hasProxy {
		if host, _, err := net.SplitHostPort(v.remoteAddr); err == nil {
			if ip := net.ParseIP(host); ip != nil {
				e.ServerIPAddress = ip.String()
			}
		}
	}

	if v.localAddr != "" {
		// HAR "connection": a unique ID of the underlying connection; the
		// local port is unique per live connection, matching browser usage.
		if _, port, err := net.SplitHostPort(v.localAddr); err == nil {
			e.Connection = port
		}
	}

	e.Network = ex.buildNetwork(v, proto)
	if ex.t.Options.CaptureTLS {
		e.TLS = ex.buildTLS(v, snap)
	}

	e.RequestBody = ex.reqCap.info(ex.red)

	e.ResponseBody = ex.respCap.info(ex.respRed)
	if ex.t.Options.CaptureRawTrace {
		e.RawTrace = ex.red.traceEvents(v.raw)
	}

	if ex.t.Options.CaptureHeaders {
		if len(ex.req.Trailer) > 0 {
			e.RequestTrailers = ex.red.headerPairs(ex.req.Trailer, "")
		}

		if len(trailers) > 0 {
			e.ResponseTrailers = ex.respRed.headerPairs(trailers, "")
		}
	}

	if len(ex.req.TransferEncoding) > 0 {
		e.RequestTransferEncoding = append([]string(nil), ex.req.TransferEncoding...)
	}

	if len(snap.transferEncoding) > 0 {
		e.ResponseTransferEncoding = snap.transferEncoding
	}

	e.Redaction = ex.audit.snapshot()
	for index, failure := range ex.audit.protectionFailures() {
		if failure.first == nil || failure.count == 0 {
			continue
		}

		direction := "request"
		if index == 1 {
			direction = "response"
		}

		ex.t.internalError(fmt.Errorf("recorder: %s sensitive value protection failed for %d value(s): %w",
			direction, failure.count, failure.first))
	}

	return e
}

// httpVersion returns the HTTP version actually observed for this exchange,
// or "" when it cannot be known. Never guessed:
//
//   - a response exists        -> its negotiated protocol (authoritative)
//   - request headers written  -> the TLS ALPN result when a handshake
//     completed in this exchange; for cleartext through the standard
//     *http.Transport, HTTP/1.1 (the stdlib never speaks h2c)
//   - anything earlier (DNS/connect/TLS failures) -> "" — no request line
//     ever reached the wire, so it has no version
func (ex *exchange) httpVersion(v traceView, snap respSnapshot) string {
	if snap.present && snap.proto != "" {
		return snap.proto
	}

	if v.wroteHeaders.IsZero() {
		return ""
	}

	if st := v.tlsState; st != nil && st.HandshakeComplete {
		if st.NegotiatedProtocol == "h2" {
			return "HTTP/2.0"
		}

		return "HTTP/1.1"
	}

	if _, ok := ex.t.base().(*http.Transport); ok &&
		ex.req.URL != nil && ex.req.URL.Scheme == "http" {
		return "HTTP/1.1"
	}
	// Reused TLS connections (no handshake event) or custom transports:
	// the protocol is not observable.
	return ""
}

func (ex *exchange) buildRequest(v traceView, effectiveProto string) *Request {
	req := ex.req

	r := &Request{
		Method:      req.Method,
		URL:         ex.red.redactURL(req.URL),
		HTTPVersion: effectiveProto,
		Cookies:     []Cookie{},
		Headers:     []NameValuePair{},
		QueryString: []NameValuePair{},
		HeadersSize: -1,
		BodySize:    0,
	}
	if r.Method == "" {
		r.Method = http.MethodGet
	}

	if req.URL != nil {
		r.QueryString = ex.red.queryPairs(req.URL.RawQuery)
	}

	if ex.t.Options.CaptureHeaders {
		if len(v.wroteHeaderFields) > 0 {
			// Prefer the headers the transport actually wrote to the wire
			// (httptrace.WroteHeaderField): they include transport-added
			// fields (User-Agent, Accept-Encoding, Host / :authority) in
			// wire order. Redaction applies the same way, including URL
			// sanitization of Referer on redirect hops.
			r.Headers = ex.red.sanitizeURLHeaders(ex.red.redactPairs(v.wroteHeaderFields))
		} else {
			// Nothing was written (failure before the request line, or a
			// custom transport without trace support): fall back to the
			// caller-provided header snapshot plus the Host header.
			host := req.Host
			if host == "" && req.URL != nil {
				host = req.URL.Host
			}

			r.Headers = ex.red.sanitizeURLHeaders(ex.red.headerPairs(req.Header, host))
		}
	}

	if ex.t.Options.CaptureCookies {
		for _, c := range req.Cookies() {
			val := c.Value
			if ex.red.cookieRedacted(c.Name, "cookie") {
				if val != redactedValue {
					ex.red.recordCookieRedaction()
				}

				val = ex.red.protectString(val)
			}

			r.Cookies = append(r.Cookies, Cookie{Name: c.Name, Value: val})
		}
	}

	if ex.reqCap != nil {
		r.BodySize = ex.reqCap.totalBytes()
		if ex.reqDecision.Capture && ex.reqDecision.Embed {
			if b := ex.reqCap.bytes(); len(b) > 0 {
				mimeType := req.Header.Get("Content-Type")
				if mimeType == "" {
					mimeType = "application/octet-stream"
				}

				whole := ex.reqCap.isComplete() && !ex.reqCap.isTruncated()
				r.PostData = ex.buildPostData(mimeType, b, whole, ex.reqCap.isStoredRedacted())
			}
		}
	}

	return r
}

func (ex *exchange) buildPostData(mimeType string, b []byte, whole, storedRedacted bool) *PostData {
	pd := &PostData{MimeType: mimeType}
	if whole && !storedRedacted {
		b = ex.red.redactStructuredBody(mimeType, b)
	}

	pd.Text, pd.Encoding = contentText(mimeType, b)
	if whole && isFormMime(mimeType) {
		// Form fields reuse the query-parameter redaction rules.
		for _, p := range ex.red.queryPairs(string(b)) {
			pd.Params = append(pd.Params, PostParam{Name: p.Name, Value: p.Value})
		}
	} else if whole && isMultipartFormMime(mimeType) {
		pd.Params = multipartPostParams(mimeType, b)
	}

	return pd
}

func multipartPostParams(mimeType string, b []byte) []PostParam {
	_, params, err := mime.ParseMediaType(mimeType)
	if err != nil || params["boundary"] == "" {
		return nil
	}

	mr := multipart.NewReader(bytes.NewReader(b), params["boundary"])

	var out []PostParam

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return out
		}

		if err != nil || part.FormName() == "" {
			return nil
		}

		p := PostParam{
			Name:        part.FormName(),
			FileName:    part.FileName(),
			ContentType: part.Header.Get("Content-Type"),
		}

		content, err := io.ReadAll(part)
		if err != nil {
			return nil
		}

		if p.FileName == "" {
			p.Value = string(content)
		}

		out = append(out, p)
	}
}

func (ex *exchange) buildResponse(snap respSnapshot) *Response {
	if !snap.present {
		// No HTTP response was produced; status 0 keeps the document valid
		// HAR 1.2 while "_error" carries the failure detail.
		return &Response{
			Status:      0,
			StatusText:  "",
			HTTPVersion: "",
			Cookies:     []Cookie{},
			Headers:     []NameValuePair{},
			Content:     &Content{Size: 0, MimeType: "x-unknown"},
			RedirectURL: "",
			HeadersSize: -1,
			BodySize:    -1,
		}
	}

	r := &Response{
		Status:      snap.status,
		StatusText:  snap.statusText,
		HTTPVersion: snap.proto,
		Cookies:     []Cookie{},
		Headers:     []NameValuePair{},
		RedirectURL: ex.respRed.redactURLString(snap.headers.Get("Location")),
		HeadersSize: -1,
		BodySize:    -1,
	}
	if ex.t.Options.CaptureHeaders {
		r.Headers = ex.respRed.responseHeaderPairs(snap.headers)
	}

	if ex.t.Options.CaptureCookies {
		for _, c := range snap.cookies {
			val := c.Value
			if ex.respRed.cookieRedacted(c.Name, "set-cookie") {
				if val != redactedValue {
					ex.respRed.recordCookieRedaction()
				}

				val = ex.respRed.protectString(val)
			}

			hc := Cookie{
				Name:     c.Name,
				Value:    val,
				Path:     c.Path,
				Domain:   c.Domain,
				HTTPOnly: c.HttpOnly,
				Secure:   c.Secure,
			}
			if !c.Expires.IsZero() {
				hc.Expires = c.Expires.UTC().Format(time.RFC3339)
			}

			r.Cookies = append(r.Cookies, hc)
		}
	}

	mimeType := snap.headers.Get("Content-Type")
	if mimeType == "" {
		mimeType = "x-unknown"
	}

	content := &Content{Size: 0, MimeType: mimeType}
	if ex.respCap != nil {
		content.Size = ex.respCap.totalBytes()

		complete := ex.respCap.isComplete()
		if snap.uncompressed {
			// http.Transport decompressed the stream transparently: the
			// caller-visible byte count is the decoded size, and the
			// compressed wire size is no longer observable -> BodySize -1.
			content.Decoded = true
		} else if complete {
			// Identity encoding, fully read: caller bytes == wire payload.
			r.BodySize = content.Size
		}

		if ex.respDecision.Capture && ex.respDecision.Embed {
			if b := ex.respCap.bytes(); len(b) > 0 {
				whole := complete && !ex.respCap.isTruncated()
				if whole && !snap.uncompressed {
					if ex.respCap.isStoredDecoded() {
						content.Decoded = true

						content.Size = int64(len(b))
						if r.BodySize >= 0 {
							content.Compression = content.Size - r.BodySize
						}
					} else {
						// The transport did not decompress; store the decoded
						// form when a decoder is registered for the encoding.
						// bodySize, hash and stream counters keep the wire view.
						if decoded, ok := ex.decodeBody(snap.headers.Get("Content-Encoding"), b); ok {
							b = decoded
							content.Decoded = true

							content.Size = int64(len(decoded))
							if r.BodySize >= 0 {
								// HAR compression = bytes saved on the wire; can
								// be negative when encoding expanded the content.
								content.Compression = content.Size - r.BodySize
							}
						}
					}
				}

				if whole && !ex.respCap.isStoredRedacted() {
					b = ex.respRed.redactStructuredBody(mimeType, b)
				}

				content.Text, content.Encoding = contentText(mimeType, b)
			}
		}
	}

	r.Content = content

	return r
}

// decodeBody decodes fully captured compressed content for HAR embedding when
// the stored representation was not already decoded by streaming structured
// redaction. It refuses multi-step encodings ("gzip, br"), reports decoder
// failures as internal errors (the record then keeps the captured wire
// representation), and abandons decoding when the decoded form would exceed
// MaxResponseBodyBytes — a compression bomb must not inflate the recorder's
// memory beyond the configured capture budget.
func (ex *exchange) decodeBody(encoding string, b []byte) ([]byte, bool) {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	if enc == "" || enc == "identity" || strings.Contains(enc, ",") {
		return nil, false
	}

	dec := ex.t.Options.ContentDecoders[enc]
	if dec == nil {
		return nil, false
	}

	rc, err := dec(bytes.NewReader(b))
	if err != nil {
		ex.t.internalError(fmt.Errorf("recorder: open %s decoder: %w", enc, err))
		return nil, false
	}

	defer func() { _ = rc.Close() }()

	limit := ex.respDecision.MaxBodyBytes

	var buf bytes.Buffer
	if limit > 0 {
		n, err := io.Copy(&buf, io.LimitReader(rc, limit+1))
		if err != nil {
			ex.t.internalError(fmt.Errorf("recorder: decode %s content: %w", enc, err))
			return nil, false
		}

		if n > limit {
			return nil, false
		}
	} else if _, err := io.Copy(&buf, rc); err != nil {
		ex.t.internalError(fmt.Errorf("recorder: decode %s content: %w", enc, err))
		return nil, false
	}

	return buf.Bytes(), true
}

func (ex *exchange) buildNetwork(v traceView, proto string) *NetworkInfo {
	n := &NetworkInfo{
		DNSAddresses:     v.dnsAddrs,
		DNSCoalesced:     v.dnsCoalesced,
		Network:          v.network,
		LocalAddress:     v.localAddr,
		RemoteAddress:    v.remoteAddr,
		ConnectionReused: v.reused,
		WasIdle:          v.wasIdle,
		Proxy:            ex.proxyURL,
		HTTP2:            strings.HasPrefix(proto, "HTTP/2"),
	}
	if v.wasIdle {
		n.IdleTimeMS = durMS(v.idleTime)
	}

	if v.putIdle != nil {
		pi := &PutIdleInfo{Returned: v.putIdle.returned}
		if v.putIdle.err != nil {
			pi.Error = ex.red.redactError(v.putIdle.err.Error())
		}

		n.PutIdle = pi
	}

	if host, _, err := net.SplitHostPort(v.remoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			if ip.To4() != nil {
				n.IPVersion = "ipv4"
			} else {
				n.IPVersion = "ipv6"
			}
		}
	}

	return n
}

func (ex *exchange) buildTLS(v traceView, snap respSnapshot) *TLSInfo {
	st := v.tlsState
	if st == nil {
		st = snap.tls // reused connections see no handshake event
	}

	if st == nil {
		return nil
	}

	ti := &TLSInfo{
		Version:            tls.VersionName(st.Version),
		CipherSuite:        tls.CipherSuiteName(st.CipherSuite),
		NegotiatedProtocol: st.NegotiatedProtocol,
		ServerName:         st.ServerName,
		HandshakeComplete:  st.HandshakeComplete,
		DidResume:          st.DidResume,
		OCSPStapled:        len(st.OCSPResponse) > 0,
		SCTCount:           len(st.SignedCertificateTimestamps),
		VerifiedChains:     len(st.VerifiedChains),
	}
	if ex.t.Options.CaptureCertificates {
		for _, cert := range st.PeerCertificates {
			ti.PeerCertificates = append(ti.PeerCertificates, newCertInfo(cert, ex.t.Options.CaptureRawCertificates))
		}
	}

	return ti
}

func newCertInfo(cert *x509.Certificate, includeRaw bool) CertInfo {
	sum := sha256.Sum256(cert.Raw)

	ci := CertInfo{
		Subject:            cert.Subject.String(),
		Issuer:             cert.Issuer.String(),
		SerialNumber:       cert.SerialNumber.String(),
		DNSNames:           append([]string(nil), cert.DNSNames...),
		NotBefore:          cert.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:           cert.NotAfter.UTC().Format(time.RFC3339),
		PublicKeyAlgorithm: cert.PublicKeyAlgorithm.String(),
		SignatureAlgorithm: cert.SignatureAlgorithm.String(),
		SHA256Fingerprint:  hex.EncodeToString(sum[:]),
	}
	for _, ip := range cert.IPAddresses {
		ci.IPAddresses = append(ci.IPAddresses, ip.String())
	}

	if includeRaw {
		ci.RawDER = base64.StdEncoding.EncodeToString(cert.Raw)
	}

	return ci
}

// statusText extracts the reason phrase from resp.Status ("200 OK" -> "OK"),
// falling back to the standard text for the code.
func statusText(resp *http.Response) string {
	if resp.Status != "" {
		if _, text, ok := strings.Cut(resp.Status, " "); ok {
			return text
		}
	}

	return http.StatusText(resp.StatusCode)
}
