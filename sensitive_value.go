package recorder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	defaultMaxProtectedValueBytes = 64 << 10
	maxProtectedValueBytes        = 16 << 20
	encryptedValuePrefix          = "REC-ENC-v1."
	tokenizedValuePrefix          = "REC-TOK-v1."
)

// ProtectionMode controls how configured sensitive values are represented in
// recorded copies. The zero value preserves the traditional [REDACTED]
// behavior.
type ProtectionMode string

const (
	ProtectionRedact   ProtectionMode = "redact"
	ProtectionEncrypt  ProtectionMode = "encrypt"
	ProtectionTokenize ProtectionMode = "tokenize"
)

// ProtectionKey is the active key used for a recorded sensitive value. ID is
// embedded in protected tokens to support rotation; it must not itself be
// sensitive. Encryption requires exactly 32 key bytes. Tokenization accepts
// 32 or more bytes.
type ProtectionKey struct {
	ID  string
	Key []byte
}

// ProtectionKeyProvider returns the active key for the requested mode. It may
// be called concurrently and must not return key material that callers mutate.
type ProtectionKeyProvider interface {
	ProtectionKey(mode ProtectionMode) (ProtectionKey, error)
}

// ProtectionKeyProviderFunc adapts a function to ProtectionKeyProvider.
type ProtectionKeyProviderFunc func(ProtectionMode) (ProtectionKey, error)

func (f ProtectionKeyProviderFunc) ProtectionKey(mode ProtectionMode) (ProtectionKey, error) {
	return f(mode)
}

// SensitiveValueProtection configures the representation of all values
// selected by built-in redaction rules. MaxValueBytes bounds a single value
// buffered for encryption or tokenization. Values <= 0 select the safe 64 KiB
// default; values above 16 MiB are clamped. Failures and oversized values are
// replaced with [REDACTED].
type SensitiveValueProtection struct {
	Mode          ProtectionMode
	KeyProvider   ProtectionKeyProvider
	MaxValueBytes int
}

type sensitiveValueProtector struct {
	config SensitiveValueProtection
	rand   func([]byte) (int, error)
}

// protectedValueBuffer bounds the only plaintext retained by streaming
// redactors. Once the limit is crossed it forgets the accumulated value and
// remembers only that protection must fail closed.
type protectedValueBuffer struct {
	protector *sensitiveValueProtector
	value     []byte
	tooLarge  bool
	emitted   bool
	report    ProtectionCounts
}

func (b *protectedValueBuffer) reset(protector *sensitiveValueProtector) {
	b.protector = protector
	b.value = b.value[:0]
	b.tooLarge = false
	b.emitted = false
}

func (b *protectedValueBuffer) append(p ...byte) {
	if b.tooLarge {
		return
	}
	if len(b.value)+len(p) > b.protector.maxValueBytes() {
		for i := range b.value {
			b.value[i] = 0
		}
		b.value = b.value[:0]
		b.tooLarge = true
		return
	}
	b.value = append(b.value, p...)
}

func (b *protectedValueBuffer) finish() (string, ProtectionMode, string) {
	if b.emitted {
		return "", ProtectionRedact, ""
	}
	if b.tooLarge {
		b.record(ProtectionRedact, "value_too_large")
		return redactedValue, ProtectionRedact, "value_too_large"
	}
	value, mode, reason := b.protector.protect(b.value)
	b.record(mode, reason)
	return value, mode, reason
}

func (b *protectedValueBuffer) redactImmediately() bool {
	if b.protector.config.Mode == ProtectionRedact {
		b.emitted = true
		b.record(ProtectionRedact, "")
		return true
	}
	return false
}

func (b *protectedValueBuffer) record(mode ProtectionMode, reason string) {
	switch mode {
	case ProtectionEncrypt:
		b.report.Encrypted++
	case ProtectionTokenize:
		b.report.Tokenized++
	default:
		b.report.Redacted++
	}
	if reason != "" {
		if b.report.Fallbacks == nil {
			b.report.Fallbacks = make(map[string]int64)
		}
		b.report.Fallbacks[reason]++
	}
}

func (b *protectedValueBuffer) protectionReport() ProtectionCounts {
	return cloneProtectionCounts(b.report)
}

func cloneProtectionCounts(in ProtectionCounts) ProtectionCounts {
	out := in
	if len(in.Fallbacks) > 0 {
		out.Fallbacks = make(map[string]int64, len(in.Fallbacks))
		for reason, count := range in.Fallbacks {
			out.Fallbacks[reason] = count
		}
	}
	return out
}

func protectionCountsEmpty(in ProtectionCounts) bool {
	return in.Redacted == 0 && in.Encrypted == 0 && in.Tokenized == 0 && len(in.Fallbacks) == 0
}

