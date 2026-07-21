package recorder

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
)

// HeadSamplingDecision selects how much work the Transport performs for an
// exchange. The zero value preserves the configured full-recording behavior.
type HeadSamplingDecision uint8

const (
	HeadSampleFull HeadSamplingDecision = iota
	HeadSampleMetadataOnly
	HeadSampleDrop
)

// HeadSamplingMeta is the bounded, non-secret request metadata available
// before exchange instrumentation is constructed. MIMEType excludes parameters.
type HeadSamplingMeta struct {
	Method        string
	Scheme        string
	Host          string
	Path          string
	MIMEType      string
	ContentLength int64
	SamplingKey   string
	TraceID       string
	RedirectIndex int
}

// HeadSamplingPolicy decides whether an exchange is fully recorded, reduced
// to metadata, or passed directly to the wrapped transport. Implementations
// must be fast, side-effect free, and safe for concurrent use.
type HeadSamplingPolicy interface {
	SampleHead(context.Context, HeadSamplingMeta) HeadSamplingDecision
}

// HeadSamplingPolicyFunc adapts a function to HeadSamplingPolicy.
type HeadSamplingPolicyFunc func(context.Context, HeadSamplingMeta) HeadSamplingDecision

// SampleHead implements HeadSamplingPolicy.
func (f HeadSamplingPolicyFunc) SampleHead(ctx context.Context, meta HeadSamplingMeta) HeadSamplingDecision {
	return f(ctx, meta)
}

// RetentionDecision controls whether a finalized entry reaches the Recorder.
type RetentionDecision uint8

const (
	RetainEntry RetentionDecision = iota
	DiscardEntry
)

// RetentionPolicy runs after OnEntryCompleted and before Recorder.Record.
// Capture cost has already been paid. Implementations must be concurrency-safe.
type RetentionPolicy interface {
	Retain(context.Context, *Entry) RetentionDecision
}

// RetentionPolicyFunc adapts a function to RetentionPolicy.
type RetentionPolicyFunc func(context.Context, *Entry) RetentionDecision

// Retain implements RetentionPolicy.
func (f RetentionPolicyFunc) Retain(ctx context.Context, entry *Entry) RetentionDecision {
	return f(ctx, entry)
}

// SamplingStats is an atomic snapshot of head and tail decisions.
type SamplingStats struct {
	HeadFull             uint64
	HeadMetadataOnly     uint64
	HeadDropped          uint64
	HeadPanics           uint64
	HeadInvalidDecisions uint64

	Retained                  uint64
	Discarded                 uint64
	RetentionPanics           uint64
	RetentionInvalidDecisions uint64
	AssetReleaseFailures      uint64
}

type samplingCounters struct {
	headFull             atomic.Uint64
	headMetadataOnly     atomic.Uint64
	headDropped          atomic.Uint64
	headPanics           atomic.Uint64
	headInvalidDecisions atomic.Uint64

	retained                  atomic.Uint64
	discarded                 atomic.Uint64
	retentionPanics           atomic.Uint64
	retentionInvalidDecisions atomic.Uint64
	assetReleaseFailures      atomic.Uint64
}

type samplingKeyContextKey struct{}

// WithSamplingKey returns a context carrying a stable application sampling
// key. Rate policies prefer it over TraceID, preserving decisions across
// related requests even when no recorder trace context is installed.
func WithSamplingKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, samplingKeyContextKey{}, key)
}

// SamplingKeyFromContext returns the key installed by WithSamplingKey.
func SamplingKeyFromContext(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(samplingKeyContextKey{}).(string)

	return key, ok && key != ""
}

type rateHeadSampler struct {
	threshold uint64
	always    bool
	sampled   HeadSamplingDecision
	unsampled HeadSamplingDecision
}

// NewRateHeadSampler creates a deterministic rate policy. SamplingKey is used
// first, then TraceID. Without either, each physical exchange uses crypto/rand
// and redirect-chain consistency is not guaranteed.
func NewRateHeadSampler(fraction float64, sampled, unsampled HeadSamplingDecision) (HeadSamplingPolicy, error) {
	if math.IsNaN(fraction) || fraction < 0 || fraction > 1 {
		return nil, errors.New("recorder: head sampling fraction must be between 0 and 1")
	}

	if !validHeadSamplingDecision(sampled) || !validHeadSamplingDecision(unsampled) {
		return nil, errors.New("recorder: invalid rate head sampling decision")
	}

	var threshold uint64
	if fraction < 1 {
		threshold = uint64(math.Ldexp(fraction, 64))
	}

	return &rateHeadSampler{threshold: threshold, always: fraction == 1, sampled: sampled, unsampled: unsampled}, nil
}

func (s *rateHeadSampler) SampleHead(_ context.Context, meta HeadSamplingMeta) HeadSamplingDecision {
	key := meta.SamplingKey

	if key == "" {
		key = meta.TraceID
	}

	var value uint64

	if key == "" {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return s.sampled
		}

		value = binary.BigEndian.Uint64(random[:])
	} else {
		value = stableSamplingHash(key)
	}

	if s.always || value < s.threshold {
		return s.sampled
	}

	return s.unsampled
}

