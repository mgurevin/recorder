// Package hario reads and validates bounded HAR 1.2 documents and recorder
// NDJSON entry streams. Collector functions are all-or-nothing; pull streams
// yield one validated entry at a time and complete full-input validation only
// when Next reaches io.EOF.
//
// It is a consumer-side helper. The recorder package does not depend on it,
// and using it does not change recording behavior. Readers, external body
// assets, and protected-value resolution remain caller-owned.
package hario
