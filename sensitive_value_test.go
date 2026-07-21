package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func testProtectionKey(_ context.Context, mode ProtectionMode) (ProtectionKey, error) {
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
		Mode: ProtectionEncrypt, KeyProvider: ProtectionKeyProvider(testProtectionKey),
	})
	first, mode, fallback := p.protect([]byte(`{"secret":true}`))

	second, _, _ := p.protect([]byte(`{"secret":true}`))
	if mode != ProtectionEncrypt || fallback != "" || first == second {
		t.Fatalf("first=%q second=%q mode=%q fallback=%q", first, second, mode, fallback)
	}

	key, _ := testProtectionKey(context.Background(), ProtectionEncrypt)

	plain, err := DecryptProtectedValue(first, key)
	if err != nil || string(plain) != `{"secret":true}` {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}

func TestSensitiveValueTokenizationIsDeterministicAndVerifiable(t *testing.T) {
	p := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionTokenize, KeyProvider: ProtectionKeyProvider(testProtectionKey),
	})
	first, mode, fallback := p.protect([]byte("secret"))

	second, _, _ := p.protect([]byte("secret"))
	if mode != ProtectionTokenize || fallback != "" || first != second {
		t.Fatalf("first=%q second=%q mode=%q fallback=%q", first, second, mode, fallback)
	}

	key, _ := testProtectionKey(context.Background(), ProtectionTokenize)

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
		Mode: ProtectionEncrypt, KeyProvider: ProtectionKeyProvider(testProtectionKey),
	})

	token, _, _ := p.protect([]byte("secret"))
	if _, err := DecryptProtectedValue(token, ProtectionKey{ID: "wrong", Key: make([]byte, 32)}); err == nil {
		t.Fatal("expected wrong key error")
	}

	if _, err := DecryptProtectedValue("REC-ENC-v1.bad", ProtectionKey{}); err == nil {
		t.Fatal("expected malformed token error")
	}

	for _, malformed := range []string{
		"foreign",
		"REC-ENC-v1.bad",
		"REC-ENC-v1.a2lk.*",
		"REC-TOK-v1..AA",
		"REC-TOK-v1.a2lk.AA.extra",
	} {
		if _, err := ProtectedTokenKeyID(malformed); err == nil {
			t.Errorf("ProtectedTokenKeyID(%q) succeeded", malformed)
		}
	}
}

func TestProtectedTokenResolverSupportsRotatedKeys(t *testing.T) {
	keys := map[string]ProtectionKey{
		"enc-k1": {ID: "enc-k1", Key: bytes.Repeat([]byte{0x31}, 32)},
		"enc-k2": {ID: "enc-k2", Key: bytes.Repeat([]byte{0x32}, 32)},
		"tok-k1": {ID: "tok-k1", Key: bytes.Repeat([]byte{0x41}, 32)},
		"tok-k2": {ID: "tok-k2", Key: bytes.Repeat([]byte{0x42}, 32)},
	}
	resolver := ProtectionKeyResolver(func(keyID string) (ProtectionKey, error) {
		key, ok := keys[keyID]
		if !ok {
			return ProtectionKey{}, fmt.Errorf("unknown key %q", keyID)
		}

		return key, nil
	})

	for _, keyID := range []string{"enc-k1", "enc-k2"} {
		protector := newSensitiveValueProtector(SensitiveValueProtection{
			Mode: ProtectionEncrypt,
			KeyProvider: ProtectionKeyProvider(func(context.Context, ProtectionMode) (ProtectionKey, error) {
				return keys[keyID], nil
			}),
		})
		token, _, _ := protector.protect([]byte("secret-" + keyID))

		if got, err := ProtectedTokenKeyID(token); err != nil || got != keyID {
			t.Fatalf("ProtectedTokenKeyID(%s) = %q, %v", keyID, got, err)
		}

		plain, err := DecryptProtectedValueWith(token, resolver)
		if err != nil || string(plain) != "secret-"+keyID {
			t.Fatalf("DecryptProtectedValueWith(%s) = %q, %v", keyID, plain, err)
		}
	}

	for _, keyID := range []string{"tok-k1", "tok-k2"} {
		protector := newSensitiveValueProtector(SensitiveValueProtection{
			Mode: ProtectionTokenize,
			KeyProvider: ProtectionKeyProvider(func(context.Context, ProtectionMode) (ProtectionKey, error) {
				return keys[keyID], nil
			}),
		})
		token, _, _ := protector.protect([]byte("candidate"))

		if got, err := ProtectedTokenKeyID(token); err != nil || got != keyID {
			t.Fatalf("ProtectedTokenKeyID(%s) = %q, %v", keyID, got, err)
		}

		valid, err := VerifyProtectedTokenWith(token, []byte("candidate"), resolver)
		if err != nil || !valid {
			t.Fatalf("VerifyProtectedTokenWith(%s) = %v, %v", keyID, valid, err)
		}
	}
}

