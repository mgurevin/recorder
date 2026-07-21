package otelrecorder

import (
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	recorder "github.com/mgurevin/recorder"
)

// baseAttributes is the shared low-cardinality set used on synthesized spans
// and inside span events. It never contains URLs beyond scheme+host, header
// or body material, or correlation IDs.
func (e *Exporter) baseAttributes(entry *recorder.Entry) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 16)

	if entry.Request != nil {
		if entry.Request.Method != "" {
			attrs = append(attrs, attribute.String("http.request.method", e.clamp(entry.Request.Method)))
		}

		if scheme, host := schemeAndHost(entry.Request.URL); scheme != "" || host != "" {
			if scheme != "" {
				attrs = append(attrs, attribute.String("url.scheme", scheme))
			}

			if host != "" {
				attrs = append(attrs, attribute.String("server.address", e.clamp(host)))
			}
		}
	}

	if entry.Response != nil {
		attrs = append(attrs, attribute.Int("http.response.status_code", entry.Response.Status))
	}

	if name, version := protocol(entry); name != "" {
		attrs = append(attrs, attribute.String("network.protocol.name", name))
		if version != "" {
			attrs = append(attrs, attribute.String("network.protocol.version", version))
		}
	}

	attrs = append(attrs, attribute.String("recorder.state", e.clamp(entry.State)))
	if entry.Error != nil {
		attrs = append(attrs, attribute.String("recorder.error.phase", e.clamp(entry.Error.Phase)))
	}

	if entry.Response != nil && entry.Response.Content != nil && entry.Response.Content.Decoded {
		attrs = append(attrs, attribute.Bool("recorder.response.decoded", true))
	}

	if entry.RequestBody != nil && entry.RequestBody.Truncated {
		attrs = append(attrs, attribute.Bool("recorder.request.body.truncated", true))
	}

	if entry.ResponseBody != nil && entry.ResponseBody.Truncated {
		attrs = append(attrs, attribute.Bool("recorder.response.body.truncated", true))
	}

	return attrs
}

// eventAttributes builds the full "recorder.http.exchange" span event set:
// base attributes plus numeric timings, body sizes, network and TLS summary.
func (e *Exporter) eventAttributes(entry *recorder.Entry) []attribute.KeyValue {
	attrs := e.baseAttributes(entry)

	attrs = append(attrs, attribute.Float64("recorder.duration_ms", entry.Time))
	if t := entry.Timings; t != nil {
		for _, tv := range []struct {
			name string
			v    float64
		}{
			{"blocked", t.Blocked},
			{"dns", t.DNS},
			{"connect", t.Connect},
			{"ssl", t.SSL},
			{"send", t.Send},
			{"wait", t.Wait},
			{"receive", t.Receive},
		} {
			if tv.v >= 0 { // -1 means unmeasured; omit rather than mislead
				attrs = append(attrs, attribute.Float64("recorder.timings."+tv.name, tv.v))
			}
		}
	}

	if rb := entry.RequestBody; rb != nil {
		attrs = append(attrs, attribute.Int64("recorder.request.body.bytes", rb.TotalBytes))
	}

	if rb := entry.ResponseBody; rb != nil {
		attrs = append(attrs, attribute.Int64("recorder.response.body.bytes", rb.TotalBytes))
	}

	if closedEarly(entry) {
		attrs = append(attrs, attribute.Bool("recorder.response.closed_early", true))
	}

	if n := entry.Network; n != nil {
		attrs = append(attrs,
			attribute.Bool("recorder.network.reused", n.ConnectionReused),
			attribute.Bool("recorder.network.http2", n.HTTP2),
		)
		if n.WasIdle {
			attrs = append(attrs, attribute.Bool("recorder.network.was_idle", true))
		}

		if n.DNSCoalesced {
			attrs = append(attrs, attribute.Bool("recorder.network.dns_coalesced", true))
		}
	}

	if tl := entry.TLS; tl != nil {
		attrs = append(attrs,
			attribute.String("recorder.tls.version", e.clamp(tl.Version)),
			attribute.String("recorder.tls.cipher_suite", e.clamp(tl.CipherSuite)),
		)
		if tl.DidResume {
			attrs = append(attrs, attribute.Bool("recorder.tls.resumed", true))
		}
	}

	if e.cfg.includeIDs {
		if entry.TraceID != "" {
			attrs = append(attrs, attribute.String("recorder.trace_id", e.clamp(entry.TraceID)))
		}

		if entry.ExchangeID != "" {
			attrs = append(attrs, attribute.String("recorder.exchange_id", e.clamp(entry.ExchangeID)))
		}
	}

	return e.appendCustom(attrs, e.cfg.spanAttrsFn, entry)
}

