package recorder

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testRedactor() *redactor { return newRedactor(&Options{}) }

func TestClassifyDNSError(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://x/", Err: &net.OpError{
		Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true},
	}}
	if p := classifyPhase(err, traceView{}, false, false, false, nil); p != PhaseDNS {
		t.Errorf("phase = %q", p)
	}

	info := newErrorInfo(err, PhaseDNS, testRedactor(), nil, nil)
	if info.Type != "*net.DNSError" {
		t.Errorf("type = %q", info.Type)
	}

	want := []string{"*url.Error", "*net.OpError", "*net.DNSError"}
	if len(info.UnwrapChain) != len(want) {
		t.Fatalf("chain = %v", info.UnwrapChain)
	}

	for i, w := range want {
		if info.UnwrapChain[i] != w {
			t.Errorf("chain[%d] = %q, want %q", i, info.UnwrapChain[i], w)
		}
	}
}

func TestClassifyConnectionRefused(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://x/", Err: &net.OpError{
		Op: "dial", Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	}}
	if p := classifyPhase(err, traceView{}, false, false, false, nil); p != PhaseConnect {
		t.Errorf("phase = %q", p)
	}

	if p := classifyPhase(err, traceView{}, true, false, false, nil); p != PhaseProxy {
		t.Errorf("phase with proxy = %q", p)
	}
}

func TestClassifyTLSErrors(t *testing.T) {
	cases := []error{
		&url.Error{Op: "Get", URL: "x", Err: x509.UnknownAuthorityError{}},
		&url.Error{Op: "Get", URL: "x", Err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"}},
		&url.Error{Op: "Get", URL: "x", Err: x509.CertificateInvalidError{Cert: &x509.Certificate{}}},
	}
	for i, err := range cases {
		if p := classifyPhase(err, traceView{}, false, false, false, nil); p != PhaseTLS {
			t.Errorf("case %d phase = %q, want tls", i, p)
		}
	}
}

func TestClassifyContextErrors(t *testing.T) {
	deadline := &url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}

	// Context fired while connecting: attribute to the network phase.
	v := traceView{connectStart: at(1)}
	if p := classifyPhase(deadline, v, false, false, true, nil); p != PhaseConnect {
		t.Errorf("phase during connect = %q", p)
	}
	// Context fired while waiting for the response: attribute to context.
	v = traceView{gotConn: at(1), wroteRequest: at(2)}
	if p := classifyPhase(deadline, v, false, false, true, nil); p != PhaseContext {
		t.Errorf("phase during wait = %q", p)
	}
	// No progress at all: still context.
	if p := classifyPhase(deadline, traceView{}, false, false, true, nil); p != PhaseContext {
		t.Errorf("phase without trace = %q", p)
	}
	// The error merely matches context.DeadlineExceeded (net/http's
	// ResponseHeaderTimeout error does this) but the request context never
	// fired: the phase must reflect where the exchange actually stood.
	v = traceView{gotConn: at(1), wroteRequest: at(2)}
	if p := classifyPhase(deadline, v, false, false, false, nil); p != PhaseWaitResponse {
		t.Errorf("phase with live context = %q, want wait_response", p)
	}
}

func TestClassifyRedirectLoopLastResort(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "/loop", Err: errors.New("stopped after 10 redirects")}
	if p := classifyPhase(err, traceView{}, false, false, false, nil); p != PhaseRedirect {
		t.Errorf("phase = %q", p)
	}
}

func TestClassifyRequestBodyReadError(t *testing.T) {
	bodyErr := errors.New("boom")

	err := fmt.Errorf("send failed: %w", bodyErr)
	if p := classifyPhase(err, traceView{gotConn: at(1)}, false, false, false, bodyErr); p != PhaseWriteRequestBody {
		t.Errorf("phase = %q", p)
	}
}

func TestClassifyByTraceProgress(t *testing.T) {
	plain := errors.New("opaque failure")

	cases := []struct {
		name string
		v    traceView
		want string
	}{
		{"nothing started", traceView{}, PhaseRequestSetup},
		{"dns in flight", traceView{getConn: at(1), dnsStart: at(2)}, PhaseDNS},
		{"connect in flight", traceView{getConn: at(1), connectStart: at(2)}, PhaseConnect},
		{"tls in flight", traceView{getConn: at(1), connectStart: at(2), connectDone: at(3), tlsStart: at(4)}, PhaseTLS},
		{"conn obtained", traceView{getConn: at(1), gotConn: at(2)}, PhaseWriteRequest},
		{"request written", traceView{getConn: at(1), gotConn: at(2), wroteRequest: at(3)}, PhaseWaitResponse},
		{"first byte seen", traceView{getConn: at(1), gotConn: at(2), wroteRequest: at(3), firstByte: at(4)}, PhaseReadResponseHeaders},
	}
	for _, tc := range cases {
		if p := classifyPhase(plain, tc.v, false, false, false, nil); p != tc.want {
			t.Errorf("%s: phase = %q, want %q", tc.name, p, tc.want)
		}
	}

	if p := classifyPhase(io.ErrUnexpectedEOF, traceView{gotConn: at(1), wroteRequest: at(2), firstByte: at(3)}, false, false, false, nil); p != PhaseReadResponseHeaders {
		t.Errorf("unexpected EOF phase = %q", p)
	}
}

