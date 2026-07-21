package recorder

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func encryptionProtector() (*sensitiveValueProtector, ProtectionKey) {
	key := ProtectionKey{ID: "enc-test", Key: bytes.Repeat([]byte{0x42}, 32)}

	return newSensitiveValueProtector(SensitiveValueProtection{
		Mode:        ProtectionEncrypt,
		KeyProvider: ProtectionKeyProviderFunc(func(ProtectionMode) (ProtectionKey, error) { return key, nil }),
	}), key
}

func decryptTestToken(t *testing.T, token string, key ProtectionKey, want string) {
	t.Helper()

	got, err := DecryptProtectedValue(token, key)
	if err != nil || string(got) != want {
		t.Fatalf("decrypt %q = %q, %v; want %q", token, got, err, want)
	}
}

func TestBuiltinBodyProtectionEncryptsExactMatchedValues(t *testing.T) {
	protector, key := encryptionProtector()

	t.Run("json", func(t *testing.T) {
		var out bytes.Buffer

		w := newJSONStreamRedactor(&out, lowerSet([]string{"password"}), newBodyValueProtector(protector))

		_, _ = w.Write([]byte(`{"keep":1,"password":{"nested":true},"tail":2}`))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}

		decryptTestToken(t, doc["password"].(string), key, `{"nested":true}`)

		if doc["keep"].(float64) != 1 || doc["tail"].(float64) != 2 {
			t.Fatalf("unmatched JSON changed: %s", out.Bytes())
		}
	})

	t.Run("xml", func(t *testing.T) {
		var out bytes.Buffer

		w := newXMLStreamRedactor(&out, lowerSet([]string{"password"}), newBodyValueProtector(protector))

		_, _ = w.Write([]byte(`<r><keep a="1">x</keep><password>secret<b>nested</b></password></r>`))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		text := out.String()
		start := strings.Index(text, encryptedValuePrefix)
		end := strings.Index(text[start:], "</password>")
		decryptTestToken(t, text[start:start+end], key, `secret<b>nested</b>`)

		if !strings.Contains(text, `<keep a="1">x</keep>`) {
			t.Fatalf("unmatched XML changed: %s", text)
		}
	})

	t.Run("form", func(t *testing.T) {
		var out bytes.Buffer

		w := newFormStreamRedactor(&out, lowerSet([]string{"token"}), newBodyValueProtector(protector))

		_, _ = w.Write([]byte(`keep=a%20b&token=s%2Bcret%20value&tail=z`))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		values, err := url.ParseQuery(out.String())
		if err != nil {
			t.Fatal(err)
		}

		decryptTestToken(t, values.Get("token"), key, `s%2Bcret%20value`)

		if values.Get("keep") != "a b" || values.Get("tail") != "z" {
			t.Fatalf("unmatched form changed: %s", out.String())
		}
	})

	t.Run("multipart", func(t *testing.T) {
		var out bytes.Buffer

		w := newMultipartStreamRedactor(&out, multipartTestType, lowerSet([]string{"token"}), newBodyValueProtector(protector))
		input := "--recorder-boundary\r\nContent-Disposition: form-data; name=token\r\n\r\npart-secret\r\n--recorder-boundary--\r\n"

		_, _ = w.Write([]byte(input))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		text := out.String()
		start := strings.Index(text, encryptedValuePrefix)
		end := strings.Index(text[start:], "\r\n--recorder-boundary")
		decryptTestToken(t, text[start:start+end], key, "part-secret")
	})
}

func TestScalarProtectionCoversHeadersQueryURLAndCookies(t *testing.T) {
	protector, key := encryptionProtector()
	r := &redactor{
		headers: lowerSet([]string{"authorization"}), query: lowerSet([]string{"token"}),
		cookies: lowerSet([]string{"session"}), protector: protector,
	}
	header := r.headerPairs(http.Header{"Authorization": {"Bearer secret"}}, "")
	decryptTestToken(t, header[0].Value, key, "Bearer secret")

	query := r.queryPairs("keep=ok&token=query-secret")
	decryptTestToken(t, query[1].Value, key, "query-secret")

	u, _ := url.Parse("https://user:pass@example.test/a?token=url-secret&keep=1")
	protectedURL, _ := url.Parse(r.redactURL(u))
	password, _ := protectedURL.User.Password()
	decryptTestToken(t, password, key, "pass")
	decryptTestToken(t, protectedURL.Query().Get("token"), key, "url-secret")
	decryptTestToken(t, r.protectString("cookie-secret"), key, "cookie-secret")
}

func TestBuiltinProtectionLimitFailsClosed(t *testing.T) {
	protector, _ := encryptionProtector()
	protector.config.MaxValueBytes = 3

	var out bytes.Buffer

	w := newJSONStreamRedactor(&out, lowerSet([]string{"password"}), newBodyValueProtector(protector))

	_, _ = w.Write([]byte(`{"password":"secret","keep":1}`))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out.String(), "secret") || !strings.Contains(out.String(), redactedValue) {
		t.Fatalf("oversized value did not fail closed: %s", out.String())
	}
}

