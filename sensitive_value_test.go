package recorder

import (
	"bytes"
	"errors"
	"testing"
)

func testProtectionKey(mode ProtectionMode) (ProtectionKey, error) {
	switch mode {
	case ProtectionEncrypt:
		return ProtectionKey{ID: "enc-2026-07", Key: bytes.Repeat([]byte{0x11}, 32)}, nil
	case ProtectionTokenize:
		return ProtectionKey{ID: "tok-2026-07", Key: bytes.Repeat([]byte{0x22}, 32)}, nil
	default:
		return ProtectionKey{}, errors.New("unexpected mode")
	}
}

func TestSensitiveValueEncryptionRoundTripAndRandomNonce(t *testing.T) {
	p := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionEncrypt, KeyProvider: ProtectionKeyProviderFunc(testProtectionKey),
	})
	first, mode, fallback := p.protect([]byte(`{"secret":true}`))
	second, _, _ := p.protect([]byte(`{"secret":true}`))
	if mode != ProtectionEncrypt || fallback != "" || first == second {
		t.Fatalf("first=%q second=%q mode=%q fallback=%q", first, second, mode, fallback)
	}
	key, _ := testProtectionKey(ProtectionEncrypt)
	plain, err := DecryptProtectedValue(first, key)
	if err != nil || string(plain) != `{"secret":true}` {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}

func TestSensitiveValueTokenizationIsDeterministicAndVerifiable(t *testing.T) {
	p := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionTokenize, KeyProvider: ProtectionKeyProviderFunc(testProtectionKey),
	})
	first, mode, fallback := p.protect([]byte("secret"))
	second, _, _ := p.protect([]byte("secret"))
	if mode != ProtectionTokenize || fallback != "" || first != second {
		t.Fatalf("first=%q second=%q mode=%q fallback=%q", first, second, mode, fallback)
	}
	key, _ := testProtectionKey(ProtectionTokenize)
	ok, err := VerifyProtectedToken(first, []byte("secret"), key)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	ok, err = VerifyProtectedToken(first, []byte("wrong"), key)
	if err != nil || ok {
		t.Fatalf("wrong value ok=%v err=%v", ok, err)
	}
}

func TestSensitiveValueProtectionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config SensitiveValueProtection
		value  string
		reason string
	}{
		{"oversized", SensitiveValueProtection{Mode: ProtectionEncrypt, MaxValueBytes: 3}, "four", "value_too_large"},
		{"missing encryption key", SensitiveValueProtection{Mode: ProtectionEncrypt}, "x", "encryption_failed"},
		{"missing token key", SensitiveValueProtection{Mode: ProtectionTokenize}, "x", "tokenization_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, mode, reason := newSensitiveValueProtector(tc.config).protect([]byte(tc.value))
			if got != redactedValue || mode != ProtectionRedact || reason != tc.reason {
				t.Fatalf("got=%q mode=%q reason=%q", got, mode, reason)
			}
		})
	}
}

func TestProtectedTokenRejectsWrongKeyAndMalformedInput(t *testing.T) {
	p := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionEncrypt, KeyProvider: ProtectionKeyProviderFunc(testProtectionKey),
	})
	token, _, _ := p.protect([]byte("secret"))
	if _, err := DecryptProtectedValue(token, ProtectionKey{ID: "wrong", Key: make([]byte, 32)}); err == nil {
		t.Fatal("expected wrong key error")
	}
	if _, err := DecryptProtectedValue("REC-ENC-v1.bad", ProtectionKey{}); err == nil {
		t.Fatal("expected malformed token error")
	}
}
