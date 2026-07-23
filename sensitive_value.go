package recorder

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
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
	// ProtectionRedact replaces every selected value with [REDACTED].
	ProtectionRedact ProtectionMode = "redact"
	// ProtectionEncrypt emits a reversible, key-ID-bearing AES-256-GCM token.
	ProtectionEncrypt ProtectionMode = "encrypt"
	// ProtectionTokenize emits a deterministic, non-reversible HMAC token.
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

// ProtectionKeyProvider returns the active key for the request context and
// requested mode. Recorder resolves each mode at most once per exchange, and
// the request and response share that immutable key snapshot. The function may
// be called concurrently across exchanges and must not return key material
// that callers mutate.
type ProtectionKeyProvider func(context.Context, ProtectionMode) (ProtectionKey, error)

// ProtectionKeyResolver resolves historical key material by the non-secret ID
// embedded in a protected token. The function may be called concurrently
// and must not return key material that callers mutate.
type ProtectionKeyResolver func(keyID string) (ProtectionKey, error)

// SensitiveValueProtection configures the representation of every value
// selected by built-in or custom body redactors. MaxValueBytes bounds a single
// value buffered for encryption. Tokenization streams values through HMAC
// without retaining them. Values <= 0 select the safe 64 KiB default; values
// above 16 MiB are clamped. Failures and oversized values are replaced with
// [REDACTED].
type SensitiveValueProtection struct {
	// Mode selects redact, encrypt, or tokenize. The zero value redacts.
	Mode ProtectionMode
	// KeyProvider supplies encryption or tokenization key material.
	KeyProvider ProtectionKeyProvider
	// MaxValueBytes bounds one value buffered for encryption.
	MaxValueBytes int
}

type sensitiveValueProtector struct {
	config SensitiveValueProtection
	ctx    context.Context
	rand   func([]byte) (int, error)
	keys   *protectionKeyCache
}

type protectionKeyCache struct {
	mu       sync.Mutex
	resolved map[ProtectionMode]resolvedProtectionKey
}

type resolvedProtectionKey struct {
	key       ProtectionKey
	err       error
	aead      cipher.AEAD
	aad       []byte
	aeadReady bool
}

type bodyValueProtector struct {
	mu           sync.Mutex
	protector    *sensitiveValueProtector
	report       ProtectionCounts
	replacements int64
	firstErr     error
	failures     int64
}

func newBodyValueProtector(protector *sensitiveValueProtector) *bodyValueProtector {
	if protector == nil {
		protector = newSensitiveValueProtector(SensitiveValueProtection{})
	}

	return &bodyValueProtector{protector: protector}
}

func (p *bodyValueProtector) NewValue() BodyValue {
	value := &protectedValueBuffer{}
	value.reset(p)

	return value
}

func (p *bodyValueProtector) protectionReport() (ProtectionCounts, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return cloneProtectionCounts(p.report), p.replacements
}

func (p *bodyValueProtector) protectionFailure() (error, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.firstErr, p.failures
}

func (p *bodyValueProtector) record(mode ProtectionMode, reason string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.replacements++
	addProtectionOutcome(&p.report, mode, reason)

	if err != nil {
		if p.firstErr == nil {
			p.firstErr = err
		}

		p.failures++
	}
}

// protectedValueBuffer bounds the only plaintext retained by streaming
// redactors. Once the limit is crossed it forgets the accumulated value and
// remembers only that protection must fail closed.
type protectedValueBuffer struct {
	session   *bodyValueProtector
	value     []byte
	tooLarge  bool
	emitted   bool
	tokenMAC  hash.Hash
	tokenID   string
	tokenBuf  [256]byte
	tokenLen  int
	tokenSum  [sha256.Size]byte
	cryptoBuf []byte
	encoded   []byte
	tokenFail bool
	tokenErr  error
	finished  bool
	result    []byte
}