func TestErrorInfoFlags(t *testing.T) {
	deadline := &url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}

	info := newErrorInfo(deadline, PhaseContext, testRedactor(), context.DeadlineExceeded, nil)
	if !info.ContextDeadlineExceeded || !info.Timeout || info.ContextCanceled {
		t.Errorf("deadline flags = %+v", info)
	}

	canceled := &url.Error{Op: "Get", URL: "x", Err: context.Canceled}

	info = newErrorInfo(canceled, PhaseContext, testRedactor(), context.Canceled, nil)
	if !info.ContextCanceled || info.ContextDeadlineExceeded {
		t.Errorf("canceled flags = %+v", info)
	}

	var netErr net.Error = &net.OpError{Op: "dial", Err: &timeoutErr{}}

	info = newErrorInfo(netErr, PhaseConnect, testRedactor(), nil, nil)
	if !info.Timeout {
		t.Errorf("net timeout not detected: %+v", info)
	}

	// Matching a context error is not enough: with a live request context
	// the flags stay false (Timeout still reflects the error itself).
	info = newErrorInfo(deadline, PhaseWaitResponse, testRedactor(), nil, nil)
	if info.ContextDeadlineExceeded || info.ContextCanceled {
		t.Errorf("flags set despite live request context: %+v", info)
	}

	if !info.Timeout {
		t.Errorf("timeout flag lost: %+v", info)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestErrorInfoTemporary(t *testing.T) {
	info := newErrorInfo(&timeoutErr{}, PhaseConnect, testRedactor(), nil, nil)
	if !info.Temporary {
		t.Errorf("temporary not detected")
	}
}

func TestErrorInfoCause(t *testing.T) {
	cause := errors.New("user gave up")

	info := newErrorInfo(context.Canceled, PhaseContext, testRedactor(), context.Canceled, cause)
	if info.Cause != "user gave up" {
		t.Errorf("cause = %q", info.Cause)
	}
}

func TestErrorMessageRedaction(t *testing.T) {
	red := newRedactor(&Options{RedactErrorMessage: func(s string) string {
		return strings.ReplaceAll(s, "secret-token", "[GONE]")
	}})
	err := errors.New("lookup https://api?key=secret-token failed")

	info := newErrorInfo(err, PhaseDNS, red, nil, errors.New("cause secret-token"))
	if strings.Contains(info.Message, "secret-token") || strings.Contains(info.Cause, "secret-token") {
		t.Errorf("redaction failed: %+v", info)
	}
}

func TestUnwrapChainJoinedErrors(t *testing.T) {
	joined := errors.Join(errors.New("first"), errors.New("second"))

	chain := unwrapChain(fmt.Errorf("wrap: %w", joined))
	if len(chain) < 2 {
		t.Errorf("chain = %d elements", len(chain))
	}
}

// loopErr unwraps to itself; the chain walk must stay bounded.
type loopErr struct{}

func (l *loopErr) Error() string { return "loop" }
func (l *loopErr) Unwrap() error { return l }

func TestUnwrapChainDepthCap(t *testing.T) {
	chain := unwrapChain(&loopErr{})
	if len(chain) != maxUnwrapDepth {
		t.Errorf("chain length = %d, want cap %d", len(chain), maxUnwrapDepth)
	}
}

func TestContextCauseRecordedFromRequestContext(t *testing.T) {
	cause := errors.New("shutdown in progress")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)

	ex := &exchange{ctx: ctx}
	if got := ex.contextCause(); got == nil || got.Error() != "shutdown in progress" {
		t.Errorf("cause = %v", got)
	}

	ex = &exchange{ctx: context.Background()}
	if got := ex.contextCause(); got != nil {
		t.Errorf("cause on live context = %v", got)
	}

	_ = time.Now // keep time imported via at() usage consistency
}

func FuzzUnwrapChain(f *testing.F) {
	f.Add(3, "boom")
	f.Add(0, "")
	f.Add(64, "deep")
	f.Fuzz(func(t *testing.T, depth int, msg string) {
		if depth < 0 {
			depth = -depth
		}

		depth %= 64

		err := errors.New(msg)
		for i := 0; i < depth; i++ {
			err = fmt.Errorf("layer %d: %w", i, err)
		}

		chain := unwrapChain(err)
		if len(chain) == 0 || len(chain) > maxUnwrapDepth {
			t.Fatalf("chain length %d out of bounds", len(chain))
		}

		info := newErrorInfo(err, PhaseUnknown, testRedactor(), nil, nil)
		if info.Type == "" || len(info.UnwrapChain) != len(chain) {
			t.Fatalf("info = %+v", info)
		}
	})
}