func stableSamplingHash(value string) uint64 {
	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
	)

	hash := offset
	for i := range len(value) {
		hash ^= uint64(value[i])
		hash *= prime
	}

	return hash
}

type exchangeIdentity struct {
	traceID       string
	redirectIndex int
	hasTraceState bool
	samplingKey   string
}

func resolveExchangeIdentity(ctx context.Context) exchangeIdentity {
	identity := exchangeIdentity{}
	if key, ok := SamplingKeyFromContext(ctx); ok {
		identity.samplingKey = key
	}

	if state := traceStateFromContext(ctx); state != nil {
		identity.traceID = state.id
		identity.redirectIndex = int(state.seq.Add(1) - 1)
		identity.hasTraceState = true
	}

	return identity
}

func headSamplingMeta(req *http.Request, identity exchangeIdentity) HeadSamplingMeta {
	meta := HeadSamplingMeta{
		Method:        req.Method,
		ContentLength: req.ContentLength,
		SamplingKey:   identity.samplingKey,
		TraceID:       identity.traceID,
		RedirectIndex: identity.redirectIndex,
	}

	if req.URL != nil {
		meta.Scheme = req.URL.Scheme
		meta.Host = req.URL.Host

		meta.Path = req.URL.EscapedPath()
		if meta.Path == "" {
			meta.Path = "/"
		}
	}

	contentType := req.Header.Get("Content-Type")
	if contentType != "" {
		if mimeType, _, err := mime.ParseMediaType(contentType); err == nil {
			meta.MIMEType = strings.ToLower(mimeType)
		}
	}

	return meta
}

func validHeadSamplingDecision(decision HeadSamplingDecision) bool {
	return decision == HeadSampleFull || decision == HeadSampleMetadataOnly || decision == HeadSampleDrop
}

func entryHasStoreReferences(entry *Entry) bool {
	return entry != nil &&
		((entry.RequestBody != nil && entry.RequestBody.Store != "") ||
			(entry.ResponseBody != nil && entry.ResponseBody.Store != ""))
}

func (t *Transport) decideHeadSampling(ctx context.Context, meta HeadSamplingMeta) (decision HeadSamplingDecision) {
	policy := t.Options.HeadSamplingPolicy
	if policy == nil {
		t.sampling.headFull.Add(1)

		return HeadSampleFull
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.sampling.headPanics.Add(1)
			t.sampling.headFull.Add(1)
			t.internalError(fmt.Errorf("recorder: panic in HeadSamplingPolicy: %v", recovered))

			decision = HeadSampleFull
		}
	}()

	decision = policy.SampleHead(ctx, meta)
	if !validHeadSamplingDecision(decision) {
		t.sampling.headInvalidDecisions.Add(1)
		t.sampling.headFull.Add(1)
		t.internalError(fmt.Errorf("recorder: invalid head sampling decision %d", decision))

		return HeadSampleFull
	}

	switch decision {
	case HeadSampleFull:
		t.sampling.headFull.Add(1)

	case HeadSampleMetadataOnly:
		t.sampling.headMetadataOnly.Add(1)

	case HeadSampleDrop:
		t.sampling.headDropped.Add(1)
	}

	return decision
}

func (t *Transport) decideRetention(ctx context.Context, entry *Entry) (decision RetentionDecision) {
	policy := t.Options.RetentionPolicy
	if policy == nil {
		t.sampling.retained.Add(1)

		return RetainEntry
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.sampling.retentionPanics.Add(1)
			t.sampling.retained.Add(1)
			t.internalError(fmt.Errorf("recorder: panic in RetentionPolicy: %v", recovered))

			decision = RetainEntry
		}
	}()

	decision = policy.Retain(ctx, entry)
	if decision != RetainEntry && decision != DiscardEntry {
		t.sampling.retentionInvalidDecisions.Add(1)
		t.sampling.retained.Add(1)
		t.internalError(fmt.Errorf("recorder: invalid retention decision %d", decision))

		return RetainEntry
	}

	if decision == RetainEntry {
		t.sampling.retained.Add(1)
	}

	return decision
}

// SamplingStats returns a concurrency-safe point-in-time sampling snapshot.
func (t *Transport) SamplingStats() SamplingStats {
	return SamplingStats{
		HeadFull:                  t.sampling.headFull.Load(),
		HeadMetadataOnly:          t.sampling.headMetadataOnly.Load(),
		HeadDropped:               t.sampling.headDropped.Load(),
		HeadPanics:                t.sampling.headPanics.Load(),
		HeadInvalidDecisions:      t.sampling.headInvalidDecisions.Load(),
		Retained:                  t.sampling.retained.Load(),
		Discarded:                 t.sampling.discarded.Load(),
		RetentionPanics:           t.sampling.retentionPanics.Load(),
		RetentionInvalidDecisions: t.sampling.retentionInvalidDecisions.Load(),
		AssetReleaseFailures:      t.sampling.assetReleaseFailures.Load(),
	}
}