func (b *protectedValueBuffer) reset(session *bodyValueProtector) {
	b.session = session
	b.value = b.value[:0]
	b.tooLarge = false
	b.emitted = false
	b.tokenID = ""
	b.clearTokenBuffer()
	b.tokenFail = false
	b.tokenErr = nil
	b.finished = false
	b.result = nil

	if session.protector.config.Mode == ProtectionTokenize {
		key, err := session.protector.key(ProtectionTokenize)
		if err != nil || len(key.Key) < 32 {
			b.tokenFail = true

			if err == nil {
				err = errors.New("recorder: tokenization key must contain at least 32 bytes")
			}

			b.tokenErr = err

			return
		}

		if b.tokenMAC == nil {
			b.tokenMAC = hmac.New(sha256.New, key.Key)
		} else {
			b.tokenMAC.Reset()
		}

		b.tokenID = key.ID
	}
}

func (b *protectedValueBuffer) Write(p []byte) (int, error) {
	if b.finished {
		return 0, errors.New("recorder: write after protected value finish")
	}

	b.appendBytes(p)

	return len(p), nil
}

func (b *protectedValueBuffer) FinishTo(dst io.Writer) error {
	result := b.protectedBytes()
	if len(result) == 0 {
		return nil
	}

	n, err := dst.Write(result)
	if err == nil && n != len(result) {
		return io.ErrShortWrite
	}

	return err
}

func (b *protectedValueBuffer) appendByte(value byte) {
	if b.tooLarge || b.emitted {
		return
	}

	if b.session.protector.config.Mode == ProtectionRedact {
		return
	}

	if b.session.protector.config.Mode == ProtectionTokenize {
		if b.tokenMAC != nil {
			b.tokenBuf[b.tokenLen] = value
			b.tokenLen++

			if b.tokenLen == len(b.tokenBuf) {
				b.flushTokenBuffer()
			}
		}

		return
	}

	if len(b.value) == b.session.protector.maxValueBytes() {
		b.clearValue()
		b.tooLarge = true

		return
	}

	b.value = append(b.value, value)
}

func (b *protectedValueBuffer) appendBytes(p []byte) {
	if b.tooLarge || b.emitted {
		return
	}

	if b.session.protector.config.Mode == ProtectionRedact {
		return
	}

	if b.session.protector.config.Mode == ProtectionTokenize {
		if b.tokenMAC != nil {
			for len(p) > 0 {
				n := copy(b.tokenBuf[b.tokenLen:], p)
				b.tokenLen += n
				p = p[n:]

				if b.tokenLen == len(b.tokenBuf) {
					b.flushTokenBuffer()
				}
			}
		}

		return
	}

	if len(b.value)+len(p) > b.session.protector.maxValueBytes() {
		for i := range b.value {
			b.value[i] = 0
		}

		b.value = b.value[:0]
		b.tooLarge = true

		return
	}

	b.value = append(b.value, p...)
}

func (b *protectedValueBuffer) protectedBytes() []byte {
	if !b.finished {
		b.finished = true
		b.result, _, _ = b.finish()
	}

	return b.result
}

func (b *protectedValueBuffer) finish() ([]byte, ProtectionMode, string) {
	if b.emitted {
		return nil, ProtectionRedact, ""
	}

	if b.session.protector.config.Mode == ProtectionTokenize {
		if b.tokenFail || b.tokenMAC == nil {
			b.clearTokenBuffer()
			b.session.record(ProtectionRedact, "tokenization_failed", b.tokenErr)

			return b.redactedBytes(), ProtectionRedact, "tokenization_failed"
		}

		b.flushTokenBuffer()
		sum := b.tokenMAC.Sum(b.tokenSum[:0])
		value := b.encodeProtectedToken(tokenizedValuePrefix, b.tokenID, sum)

		for i := range sum {
			sum[i] = 0
		}

		b.session.record(ProtectionTokenize, "", nil)

		return value, ProtectionTokenize, ""
	}

	if b.tooLarge {
		b.session.record(ProtectionRedact, "value_too_large", nil)
		return b.redactedBytes(), ProtectionRedact, "value_too_large"
	}

	if b.session.protector.config.Mode == ProtectionEncrypt {
		value, err := b.encrypt()

		b.clearValue()

		if err != nil {
			b.session.record(ProtectionRedact, "encryption_failed", err)

			return b.redactedBytes(), ProtectionRedact, "encryption_failed"
		}

		b.session.record(ProtectionEncrypt, "", nil)

		return value, ProtectionEncrypt, ""
	}

	b.clearValue()
	b.session.record(ProtectionRedact, "", nil)

	return b.redactedBytes(), ProtectionRedact, ""
}

