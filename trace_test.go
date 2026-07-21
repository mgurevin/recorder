package recorder

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http/httptrace"
	"testing"
	"time"
)

var traceBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return traceBase.Add(time.Duration(ms) * time.Millisecond) }

func TestComputeTimingsNewConnection(t *testing.T) {
	v := traceView{
		getConn:      at(0),
		dnsStart:     at(5),
		dnsDone:      at(15),
		connectStart: at(15),
		connectDone:  at(35),
		tlsStart:     at(35),
		tlsDone:      at(48),
		gotConn:      at(50),
		wroteHeaders: at(52),
		wroteRequest: at(55),
		firstByte:    at(90),
	}
	tm := computeTimings(v, at(0), at(120))
	want := map[string]float64{
		"blocked": 5, "dns": 10, "connect": 20, "ssl": 13,
		"send": 5, "wait": 35, "receive": 30,
	}

	got := map[string]float64{
		"blocked": tm.Blocked, "dns": tm.DNS, "connect": tm.Connect, "ssl": tm.SSL,
		"send": tm.Send, "wait": tm.Wait, "receive": tm.Receive,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
}

func TestComputeTimingsReusedConnection(t *testing.T) {
	v := traceView{
		gotConn:      at(3),
		reused:       true,
		wroteRequest: at(5),
		firstByte:    at(20),
	}

	tm := computeTimings(v, at(0), at(30))
	if tm.Blocked != 3 {
		t.Errorf("blocked = %v", tm.Blocked)
	}

	if tm.DNS != -1 || tm.Connect != -1 || tm.SSL != -1 {
		t.Errorf("reused connection must report -1 for dns/connect/ssl, got %+v", tm)
	}

	if tm.Send != 2 || tm.Wait != 15 || tm.Receive != 10 {
		t.Errorf("send/wait/receive = %v/%v/%v", tm.Send, tm.Wait, tm.Receive)
	}
}

func TestComputeTimingsNothingMeasured(t *testing.T) {
	tm := computeTimings(traceView{}, at(0), at(10))
	for name, v := range map[string]float64{
		"blocked": tm.Blocked, "dns": tm.DNS, "connect": tm.Connect,
		"ssl": tm.SSL, "send": tm.Send, "wait": tm.Wait, "receive": tm.Receive,
	} {
		if v != -1 {
			t.Errorf("%s = %v, want -1", name, v)
		}
	}
}

func TestComputeTimingsNeverNegative(t *testing.T) {
	// Out-of-order clocks across goroutines: wroteRequest before gotConn.
	v := traceView{gotConn: at(10), wroteRequest: at(5), firstByte: at(20)}

	tm := computeTimings(v, at(0), at(30))
	if tm.Send != 0 {
		t.Errorf("send = %v, want clamped 0", tm.Send)
	}

	for name, val := range map[string]float64{
		"blocked": tm.Blocked, "send": tm.Send, "wait": tm.Wait, "receive": tm.Receive,
	} {
		if val < -1 {
			t.Errorf("%s = %v is negative", name, val)
		}
	}
}

func TestComputeTimingsMissingTLSOnly(t *testing.T) {
	v := traceView{
		getConn:      at(0),
		connectStart: at(2),
		connectDone:  at(10),
		gotConn:      at(11),
		wroteRequest: at(12),
		firstByte:    at(20),
	}

	tm := computeTimings(v, at(0), at(25))
	if tm.SSL != -1 {
		t.Errorf("ssl = %v, want -1 for plain http", tm.SSL)
	}

	if tm.DNS != -1 {
		t.Errorf("dns = %v, want -1 when no resolver ran", tm.DNS)
	}

	if tm.Connect != 8 {
		t.Errorf("connect = %v", tm.Connect)
	}
}

// TestCollectorDuplicateAndOutOfOrderEvents drives the httptrace callbacks
// directly, including repeated and failing events, and checks first/last
// semantics without any panic.
func TestCollectorDuplicateAndOutOfOrderEvents(t *testing.T) {
	tc := newTraceCollector(true)
	clock := 0
	tc.now = func() time.Time {
		clock++
		return at(clock)
	}
	ct := tc.clientTrace()

	ct.DNSStart(httptrace.DNSStartInfo{Host: "a"})

	first := tc.view().dnsStart

	ct.DNSStart(httptrace.DNSStartInfo{Host: "a"}) // duplicate: keep first

	if got := tc.view().dnsStart; !got.Equal(first) {
		t.Errorf("dnsStart moved on duplicate event")
	}

	ct.DNSDone(httptrace.DNSDoneInfo{Addrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("127.0.0.1")}}})

	if addrs := tc.view().dnsAddrs; len(addrs) != 1 || addrs[0] != "127.0.0.1" {
		t.Errorf("dnsAddrs = %v", addrs)
	}

	// Happy-Eyeballs style: one failed connect, then a successful one.
	ct.ConnectStart("tcp", "10.0.0.1:80")
	ct.ConnectDone("tcp", "10.0.0.1:80", errors.New("unreachable"))

	if !tc.view().connectDone.IsZero() {
		t.Errorf("failed connect must not set connectDone")
	}

	ct.ConnectStart("tcp", "127.0.0.1:80")
	ct.ConnectDone("tcp", "127.0.0.1:80", nil)

	if tc.view().connectDone.IsZero() || tc.view().connectErr == nil {
		t.Errorf("view = %+v", tc.view())
	}

	ct.TLSHandshakeStart()
	ct.TLSHandshakeDone(tls.ConnectionState{}, errors.New("handshake failed"))

	v := tc.view()
	if !v.tlsDone.IsZero() || v.tlsErr == nil {
		t.Errorf("failed handshake must set tlsErr only")
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	ct.GotConn(httptrace.GotConnInfo{Conn: c1, Reused: true, WasIdle: true, IdleTime: time.Second})
	ct.GotConn(httptrace.GotConnInfo{}) // nil Conn: must not panic
	ct.WroteHeaders()
	ct.WroteRequest(httptrace.WroteRequestInfo{})
	ct.GotFirstResponseByte()

	fb := tc.view().firstByte

	ct.GotFirstResponseByte() // duplicate: keep first

	if !tc.view().firstByte.Equal(fb) {
		t.Errorf("firstByte moved on duplicate event")
	}

	ct.Got1xxResponse(103, nil)
	ct.PutIdleConn(nil)

	v = tc.view()
	if !v.reused || !v.wasIdle || v.idleTime != time.Second {
		t.Errorf("gotConn info = %+v", v)
	}

	if len(v.raw) == 0 {
		t.Errorf("raw trace empty despite captureRaw")
	}
}