func TestProtectedTokenResolverFailureIsPerToken(t *testing.T) {
	known := ProtectionKey{ID: "known", Key: bytes.Repeat([]byte{0x51}, 32)}
	unknown := ProtectionKey{ID: "unknown", Key: bytes.Repeat([]byte{0x52}, 32)}
	makeToken := func(key ProtectionKey) string {
		protector := newSensitiveValueProtector(SensitiveValueProtection{
			Mode: ProtectionEncrypt,
			KeyProvider: ProtectionKeyProvider(func(context.Context, ProtectionMode) (ProtectionKey, error) {
				return key, nil
			}),
		})
		token, _, _ := protector.protect([]byte(key.ID))

		return token
	}

	resolver := ProtectionKeyResolver(func(keyID string) (ProtectionKey, error) {
		if keyID != known.ID {
			return ProtectionKey{}, errors.New("key unavailable")
		}

		return known, nil
	})

	if _, err := DecryptProtectedValueWith(makeToken(unknown), resolver); err == nil {
		t.Fatal("unknown key resolved")
	}

	plain, err := DecryptProtectedValueWith(makeToken(known), resolver)
	if err != nil || string(plain) != known.ID {
		t.Fatalf("known token after resolver failure = %q, %v", plain, err)
	}

	wrongKeyResolver := ProtectionKeyResolver(func(keyID string) (ProtectionKey, error) {
		return ProtectionKey{ID: keyID, Key: bytes.Repeat([]byte{0x7f}, 32)}, nil
	})
	if _, err := DecryptProtectedValueWith(makeToken(known), wrongKeyResolver); err == nil {
		t.Fatal("wrong key material with matching ID decrypted token")
	}
}

func TestProtectionKeyProviderReceivesRequestContext(t *testing.T) {
	type tenantContextKey struct{}

	keys := map[string]ProtectionKey{
		"tenant-a": {ID: "tenant-a-key", Key: bytes.Repeat([]byte{0x61}, 32)},
		"tenant-b": {ID: "tenant-b-key", Key: bytes.Repeat([]byte{0x62}, 32)},
	}
	provider := ProtectionKeyProvider(func(ctx context.Context, mode ProtectionMode) (ProtectionKey, error) {
		if mode != ProtectionEncrypt {
			return ProtectionKey{}, errors.New("unexpected mode")
		}

		tenant, _ := ctx.Value(tenantContextKey{}).(string)

		key, ok := keys[tenant]
		if !ok {
			return ProtectionKey{}, errors.New("missing tenant key")
		}

		return key, nil
	})
	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(`{"password":"secret"}`)),
			ContentLength: int64(len(`{"password":"secret"}`)),
			Request:       req,
		}, nil
	})

	recorder, err := NewMemoryRecorderWithCapacity(2)
	if err != nil {
		t.Fatalf("NewMemoryRecorderWithCapacity: %v", err)
	}

	transport := NewTransport(base, recorder, configWith(withCaptureResponseBody(true),
		withEmbedBodies(true),
		withRedaction(RedactionConfig{Common: RedactionRules{JSONFields: []string{"password"}}}),
		withSensitiveValueProtection(SensitiveValueProtection{Mode: ProtectionEncrypt, KeyProvider: provider})),
	)

	var wg sync.WaitGroup
	for tenant := range keys {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ctx := context.WithValue(context.Background(), tenantContextKey{}, tenant)

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/"+tenant, nil)
			if err != nil {
				t.Errorf("NewRequestWithContext(%s): %v", tenant, err)

				return
			}

			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Errorf("RoundTrip(%s): %v", tenant, err)

				return
			}

			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Errorf("read response(%s): %v", tenant, err)
			}

			if err := response.Body.Close(); err != nil {
				t.Errorf("close response(%s): %v", tenant, err)
			}
		}()
	}

	wg.Wait()

	entries := recorder.Entries()
	if len(entries) != len(keys) {
		t.Fatalf("entries = %d, want %d", len(entries), len(keys))
	}

	for _, entry := range entries {
		tenant := strings.TrimPrefix(entry.Request.URL, "https://example.test/")
		key := keys[tenant]

		var document map[string]string
		if err := json.Unmarshal([]byte(entry.Response.Content.Text), &document); err != nil {
			t.Fatalf("parse %s recording: %v", tenant, err)
		}

		plain, err := DecryptProtectedValue(document["password"], key)
		if err != nil || string(plain) != `"secret"` {
			t.Fatalf("decrypt %s recording = %q, %v", tenant, plain, err)
		}
	}
}

func FuzzProtectedTokenKeyID(f *testing.F) {
	f.Add("REC-ENC-v1.a2lk.AA")
	f.Add("REC-TOK-v1.a2lk.AA")
	f.Add("foreign")

	f.Fuzz(func(t *testing.T, token string) {
		keyID, err := ProtectedTokenKeyID(token)
		if err == nil && (keyID == "" || len(keyID) > 256) {
			t.Fatalf("invalid successful key ID %q", keyID)
		}
	})
}

func TestProtectedTokenCrossLanguageVectors(t *testing.T) {
	encryptor := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionEncrypt,
		KeyProvider: ProtectionKeyProvider(func(context.Context, ProtectionMode) (ProtectionKey, error) {
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
		KeyProvider: ProtectionKeyProvider(func(context.Context, ProtectionMode) (ProtectionKey, error) {
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
		KeyProvider: ProtectionKeyProvider(func(context.Context, ProtectionMode) (ProtectionKey, error) {
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
				Mode: mode, KeyProvider: ProtectionKeyProvider(testProtectionKey),
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

	key, err := testProtectionKey(context.Background(), mode)
	if err != nil {
		t.Fatalf("protection key: %v", err)
	}

	return key
}