func (b *protectedValueBuffer) redactedBytes() []byte {
	b.encoded = append(b.encoded[:0], redactedValue...)

	return b.encoded
}

func (b *protectedValueBuffer) encrypt() ([]byte, error) {
	key, aead, aad, err := b.session.protector.encryption()
	if err != nil {
		return nil, err
	}

	nonceSize := aead.NonceSize()

	required := nonceSize + len(b.value) + aead.Overhead()
	if cap(b.cryptoBuf) < required {
		b.cryptoBuf = make([]byte, nonceSize, required)
	} else {
		b.cryptoBuf = b.cryptoBuf[:nonceSize]
	}

	nonce := b.cryptoBuf[:nonceSize]
	if n, err := b.session.protector.rand(nonce); err != nil {
		return nil, err
	} else if n != len(nonce) {
		return nil, io.ErrUnexpectedEOF
	}

	b.cryptoBuf = aead.Seal(b.cryptoBuf, nonce, b.value, aad)

	return b.encodeProtectedToken(encryptedValuePrefix, key.ID, b.cryptoBuf), nil
}

func (b *protectedValueBuffer) encodeProtectedToken(prefix, keyID string, payload []byte) []byte {
	b.encoded = appendProtectedToken(b.encoded[:0], prefix, keyID, payload)

	return b.encoded
}

func (b *protectedValueBuffer) flushTokenBuffer() {
	if b.tokenLen == 0 {
		return
	}

	_, _ = b.tokenMAC.Write(b.tokenBuf[:b.tokenLen])
	b.clearTokenBuffer()
}

func (b *protectedValueBuffer) clearTokenBuffer() {
	for i := 0; i < b.tokenLen; i++ {
		b.tokenBuf[i] = 0
	}

	b.tokenLen = 0
}

func (b *protectedValueBuffer) clearValue() {
	for i := range b.value {
		b.value[i] = 0
	}

	b.value = b.value[:0]
}

func (b *protectedValueBuffer) redactImmediately() bool {
	if b.session.protector.config.Mode == ProtectionRedact {
		b.emitted = true
		b.session.record(ProtectionRedact, "", nil)

		return true
	}

	return false
}

func addProtectionOutcome(report *ProtectionCounts, mode ProtectionMode, reason string) {
	switch mode {
	case ProtectionEncrypt:
		report.Encrypted++

	case ProtectionTokenize:
		report.Tokenized++

	default:
		report.Redacted++
	}

	if reason != "" {
		if report.Fallbacks == nil {
			report.Fallbacks = make(map[string]int64)
		}

		report.Fallbacks[reason]++
	}
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

	return &sensitiveValueProtector{
		config: config,
		ctx:    context.Background(),
		rand:   rand.Read,
		keys:   &protectionKeyCache{},
	}
}

func (p *sensitiveValueProtector) withContext(ctx context.Context, keys *protectionKeyCache) *sensitiveValueProtector {
	clone := *p
	clone.ctx = ctx
	clone.keys = keys

	if clone.keys == nil {
		clone.keys = &protectionKeyCache{}
	}

	return &clone
}

func (p *sensitiveValueProtector) maxValueBytes() int { return p.config.MaxValueBytes }

func (p *sensitiveValueProtector) protect(value []byte) (string, ProtectionMode, string) {
	protected, mode, reason, _ := p.protectWithError(value)
	return protected, mode, reason
}

