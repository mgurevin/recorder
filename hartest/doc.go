// Package hartest turns recorded HAR or NDJSON entries into deterministic,
// network-free HTTP client fixtures.
//
// It is an optional testing helper, not part of recorder's capture path and
// not a production traffic replay tool. Matching is strict by default; an
// optional RequestNormalizer handles volatile request values without replacing
// fixture selection or enabling network fallback.
package hartest
