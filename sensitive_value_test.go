package recorder

import (
	"bytes"
	"errors"
	"io"
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

func TestProtectedTokenCrossLanguageVectors(t *testing.T) {
	encryptor := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionEncrypt,
		KeyProvider: ProtectionKeyProviderFunc(func(ProtectionMode) (ProtectionKey, error) {
			return ProtectionKey{ID: "enc-test", Key: bytes.Repeat([]byte{0x11}, 32)}, nil
		}),
	})
	encryptor.rand = func(p []byte) (int, error) {
		clear(p)
		return len(p), nil
	}

	encrypted, _, _ := encryptor.protect([]byte("secret"))
	if want := "REC-ENC-v1.ZW5jLXRlc3Q.AAAAAAAAAAAAAAAAt699FYTbc90VTqYwASFV4Vz4ucN_7A"; encrypted != want {
		t.Fatalf("encrypted vector = %q, want %q", encrypted, want)
	}

	tokenizer := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionTokenize,
		KeyProvider: ProtectionKeyProviderFunc(func(ProtectionMode) (ProtectionKey, error) {
			return ProtectionKey{ID: "tok-test", Key: bytes.Repeat([]byte{0x22}, 32)}, nil
		}),
	})

	tokenized, _, _ := tokenizer.protect([]byte("secret"))
	if want := "REC-TOK-v1.dG9rLXRlc3Q.NSnKjUFGTW2fAPl9GG2ZSFBsVbl0_MKjPMl6jfHuL8I"; tokenized != want {
		t.Fatalf("tokenized vector = %q, want %q", tokenized, want)
	}
}

func TestEncryptionFailsClosedOnShortRandomRead(t *testing.T) {
	p := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionEncrypt,
		KeyProvider: ProtectionKeyProviderFunc(func(ProtectionMode) (ProtectionKey, error) {
			return ProtectionKey{ID: "short-random", Key: bytes.Repeat([]byte{1}, 32)}, nil
		}),
	})
	p.rand = func([]byte) (int, error) { return 0, nil }

	got, mode, reason := p.protect([]byte("secret"))
	if got != redactedValue || mode != ProtectionRedact || reason != "encryption_failed" {
		t.Fatalf("got=%q mode=%q reason=%q", got, mode, reason)
	}
}

func TestBodyValueStreamsAndCountsOnce(t *testing.T) {
	for _, mode := range []ProtectionMode{ProtectionRedact, ProtectionEncrypt, ProtectionTokenize} {
		t.Run(string(mode), func(t *testing.T) {
			protector := newSensitiveValueProtector(SensitiveValueProtection{
				Mode: mode, KeyProvider: ProtectionKeyProviderFunc(testProtectionKey),
			})
			session := newBodyValueProtector(protector)
			value := session.NewValue()

			for _, chunk := range []string{"stream-", "secret"} {
				if _, err := io.WriteString(value, chunk); err != nil {
					t.Fatalf("write value: %v", err)
				}
			}

			protected := value.Finish()
			if again := value.Finish(); again != protected {
				t.Fatalf("second Finish = %q, want %q", again, protected)
			}

			if _, err := io.WriteString(value, "late"); err == nil {
				t.Fatal("write after Finish succeeded")
			}

			report, replacements := session.protectionReport()
			if replacements != 1 {
				t.Fatalf("replacements = %d, want 1", replacements)
			}

			switch mode {
			case ProtectionEncrypt:
				plain, err := DecryptProtectedValue(protected, mustProtectionKey(t, mode))
				if err != nil || string(plain) != "stream-secret" || report.Encrypted != 1 {
					t.Fatalf("encrypted=%q plain=%q report=%+v err=%v", protected, plain, report, err)
				}

			case ProtectionTokenize:
				valid, err := VerifyProtectedToken(protected, []byte("stream-secret"), mustProtectionKey(t, mode))
				if err != nil || !valid || report.Tokenized != 1 {
					t.Fatalf("tokenized=%q valid=%v report=%+v err=%v", protected, valid, report, err)
				}

			default:
				if protected != redactedValue || report.Redacted != 1 {
					t.Fatalf("redacted=%q report=%+v", protected, report)
				}
			}
		})
	}
}

func mustProtectionKey(t *testing.T, mode ProtectionMode) ProtectionKey {
	t.Helper()

	key, err := testProtectionKey(mode)
	if err != nil {
		t.Fatalf("protection key: %v", err)
	}

	return key
}
