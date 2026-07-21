package recorder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
)

// Error phases. The phase names the step of the HTTP exchange in which the
// failure was observed.
const (
	PhaseRequestSetup        = "request_setup"
	PhaseDNS                 = "dns"
	PhaseConnect             = "connect"
	PhaseProxy               = "proxy"
	PhaseTLS                 = "tls"
	PhaseWriteRequest        = "write_request"
	PhaseWriteRequestBody    = "write_request_body"
	PhaseWaitResponse        = "wait_response"
	PhaseReadResponseHeaders = "read_response_headers"
	PhaseReadResponseBody    = "read_response_body"
	PhaseRedirect            = "redirect"
	PhaseContext             = "context"
	PhaseUnknown             = "unknown"
)

// ErrorInfo is the structured _recorder.error value describing a transport or
// body-stream failure.
type ErrorInfo struct {
	Phase                   string   `json:"phase"`
	Type                    string   `json:"type"`
	Message                 string   `json:"message"`
	Timeout                 bool     `json:"timeout"`
	Temporary               bool     `json:"temporary"`
	ContextCanceled         bool     `json:"contextCanceled"`
	ContextDeadlineExceeded bool     `json:"contextDeadlineExceeded"`
	Cause                   string   `json:"cause,omitempty"`
	UnwrapChain             []string `json:"unwrapChain,omitempty"`
}

const maxUnwrapDepth = 32

// unwrapChain walks the error chain (following the first branch of joined
// errors), depth-capped so hostile Unwrap implementations cannot loop us.
func unwrapChain(err error) []error {
	var chain []error
	for e := err; e != nil && len(chain) < maxUnwrapDepth; {
		chain = append(chain, e)
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			e = u.Unwrap()

		case interface{ Unwrap() []error }:
			if us := u.Unwrap(); len(us) > 0 {
				e = us[0]
			} else {
				e = nil
			}

		default:
			e = nil
		}
	}

	return chain
}

// newErrorInfo builds the structured error record. ctxErr is the request
// context's Err() at failure time (nil when the context was still live);
// cause is the corresponding context.Cause. The ContextCanceled /
// ContextDeadlineExceeded flags require the request context to have actually
// fired: net/http timeout errors (e.g. ResponseHeaderTimeout's
// *http.timeoutError) deliberately match context errors via errors.Is, and
// must not masquerade as a caller-initiated cancellation.
func newErrorInfo(err error, phase string, red *redactor, ctxErr, cause error) *ErrorInfo {
	if err == nil {
		return nil
	}

	chain := unwrapChain(err)
	info := &ErrorInfo{
		Phase:                   phase,
		Message:                 red.redactError(err.Error()),
		ContextCanceled:         ctxErr != nil && errors.Is(err, context.Canceled),
		ContextDeadlineExceeded: ctxErr != nil && errors.Is(err, context.DeadlineExceeded),
	}

	info.UnwrapChain = make([]string, len(chain))
	for i, e := range chain {
		info.UnwrapChain[i] = fmt.Sprintf("%T", e)
	}

	info.Type = info.UnwrapChain[len(info.UnwrapChain)-1]

	for _, e := range chain {
		if t, ok := e.(interface{ Timeout() bool }); ok && t.Timeout() {
			info.Timeout = true
		}

		if t, ok := e.(interface{ Temporary() bool }); ok && t.Temporary() {
			info.Temporary = true
		}
	}

	if info.ContextDeadlineExceeded {
		info.Timeout = true
	}

	if cause != nil {
		info.Cause = red.redactError(cause.Error())
	}

	return info
}