func TestCollectorViewIsSnapshot(t *testing.T) {
	tc := newTraceCollector(true)
	ct := tc.clientTrace()
	ct.DNSStart(httptrace.DNSStartInfo{Host: "x"})

	v := tc.view()
	rawLen := len(v.raw)

	ct.DNSDone(httptrace.DNSDoneInfo{})

	if len(v.raw) != rawLen {
		t.Errorf("snapshot mutated by later events")
	}
}

func TestMsBetween(t *testing.T) {
	if got := msBetween(time.Time{}, at(5)); got != -1 {
		t.Errorf("zero start = %v", got)
	}

	if got := msBetween(at(5), time.Time{}); got != -1 {
		t.Errorf("zero end = %v", got)
	}

	if got := msBetween(at(10), at(5)); got != 0 {
		t.Errorf("negative clamped = %v", got)
	}

	if got := msBetween(at(5), at(10)); got != 5 {
		t.Errorf("normal = %v", got)
	}
}

func TestCollectorNewObservabilityEvents(t *testing.T) {
	tc := newTraceCollector(true)
	clock := 0
	tc.now = func() time.Time {
		clock++
		return at(clock)
	}
	ct := tc.clientTrace()

	ct.WroteHeaderField("Host", []string{"example.com"})
	ct.WroteHeaderField("Accept", []string{"text/html", "text/plain"})
	ct.Wait100Continue()
	ct.Got100Continue()
	ct.Got100Continue() // duplicate: keep first
	ct.DNSDone(httptrace.DNSDoneInfo{Coalesced: true})
	ct.Got1xxResponse(103, map[string][]string{"Link": {"</a>; rel=preload"}})
	ct.PutIdleConn(errors.New("connection is in a bad state"))

	v := tc.view()
	if len(v.wroteHeaderFields) != 3 {
		t.Fatalf("wroteHeaderFields = %+v", v.wroteHeaderFields)
	}

	if v.wroteHeaderFields[2] != (NameValuePair{Name: "Accept", Value: "text/plain"}) {
		t.Errorf("wire order lost: %+v", v.wroteHeaderFields)
	}

	if v.wait100.IsZero() || v.got100.IsZero() || !v.got100.Before(v.wait100.Add(10*time.Millisecond)) {
		t.Errorf("100-continue times = %v / %v", v.wait100, v.got100)
	}

	if !v.dnsCoalesced {
		t.Errorf("coalesced flag lost")
	}

	if len(v.info1xx) != 1 || v.info1xx[0].code != 103 || v.info1xx[0].header.Get("Link") == "" {
		t.Errorf("info1xx = %+v", v.info1xx)
	}

	if v.putIdle == nil || v.putIdle.returned || v.putIdle.err == nil {
		t.Errorf("putIdle = %+v", v.putIdle)
	}

	// A later successful PutIdleConn overrides the failure.
	ct.PutIdleConn(nil)

	if v = tc.view(); v.putIdle == nil || !v.putIdle.returned {
		t.Errorf("putIdle after success = %+v", v.putIdle)
	}
}

func TestCollector1xxRecordingBounded(t *testing.T) {
	tc := newTraceCollector(false)

	ct := tc.clientTrace()
	for i := 0; i < max1xxRecorded*2; i++ {
		ct.Got1xxResponse(103, nil)
	}

	if got := len(tc.view().info1xx); got != max1xxRecorded {
		t.Errorf("recorded %d interim responses, want cap %d", got, max1xxRecorded)
	}
}