// metricAttributes is the deliberately minimal label set for every metric.
func (e *Exporter) metricAttributes(entry *recorder.Entry) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 8)
	if entry.Request != nil && entry.Request.Method != "" {
		attrs = append(attrs, attribute.String("http.request.method", e.clamp(entry.Request.Method)))
	}

	status := 0
	if entry.Response != nil {
		status = entry.Response.Status
	}

	attrs = append(attrs, attribute.String("http.response.status_class", statusClass(status)))

	attrs = append(attrs, attribute.String("recorder.state", e.clamp(entry.State)))
	if entry.Request != nil {
		if scheme, _ := schemeAndHost(entry.Request.URL); scheme != "" {
			attrs = append(attrs, attribute.String("url.scheme", scheme))
		}
	}

	if name, version := protocol(entry); name != "" {
		attrs = append(attrs, attribute.String("network.protocol.name", name))
		if version != "" {
			attrs = append(attrs, attribute.String("network.protocol.version", version))
		}
	}

	return e.appendCustom(attrs, e.cfg.metricAttrsFn, entry)
}

func (e *Exporter) appendCustom(dst []attribute.KeyValue,
	fn func(*recorder.Entry) []attribute.KeyValue, entry *recorder.Entry,
) []attribute.KeyValue {
	if fn == nil {
		return dst
	}

	extra := fn(entry)
	if len(extra) > maxCustomAttributes {
		extra = extra[:maxCustomAttributes]
	}

	for _, kv := range extra {
		if kv.Value.Type() == attribute.STRING {
			kv = attribute.String(string(kv.Key), e.clamp(kv.Value.AsString()))
		}

		dst = append(dst, kv)
	}

	return dst
}

// clamp bounds a string attribute value to the configured maximum length.
func (e *Exporter) clamp(s string) string {
	if e.cfg.maxAttrLen > 0 && len(s) > e.cfg.maxAttrLen {
		return s[:e.cfg.maxAttrLen]
	}

	return s
}

// statusClass folds a status code into a bounded label: "0" for exchanges
// without a response, otherwise "1xx".."5xx".
func statusClass(status int) string {
	if status <= 0 {
		return "0"
	}

	return strconv.Itoa(status/100) + "xx"
}

// schemeAndHost extracts only the scheme and host from the recorded URL —
// path, query, fragment and userinfo never leave the adapter.
func schemeAndHost(raw string) (scheme, host string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}

	return u.Scheme, u.Hostname()
}

// protocol maps the recorded HTTP version onto network.protocol.name/version
// ("HTTP/2.0" -> "http"/"2"). Unknown versions yield empty values rather
// than guesses.
func protocol(entry *recorder.Entry) (name, version string) {
	v := ""
	if entry.Response != nil && entry.Response.HTTPVersion != "" {
		v = entry.Response.HTTPVersion
	} else if entry.Request != nil {
		v = entry.Request.HTTPVersion
	}

	if v == "" {
		return "", ""
	}

	rest, ok := strings.CutPrefix(v, "HTTP/")
	if !ok {
		return "", ""
	}

	rest = strings.TrimSuffix(rest, ".0")

	return "http", rest
}