// classifyPhase determines the failure phase for an error returned by the
// underlying RoundTripper. Typed matching (errors.As / errors.Is) comes
// first; the httptrace progress recorded in v is the fallback. String
// matching is a last resort only, for errors that expose no type at all
// (the redirect-loop error assembled by http.Client). ctxDone reports
// whether the request's own context had fired when the error was observed.
func classifyPhase(err error, v traceView, hasProxy, respReceived, ctxDone bool, reqBodyReadErr error) string {
	if err == nil {
		return ""
	}

	// A read failure recorded on the caller-supplied request body means the
	// transport aborted while streaming the body upward.
	if reqBodyReadErr != nil && !respReceived {
		return PhaseWriteRequestBody
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return PhaseDNS
	}

	if isTLSError(err) {
		return PhaseTLS
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial":
			if hasProxy {
				return PhaseProxy
			}

			return PhaseConnect

		case "write":
			if !v.wroteHeaders.IsZero() {
				return PhaseWriteRequestBody
			}

			return PhaseWriteRequest

		case "read":
			return readPhase(v, respReceived)
		}
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		if hasProxy {
			return PhaseProxy
		}

		return PhaseConnect
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Prefer the network step that was in flight when the context fired;
		// otherwise attribute the failure to the context itself — but only
		// when the request's own context really fired. Transport-internal
		// timeouts (net/http's ResponseHeaderTimeout error) merely match
		// context errors via errors.Is and belong to the phase the exchange
		// actually stood in.
		switch p := phaseFromTrace(v, respReceived); p {
		case PhaseDNS, PhaseConnect, PhaseTLS:
			return p
		}

		if ctxDone {
			return PhaseContext
		}

		return phaseFromTrace(v, respReceived)
	}

	// Last resort string match: http.Client's redirect-loop error carries no
	// typed error to match on.
	if msg := err.Error(); strings.Contains(msg, "stopped after") && strings.Contains(msg, "redirect") {
		return PhaseRedirect
	}

	return phaseFromTrace(v, respReceived)
}

func readPhase(v traceView, respReceived bool) string {
	if respReceived {
		return PhaseReadResponseBody
	}

	if !v.firstByte.IsZero() {
		return PhaseReadResponseHeaders
	}

	return PhaseWaitResponse
}

// phaseFromTrace derives the failure phase from how far the exchange
// progressed according to httptrace events.
func phaseFromTrace(v traceView, respReceived bool) string {
	switch {
	case respReceived:
		return PhaseReadResponseBody

	case !v.firstByte.IsZero():
		return PhaseReadResponseHeaders

	case !v.wroteRequest.IsZero() || !v.wroteHeaders.IsZero():
		return PhaseWaitResponse

	case !v.tlsStart.IsZero() && v.tlsDone.IsZero():
		return PhaseTLS

	case !v.connectStart.IsZero() && v.connectDone.IsZero():
		return PhaseConnect

	case !v.dnsStart.IsZero() && v.dnsDone.IsZero():
		return PhaseDNS

	case !v.gotConn.IsZero():
		return PhaseWriteRequest

	case v.getConn.IsZero() && v.dnsStart.IsZero() && v.connectStart.IsZero():
		return PhaseRequestSetup

	default:
		return PhaseUnknown
	}
}

// isTLSError reports whether the chain contains a TLS or certificate
// verification error. Typed checks first; the package prefix of unexported
// types (tls alerts, net/http's tlsHandshakeTimeoutError) is the fallback.
func isTLSError(err error) bool {
	var (
		hostnameErr x509.HostnameError
		caErr       x509.UnknownAuthorityError
		invalidErr  x509.CertificateInvalidError
		rootsErr    x509.SystemRootsError
		recordErr   tls.RecordHeaderError
		verifyErr   *tls.CertificateVerificationError
	)

	if errors.As(err, &hostnameErr) || errors.As(err, &caErr) ||
		errors.As(err, &invalidErr) || errors.As(err, &rootsErr) ||
		errors.As(err, &recordErr) || errors.As(err, &verifyErr) {
		return true
	}

	for _, e := range unwrapChain(err) {
		tn := fmt.Sprintf("%T", e)
		if strings.HasPrefix(tn, "tls.") || strings.HasPrefix(tn, "*tls.") ||
			strings.HasPrefix(tn, "x509.") || strings.HasPrefix(tn, "*x509.") ||
			strings.Contains(tn, "tlsHandshakeTimeout") {
			return true
		}
	}

	return false
}
