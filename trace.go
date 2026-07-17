package recorder

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TraceEvent is one raw httptrace event, stored under the "_trace" extension
// when Options.CaptureRawTrace is enabled.
type TraceEvent struct {
	Name   string `json:"name"`
	Time   string `json:"time"`
	Detail string `json:"detail,omitempty"`
}

// informational1xx is one observed 1xx interim response.
type informational1xx struct {
	code   int
	header http.Header
}

// putIdleResult records whether the connection went back to the idle pool.
type putIdleResult struct {
	returned bool
	err      error
}

// traceView is an immutable snapshot of everything the collector observed.
// Zero time values mean "event did not happen".
type traceView struct {
	getConn      time.Time
	dnsStart     time.Time
	dnsDone      time.Time
	connectStart time.Time
	connectDone  time.Time
	tlsStart     time.Time
	tlsDone      time.Time
	gotConn      time.Time
	wroteHeaders time.Time
	wroteRequest time.Time
	firstByte    time.Time
	wait100      time.Time
	got100       time.Time

	dnsAddrs     []string
	dnsCoalesced bool
	getConnAddr  string

	dnsErr      error
	connectErr  error
	tlsErr      error
	wroteReqErr error

	reused   bool
	wasIdle  bool
	idleTime time.Duration

	network    string
	localAddr  string
	remoteAddr string

	tlsState *tls.ConnectionState

	// wroteHeaderFields are the header fields the transport actually wrote
	// to the wire, in wire order (HTTP/2 includes pseudo-headers such as
	// ":authority"). Values are raw; redaction happens at entry-build time.
	wroteHeaderFields []NameValuePair

	info1xx []informational1xx
	putIdle *putIdleResult

	raw []TraceEvent
}

// max1xxRecorded bounds how many interim responses one exchange stores.
const max1xxRecorded = 16

// traceCollector accumulates httptrace events for one exchange. All callback
// paths lock mu; events may arrive from transport-internal goroutines, in any
// order, and more than once (e.g. Happy-Eyeballs parallel dials) — the
// collector keeps the first start and the last successful completion of each
// step and never treats a missing event as an error.
type traceCollector struct {
	mu         sync.Mutex
	now        func() time.Time
	captureRaw bool
	notify     func(state string)
	v          traceView
}

func newTraceCollector(captureRaw bool) *traceCollector {
	return &traceCollector{now: time.Now, captureRaw: captureRaw}
}

// view returns a copy of the collected state, deep enough that later events
// cannot race with readers of the snapshot.
func (tc *traceCollector) view() traceView {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	v := tc.v
	v.raw = append([]TraceEvent(nil), tc.v.raw...)
	v.dnsAddrs = append([]string(nil), tc.v.dnsAddrs...)
	v.wroteHeaderFields = append([]NameValuePair(nil), tc.v.wroteHeaderFields...)
	v.info1xx = append([]informational1xx(nil), tc.v.info1xx...)
	if tc.v.putIdle != nil {
		cp := *tc.v.putIdle
		v.putIdle = &cp
	}
	return v
}

// event appends a raw trace event; callers must hold tc.mu.
func (tc *traceCollector) event(name, detail string) {
	if !tc.captureRaw {
		return
	}
	tc.v.raw = append(tc.v.raw, TraceEvent{
		Name:   name,
		Time:   tc.now().UTC().Format(time.RFC3339Nano),
		Detail: detail,
	})
}