func newSensitiveValueProtector(config SensitiveValueProtection) *sensitiveValueProtector {
	if config.Mode == "" {
		config.Mode = ProtectionRedact
	}
	if config.MaxValueBytes <= 0 {
		config.MaxValueBytes = defaultMaxProtectedValueBytes
	} else if config.MaxValueBytes > maxProtectedValueBytes {
		config.MaxValueBytes = maxProtectedValueBytes
	}
	return &sensitiveValueProtector{config: config, rand: rand.Read}
}

func (p *sensitiveValueProtector) maxValueBytes() int { return p.config.MaxValueBytes }

func (p *sensitiveValueProtector) protect(value []byte) (string, ProtectionMode, string) {
	if len(value) > p.maxValueBytes() {
		return redactedValue, ProtectionRedact, "value_too_large"
	}
	switch p.config.Mode {
	case ProtectionEncrypt:
		out, err := p.encrypt(value)
		if err != nil {
			return redactedValue, ProtectionRedact, "encryption_failed"
		}
		return out, ProtectionEncrypt, ""
	case ProtectionTokenize:
		out, err := p.tokenize(value)
		if err != nil {
			return redactedValue, ProtectionRedact, "tokenization_failed"
		}
		return out, ProtectionTokenize, ""
	default:
		return redactedValue, ProtectionRedact, ""
	}
}

func (p *sensitiveValueProtector) key(mode ProtectionMode) (ProtectionKey, error) {
	if p.config.KeyProvider == nil {
		return ProtectionKey{}, errors.New("recorder: sensitive value key provider is nil")
	}
	k, err := p.config.KeyProvider.ProtectionKey(mode)
	if err != nil {
		return ProtectionKey{}, err
	}
	if k.ID == "" || strings.Contains(k.ID, ".") {
		return ProtectionKey{}, errors.New("recorder: protection key ID must be non-empty and contain no dots")
	}
	return k, nil
}

func (p *sensitiveValueProtector) encrypt(value []byte) (string, error) {
	k, err := p.key(ProtectionEncrypt)
	if err != nil {
		return "", err
	}
	if len(k.Key) != 32 {
		return "", errors.New("recorder: encryption key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(k.Key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := p.rand(nonce); err != nil {
		return "", err
	}
	payload := gcm.Seal(nonce, nonce, value, []byte(k.ID))
	return encryptedValuePrefix + tokenPart(k.ID) + "." + tokenBytes(payload), nil
}

func (p *sensitiveValueProtector) tokenize(value []byte) (string, error) {
	k, err := p.key(ProtectionTokenize)
	if err != nil {
		return "", err
	}
	if len(k.Key) < 32 {
		return "", errors.New("recorder: tokenization key must contain at least 32 bytes")
	}
	mac := hmac.New(sha256.New, k.Key)
	_, _ = mac.Write(value)
	return tokenizedValuePrefix + tokenPart(k.ID) + "." + tokenBytes(mac.Sum(nil)), nil
}

func tokenPart(s string) string  { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func tokenBytes(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DecryptProtectedValue decrypts a REC-ENC-v1 token with the matching key.
// It is useful for trusted tooling; applications should avoid persisting the
// returned plaintext.
func DecryptProtectedValue(token string, key ProtectionKey) ([]byte, error) {
	kid, payload, err := parseProtectedToken(token, encryptedValuePrefix)
	if err != nil {
		return nil, err
	}
	if kid != key.ID || len(key.Key) != 32 {
		return nil, errors.New("recorder: protection key does not match token")
	}
	block, err := aes.NewCipher(key.Key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("recorder: encrypted token payload is too short")
	}
	return gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte(kid))
}

// VerifyProtectedToken reports whether value produced a REC-TOK-v1 token.
func VerifyProtectedToken(token string, value []byte, key ProtectionKey) (bool, error) {
	kid, payload, err := parseProtectedToken(token, tokenizedValuePrefix)
	if err != nil {
		return false, err
	}
	if kid != key.ID || len(key.Key) < 32 {
		return false, errors.New("recorder: protection key does not match token")
	}
	mac := hmac.New(sha256.New, key.Key)
	_, _ = mac.Write(value)
	return hmac.Equal(payload, mac.Sum(nil)), nil
}

func parseProtectedToken(token, prefix string) (string, []byte, error) {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return "", nil, errors.New("recorder: unsupported protected token")
	}
	encodedID, encodedPayload, ok := strings.Cut(rest, ".")
	if !ok || encodedID == "" || encodedPayload == "" || strings.Contains(encodedPayload, ".") {
		return "", nil, errors.New("recorder: malformed protected token")
	}
	idBytes, err := base64.RawURLEncoding.DecodeString(encodedID)
	if err != nil || len(idBytes) == 0 {
		return "", nil, errors.New("recorder: malformed protected token key ID")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return "", nil, fmt.Errorf("recorder: malformed protected token payload: %w", err)
	}
	return string(idBytes), payload, nil
}