func (p *sensitiveValueProtector) protectWithError(value []byte) (string, ProtectionMode, string, error) {
	if p.config.Mode == ProtectionEncrypt && len(value) > p.maxValueBytes() {
		return redactedValue, ProtectionRedact, "value_too_large", nil
	}

	switch p.config.Mode {
	case ProtectionEncrypt:
		out, err := p.encrypt(value)
		if err != nil {
			return redactedValue, ProtectionRedact, "encryption_failed", err
		}

		return out, ProtectionEncrypt, "", nil

	case ProtectionTokenize:
		out, err := p.tokenize(value)
		if err != nil {
			return redactedValue, ProtectionRedact, "tokenization_failed", err
		}

		return out, ProtectionTokenize, "", nil

	default:
		return redactedValue, ProtectionRedact, "", nil
	}
}

func (p *sensitiveValueProtector) key(mode ProtectionMode) (ProtectionKey, error) {
	p.keys.mu.Lock()
	defer p.keys.mu.Unlock()

	resolved := p.keyLocked(mode)

	return resolved.key, resolved.err
}

func (p *sensitiveValueProtector) keyLocked(mode ProtectionMode) resolvedProtectionKey {
	if resolved, ok := p.keys.resolved[mode]; ok {
		return resolved
	}

	key, err := p.resolveKey(mode)

	resolved := resolvedProtectionKey{key: key, err: err}
	if p.keys.resolved == nil {
		p.keys.resolved = make(map[ProtectionMode]resolvedProtectionKey, 1)
	}

	p.keys.resolved[mode] = resolved

	return resolved
}

func (p *sensitiveValueProtector) encryption() (ProtectionKey, cipher.AEAD, []byte, error) {
	p.keys.mu.Lock()
	defer p.keys.mu.Unlock()

	resolved := p.keyLocked(ProtectionEncrypt)
	if resolved.err != nil {
		return ProtectionKey{}, nil, nil, resolved.err
	}

	if resolved.aeadReady {
		return resolved.key, resolved.aead, resolved.aad, resolved.err
	}

	if len(resolved.key.Key) != 32 {
		resolved.err = errors.New("recorder: encryption key must contain exactly 32 bytes")
	} else {
		block, err := aes.NewCipher(resolved.key.Key)
		if err != nil {
			resolved.err = err
		} else {
			resolved.aead, resolved.err = cipher.NewGCM(block)
		}
	}

	resolved.aad = []byte(resolved.key.ID)
	resolved.aeadReady = true
	p.keys.resolved[ProtectionEncrypt] = resolved

	return resolved.key, resolved.aead, resolved.aad, resolved.err
}

func (p *sensitiveValueProtector) resolveKey(mode ProtectionMode) (ProtectionKey, error) {
	if p.config.KeyProvider == nil {
		return ProtectionKey{}, errors.New("recorder: sensitive value key provider is nil")
	}

	k, err := p.config.KeyProvider(p.ctx, mode)
	if err != nil {
		return ProtectionKey{}, err
	}

	if k.ID == "" || len(k.ID) > 256 || !utf8.ValidString(k.ID) {
		return ProtectionKey{}, errors.New("recorder: protection key ID must be valid UTF-8 between 1 and 256 bytes")
	}

	return k, nil
}

func (p *sensitiveValueProtector) encrypt(value []byte) (string, error) {
	k, gcm, aad, err := p.encryption()
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if n, err := p.rand(nonce); err != nil {
		return "", err
	} else if n != len(nonce) {
		return "", io.ErrUnexpectedEOF
	}

	payload := gcm.Seal(nonce, nonce, value, aad)

	return protectedToken(encryptedValuePrefix, k.ID, payload), nil
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

	return protectedToken(tokenizedValuePrefix, k.ID, mac.Sum(nil)), nil
}

func protectedToken(prefix, keyID string, payload []byte) string {
	return string(appendProtectedToken(nil, prefix, keyID, payload))
}

func appendProtectedToken(dst []byte, prefix, keyID string, payload []byte) []byte {
	keyIDBytes := []byte(keyID)
	keyIDLen := base64.RawURLEncoding.EncodedLen(len(keyIDBytes))
	payloadLen := base64.RawURLEncoding.EncodedLen(len(payload))

	required := len(prefix) + keyIDLen + 1 + payloadLen
	if cap(dst) < required {
		dst = make([]byte, required)
	} else {
		dst = dst[:required]
	}

	copy(dst, prefix)
	base64.RawURLEncoding.Encode(dst[len(prefix):len(prefix)+keyIDLen], keyIDBytes)
	dst[len(prefix)+keyIDLen] = '.'
	base64.RawURLEncoding.Encode(dst[len(prefix)+keyIDLen+1:], payload)

	return dst
}