func (tc *traceCollector) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if tc.v.getConn.IsZero() {
				tc.v.getConn = tc.now()
			}
			if tc.v.getConnAddr == "" {
				tc.v.getConnAddr = hostPort
			}
			tc.event("GetConn", hostPort)
		},
		DNSStart: func(info httptrace.DNSStartInfo) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if tc.v.dnsStart.IsZero() {
				tc.v.dnsStart = tc.now()
			}
			tc.event("DNSStart", info.Host)
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			tc.v.dnsDone = tc.now()
			tc.v.dnsCoalesced = tc.v.dnsCoalesced || info.Coalesced
			for _, a := range info.Addrs {
				tc.v.dnsAddrs = appendUnique(tc.v.dnsAddrs, a.IP.String())
			}
			detail := ""
			if info.Err != nil {
				tc.v.dnsErr = info.Err
				detail = "error: " + info.Err.Error()
			}
			if info.Coalesced {
				detail = strings.TrimSpace("coalesced " + detail)
			}
			tc.event("DNSDone", detail)
		},
		ConnectStart: func(network, addr string) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if tc.v.connectStart.IsZero() {
				tc.v.connectStart = tc.now()
			}
			if tc.v.network == "" {
				tc.v.network = network
			}
			tc.event("ConnectStart", network+" "+addr)
		},
		ConnectDone: func(network, addr string, err error) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if err != nil {
				tc.v.connectErr = err
				tc.event("ConnectDone", network+" "+addr+" error: "+err.Error())
				return
			}
			tc.v.connectDone = tc.now()
			tc.v.network = network
			tc.event("ConnectDone", network+" "+addr)
		},
		TLSHandshakeStart: func() {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if tc.v.tlsStart.IsZero() {
				tc.v.tlsStart = tc.now()
			}
			tc.event("TLSHandshakeStart", "")
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if err != nil {
				tc.v.tlsErr = err
				tc.event("TLSHandshakeDone", "error: "+err.Error())
				return
			}
			tc.v.tlsDone = tc.now()
			st := state
			tc.v.tlsState = &st
			tc.event("TLSHandshakeDone", state.NegotiatedProtocol)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			tc.v.gotConn = tc.now()
			// A GotConn without a Conn carries no information; do not let it
			// erase details from an earlier, complete event.
			if info.Conn != nil {
				tc.v.reused = info.Reused
				tc.v.wasIdle = info.WasIdle
				tc.v.idleTime = info.IdleTime
				if la := info.Conn.LocalAddr(); la != nil {
					tc.v.localAddr = la.String()
					if tc.v.network == "" {
						tc.v.network = la.Network()
					}
				}
				if ra := info.Conn.RemoteAddr(); ra != nil {
					tc.v.remoteAddr = ra.String()
				}
			}
			tc.event("GotConn", tc.v.remoteAddr)
		},
		WroteHeaderField: func(key string, values []string) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			for _, v := range values {
				tc.v.wroteHeaderFields = append(tc.v.wroteHeaderFields, NameValuePair{Name: key, Value: v})
			}
			tc.event("WroteHeaderField", key)
		},
		Wait100Continue: func() {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if tc.v.wait100.IsZero() {
				tc.v.wait100 = tc.now()
			}
			tc.event("Wait100Continue", "")
		},
		Got100Continue: func() {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if tc.v.got100.IsZero() {
				tc.v.got100 = tc.now()
			}
			tc.event("Got100Continue", "")
		},
		WroteHeaders: func() {
			tc.mu.Lock()
			if tc.v.wroteHeaders.IsZero() {
				tc.v.wroteHeaders = tc.now()
			}
			tc.event("WroteHeaders", "")
			notify := tc.notify
			tc.mu.Unlock()
			if notify != nil {
				notify(StateRequestHeadersWritten)
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			tc.v.wroteRequest = tc.now()
			detail := ""
			if info.Err != nil {
				tc.v.wroteReqErr = info.Err
				detail = "error: " + info.Err.Error()
			}
			tc.event("WroteRequest", detail)
		},
		GotFirstResponseByte: func() {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if tc.v.firstByte.IsZero() {
				tc.v.firstByte = tc.now()
			}
			tc.event("GotFirstResponseByte", "")
		},
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			if len(tc.v.info1xx) < max1xxRecorded {
				h := make(http.Header, len(header))
				for k, vs := range header {
					h[k] = append([]string(nil), vs...)
				}
				tc.v.info1xx = append(tc.v.info1xx, informational1xx{code: code, header: h})
			}
			tc.event("Got1xxResponse", strconv.Itoa(code))
			return nil
		},
		PutIdleConn: func(err error) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			tc.v.putIdle = &putIdleResult{returned: err == nil, err: err}
			detail := ""
			if err != nil {
				detail = "error: " + err.Error()
			}
			tc.event("PutIdleConn", detail)
		},
	}
}

func appendUnique(list []string, s string) []string {
	for _, existing := range list {
		if existing == s {
			return list
		}
	}
	return append(list, s)
}

// durMS converts a duration to non-negative milliseconds.
func durMS(d time.Duration) float64 {
	if d < 0 {
		d = 0
	}
	return float64(d) / float64(time.Millisecond)
}

// msBetween returns the milliseconds between two events, -1 when either did
// not happen, and never a negative value (clock jitter between goroutines is
// clamped to 0).
func msBetween(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() {
		return -1
	}
	return durMS(b.Sub(a))
}

// computeTimings maps the collected events onto HAR timing semantics.
//
//   - On a reused connection (including HTTP/2 multiplexing onto an existing
//     connection) no DNS/connect/TLS work happened for this exchange, so those
//     fields are -1 — not a misleading 0 — and blocked covers the wait for the
//     connection from the pool.
//   - Any phase whose events were not observed stays -1.
func computeTimings(v traceView, start, finish time.Time) *Timings {
	t := &Timings{Blocked: -1, DNS: -1, Connect: -1, Send: -1, Wait: -1, Receive: -1, SSL: -1}
	if !v.gotConn.IsZero() {
		if v.reused {
			t.Blocked = msBetween(start, v.gotConn)
		} else {
			firstNet := v.dnsStart
			if firstNet.IsZero() {
				firstNet = v.connectStart
			}
			if firstNet.IsZero() {
				firstNet = v.gotConn
			}
			t.Blocked = msBetween(start, firstNet)
			t.DNS = msBetween(v.dnsStart, v.dnsDone)
			t.Connect = msBetween(v.connectStart, v.connectDone)
			t.SSL = msBetween(v.tlsStart, v.tlsDone)
		}
	}
	sendEnd := v.wroteRequest
	if sendEnd.IsZero() {
		sendEnd = v.wroteHeaders
	}
	t.Send = msBetween(v.gotConn, sendEnd)
	waitStart := sendEnd
	if waitStart.IsZero() {
		waitStart = v.gotConn
	}
	t.Wait = msBetween(waitStart, v.firstByte)
	t.Receive = msBetween(v.firstByte, finish)
	return t
}
