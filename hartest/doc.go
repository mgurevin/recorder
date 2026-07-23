// Package hartest turns recorded HAR or NDJSON entries into deterministic,
// network-free HTTP client fixtures.
//
// It is an optional testing helper, not part of recorder's capture path and
// not a production traffic replay tool. Matching is strict by default; an
// optional RequestNormalizer handles volatile request values without replacing
// fixture selection or enabling network fallback. A Transport is finite and
// stateful: every matched exchange is consumed once, and Verify reports unused
// or invalid trailing fixture input after application requests have completed.
// Optional ReplayTimingConfig playback supports bounded latency-sensitive
// tests without pretending to recreate DNS, TCP, or TLS operations.
package hartest