func TestProtectionAuditReportsModesAndFixedFallbacksOnly(t *testing.T) {
	protector, _ := encryptionProtector()
	audit := &redactionAudit{}
	r := &redactor{
		headers:   lowerSet([]string{"authorization"}),
		protector: protector, audit: audit, direction: RequestBody,
	}
	_ = r.headerPairs(http.Header{"Authorization": {"header-secret"}}, "")

	var out bytes.Buffer

	session := newBodyValueProtector(protector)
	w := newJSONStreamRedactor(&out, lowerSet([]string{"password"}), session)

	_, _ = w.Write([]byte(`{"password":"body-secret"}`))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	protection, replacements := session.protectionReport()
	audit.setBody(RequestBody, BodyRedactionInfo{
		Kind: "builtin:json", Outcome: BodyRedactionRedacted,
		Replacements: &replacements, Protection: &protection,
	})

	info := audit.snapshot()
	if info.Request == nil || info.Request.Protection == nil || info.Request.Protection.Encrypted != 1 ||
		info.Request.Body == nil || info.Request.Body.Protection == nil || info.Request.Body.Protection.Encrypted != 1 {
		t.Fatalf("audit = %+v", info)
	}

	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}

	for _, forbidden := range []string{"header-secret", "body-secret", "enc-test", encryptedValuePrefix} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("audit leaked %q: %s", forbidden, encoded)
		}
	}

	limited := newSensitiveValueProtector(SensitiveValueProtection{Mode: ProtectionEncrypt, MaxValueBytes: 1})

	var fallbackOut bytes.Buffer

	fallbackSession := newBodyValueProtector(limited)
	fallbackWriter := newJSONStreamRedactor(&fallbackOut, lowerSet([]string{"password"}), fallbackSession)
	_, _ = fallbackWriter.Write([]byte(`{"password":"too-large"}`))
	_ = fallbackWriter.Close()

	fallback, _ := fallbackSession.protectionReport()
	if fallback.Redacted != 1 || fallback.Fallbacks["value_too_large"] != 1 {
		t.Fatalf("fallback report = %+v", fallback)
	}
}

func TestTokenizationStreamsValuesBeyondEncryptionBufferLimit(t *testing.T) {
	key := ProtectionKey{ID: "tok-stream", Key: bytes.Repeat([]byte{0x55}, 32)}
	protector := newSensitiveValueProtector(SensitiveValueProtection{
		Mode: ProtectionTokenize, MaxValueBytes: 1,
		KeyProvider: ProtectionKeyProviderFunc(func(ProtectionMode) (ProtectionKey, error) { return key, nil }),
	})
	secret := strings.Repeat("stream-secret-", 1<<16)

	var out bytes.Buffer

	w := newFormStreamRedactor(&out, lowerSet([]string{"token"}), newBodyValueProtector(protector))

	for offset := 0; offset < len(secret); offset += 17 {
		end := min(len(secret), offset+17)

		if _, err := w.Write(nil); err != nil { // exercise no-op writes too
			t.Fatal(err)
		}

		if offset == 0 {
			_, _ = w.Write([]byte("token="))
		}

		if _, err := w.Write([]byte(secret[offset:end])); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	token, err := url.QueryUnescape(strings.TrimPrefix(out.String(), "token="))
	if err != nil {
		t.Fatal(err)
	}

	ok, err := VerifyProtectedToken(token, []byte(secret), key)
	if err != nil || !ok {
		t.Fatalf("verify=%v err=%v token=%q", ok, err, token)
	}

	if len(w.protected.value) != 0 {
		t.Fatalf("tokenization retained %d plaintext bytes", len(w.protected.value))
	}
}

func TestRedactionAndCompletedEncryptionRetainNoMatchedPlaintext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protector *sensitiveValueProtector
	}{
		{"redact", newSensitiveValueProtector(SensitiveValueProtection{})},
		{"encrypt", func() *sensitiveValueProtector { p, _ := encryptionProtector(); return p }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			w := newJSONStreamRedactor(&out, lowerSet([]string{"password"}), newBodyValueProtector(tc.protector))

			_, _ = w.Write([]byte(`{"password":"memory-secret"}`))
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			if len(w.protected.value) != 0 {
				t.Fatalf("retained %d plaintext bytes", len(w.protected.value))
			}
		})
	}
}

func TestMultipartProtectionFailuresIncludeFilenameAndPayloads(t *testing.T) {
	protector := newSensitiveValueProtector(SensitiveValueProtection{Mode: ProtectionEncrypt})

	var out bytes.Buffer

	session := newBodyValueProtector(protector)

	w := newMultipartStreamRedactor(&out, multipartTestType, lowerSet([]string{"token", "upload"}), session)
	if _, err := w.Write([]byte(multipartFixture("secret"))); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	report, _ := session.protectionReport()
	if report.Redacted != 3 || report.Encrypted != 0 ||
		report.Fallbacks["encryption_failed"] != 3 {
		t.Fatalf("protection report = %+v", report)
	}

	err, count := session.protectionFailure()
	if err == nil || count != 3 {
		t.Fatalf("protection failure = %v, count = %d", err, count)
	}
}