// ProtectedTokenKeyID reports the non-secret key ID embedded in a supported
// protected token without decrypting or verifying its payload.
func ProtectedTokenKeyID(token string) (string, error) {
	switch {
	case strings.HasPrefix(token, encryptedValuePrefix):
		keyID, payload, err := splitProtectedToken(token, encryptedValuePrefix)
		if err == nil {
			err = validateProtectedTokenPayload(payload)
		}

		return keyID, err

	case strings.HasPrefix(token, tokenizedValuePrefix):
		keyID, payload, err := splitProtectedToken(token, tokenizedValuePrefix)
		if err == nil {
			err = validateProtectedTokenPayload(payload)
		}

		return keyID, err

	default:
		return "", errors.New("recorder: unsupported protected token")
	}
}

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

// DecryptProtectedValueWith resolves the key ID embedded in token and decrypts
// it. It is intended for trusted archive tooling spanning key rotations.
func DecryptProtectedValueWith(token string, resolver ProtectionKeyResolver) ([]byte, error) {
	if resolver == nil {
		return nil, errors.New("recorder: protection key resolver is nil")
	}

	keyID, payload, err := splitProtectedToken(token, encryptedValuePrefix)
	if err != nil {
		return nil, err
	}

	if err := validateProtectedTokenPayload(payload); err != nil {
		return nil, err
	}

	key, err := resolver(keyID)
	if err != nil {
		return nil, fmt.Errorf("recorder: resolve protection key %q: %w", keyID, err)
	}

	return DecryptProtectedValue(token, key)
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

// VerifyProtectedTokenWith resolves the key ID embedded in token and verifies
// value. It is intended for trusted archive tooling spanning key rotations.
func VerifyProtectedTokenWith(token string, value []byte, resolver ProtectionKeyResolver) (bool, error) {
	if resolver == nil {
		return false, errors.New("recorder: protection key resolver is nil")
	}

	keyID, payload, err := splitProtectedToken(token, tokenizedValuePrefix)
	if err != nil {
		return false, err
	}

	if err := validateProtectedTokenPayload(payload); err != nil {
		return false, err
	}

	key, err := resolver(keyID)
	if err != nil {
		return false, fmt.Errorf("recorder: resolve protection key %q: %w", keyID, err)
	}

	return VerifyProtectedToken(token, value, key)
}

func parseProtectedToken(token, prefix string) (string, []byte, error) {
	keyID, encodedPayload, err := splitProtectedToken(token, prefix)
	if err != nil {
		return "", nil, err
	}

	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return "", nil, fmt.Errorf("recorder: malformed protected token payload: %w", err)
	}

	return keyID, payload, nil
}

func splitProtectedToken(token, prefix string) (string, string, error) {
	rest, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return "", "", errors.New("recorder: unsupported protected token")
	}

	encodedID, encodedPayload, ok := strings.Cut(rest, ".")
	if !ok || encodedID == "" || encodedPayload == "" || strings.Contains(encodedPayload, ".") {
		return "", "", errors.New("recorder: malformed protected token")
	}

	idBytes, err := base64.RawURLEncoding.DecodeString(encodedID)
	if err != nil || len(idBytes) == 0 || len(idBytes) > 256 || !utf8.Valid(idBytes) {
		return "", "", errors.New("recorder: malformed protected token key ID")
	}

	return string(idBytes), encodedPayload, nil
}

func validateProtectedTokenPayload(encodedPayload string) error {
	decoder := base64.NewDecoder(base64.RawURLEncoding.Strict(), strings.NewReader(encodedPayload))
	if _, err := io.Copy(io.Discard, decoder); err != nil {
		return fmt.Errorf("recorder: malformed protected token payload: %w", err)
	}

	return nil
}
