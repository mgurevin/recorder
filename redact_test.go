package recorder

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func rulesRedactor(rules RedactionRules) *redactor {
	return newRedactor(&Options{Redaction: RedactionConfig{Common: rules}})
}

func TestHeaderRedactionCaseInsensitive(t *testing.T) {
	red := rulesRedactor(RedactionRules{Headers: []string{"authorization", "X-API-KEY"}})
	h := http.Header{}
	h.Set("Authorization", "Bearer abc")
	h.Set("X-Api-Key", "k")
	h.Set("Accept", "text/plain")
	pairs := red.headerPairs(h, "example.com")

	if pairs[0].Name != "Host" || pairs[0].Value != "example.com" {
		t.Errorf("host pair = %+v", pairs[0])
	}

	byName := map[string]string{}
	for _, p := range pairs {
		byName[p.Name] = p.Value
	}

	if byName["Authorization"] != redactedValue || byName["X-Api-Key"] != redactedValue {
		t.Errorf("pairs = %+v", pairs)
	}

	if byName["Accept"] != "text/plain" {
		t.Errorf("non-listed header touched: %+v", pairs)
	}
}

func TestHeaderPairsDeterministicOrder(t *testing.T) {
	red := testRedactor()
	h := http.Header{}
	h.Set("Zeta", "1")
	h.Set("Alpha", "2")
	h.Add("Alpha", "3")
	pairs := red.headerPairs(h, "")
	wantNames := []string{"Alpha", "Alpha", "Zeta"}

	if len(pairs) != 3 {
		t.Fatalf("pairs = %+v", pairs)
	}

	for i, w := range wantNames {
		if pairs[i].Name != w {
			t.Errorf("pair %d = %q, want %q", i, pairs[i].Name, w)
		}
	}
}

func TestQueryPairsOrderDuplicatesAndRedaction(t *testing.T) {
	red := rulesRedactor(RedactionRules{QueryParameters: []string{"TOKEN"}})
	pairs := red.queryPairs("b=2&a=1&a=3&token=s%20x&flag")

	want := []NameValuePair{
		{Name: "b", Value: "2"},
		{Name: "a", Value: "1"},
		{Name: "a", Value: "3"},
		{Name: "token", Value: redactedValue},
		{Name: "flag", Value: ""},
	}
	if len(pairs) != len(want) {
		t.Fatalf("pairs = %+v", pairs)
	}

	for i, w := range want {
		if pairs[i] != w {
			t.Errorf("pair %d = %+v, want %+v", i, pairs[i], w)
		}
	}
}

func TestRedactURL(t *testing.T) {
	red := rulesRedactor(RedactionRules{QueryParameters: []string{"token"}})
	u, _ := url.Parse("https://user:hunter2@example.com/path?token=verysecret&keep=1")

	s := red.redactURL(u)
	if strings.Contains(s, "verysecret") || strings.Contains(s, "hunter2") {
		t.Errorf("url leaked secrets: %s", s)
	}

	if !strings.Contains(s, "keep=1") {
		t.Errorf("non-secret query dropped: %s", s)
	}
	// The original URL must be untouched.
	if !strings.Contains(u.String(), "verysecret") {
		t.Errorf("original URL was mutated: %s", u.String())
	}
	// The result must remain a parseable URL.
	if _, err := url.Parse(s); err != nil {
		t.Errorf("redacted URL unparseable: %v", err)
	}
}

func TestRedactRelativeURLString(t *testing.T) {
	red := rulesRedactor(RedactionRules{QueryParameters: []string{"token"}})

	got := red.redactURLString("/callback?token=secret&keep=1")
	if strings.Contains(got, "secret") || !strings.Contains(got, "keep=1") {
		t.Fatalf("relative URL was not safely redacted: %q", got)
	}
}

func TestRedactJSONNested(t *testing.T) {
	red := rulesRedactor(RedactionRules{JSONFields: []string{"password"}})
	in := []byte(`{"password":"x","nested":{"Password":"y","keep":2},"list":[{"PASSWORD":"z"}],"n":1.5}`)

	out := red.redactJSONBody(in)
	if !json.Valid(out) {
		t.Fatalf("output invalid JSON: %s", out)
	}

	s := string(out)
	for _, leaked := range []string{`"x"`, `"y"`, `"z"`} {
		if strings.Contains(s, leaked) {
			t.Errorf("leaked %s in %s", leaked, s)
		}
	}

	if c := strings.Count(s, redactedValue); c != 3 {
		t.Errorf("redacted %d fields, want 3: %s", c, s)
	}

	if !strings.Contains(s, `"keep":2`) || !strings.Contains(s, "1.5") {
		t.Errorf("non-secret values altered: %s", s)
	}
}

func TestRedactJSONPreservesUnredactedBytes(t *testing.T) {
	red := rulesRedactor(RedactionRules{JSONFields: []string{"password", "secret"}})
	in := []byte(" \n{\n" +
		"  \"keepEscaped\": \"a\\u0020b\",\n" +
		"  \"number\": 1.2300e+04,\n" +
		"  \"pass\\u0077ord\" : { \"nested\": [1, true, null] },\n" +
		"  \"duplicate\": 1, \"duplicate\": 2,\n" +
		"  \"items\": [{\"secret\":false}, { \"ok\" : 3 }],\n" +
		"  \"tail\": \"unchanged\"\n" +
		"}\t")
	want := bytes.ReplaceAll(in, []byte(`{ "nested": [1, true, null] }`), []byte(`"[REDACTED]"`))
	want = bytes.ReplaceAll(want, []byte(`false`), []byte(`"[REDACTED]"`))

	out := red.redactJSONBody(in)
	if !bytes.Equal(out, want) {
		t.Fatalf("unredacted JSON bytes changed\n got: %s\nwant: %s", out, want)
	}

	if !json.Valid(out) {
		t.Fatalf("redacted output is invalid JSON: %s", out)
	}
}

func TestRedactJSONInvalidInputUnchanged(t *testing.T) {
	red := rulesRedactor(RedactionRules{JSONFields: []string{"password"}})
	in := []byte(`this is not json {password:`)

	out := red.redactJSONBody(in)
	if string(out) != string(in) {
		t.Errorf("invalid JSON must pass through unchanged")
	}
}

func TestRedactJSONNoFieldsConfigured(t *testing.T) {
	red := testRedactor()

	in := []byte(`{"password":"x"}`)
	if out := red.redactJSONBody(in); string(out) != string(in) {
		t.Errorf("no-op redaction changed body")
	}
}

func TestCookieRedactedViaCarrierHeader(t *testing.T) {
	red := rulesRedactor(RedactionRules{Headers: []string{"Cookie"}})
	if !red.cookieRedacted("session", "cookie") {
		t.Errorf("cookie not redacted when Cookie header is redacted")
	}

	red = rulesRedactor(RedactionRules{Cookies: []string{"SESSION"}})
	if !red.cookieRedacted("session", "cookie") {
		t.Errorf("cookie name matching not case-insensitive")
	}

	red = testRedactor()
	if red.cookieRedacted("session", "cookie") {
		t.Errorf("unconfigured cookie redacted")
	}
}

const soapEnvelope = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">
  <soapenv:Header>
    <wsse:Security>
      <wsse:UsernameToken>
        <wsse:Username>alice</wsse:Username>
        <wsse:Password Type="PasswordText">hunter2</wsse:Password>
      </wsse:UsernameToken>
    </wsse:Security>
  </soapenv:Header>
  <soapenv:Body>
    <GetBalance><AccountId>1234</AccountId></GetBalance>
  </soapenv:Body>
</soapenv:Envelope>`

func xmlWellFormed(b []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		_, err := dec.RawToken()
		if err == io.EOF {
			return true
		}

		if err != nil {
			return false
		}
	}
}

func TestRedactXMLSOAPEnvelope(t *testing.T) {
	red := rulesRedactor(RedactionRules{XMLElements: []string{"username", "PASSWORD"}})
	out := string(red.redactXMLBody([]byte(soapEnvelope)))

	for _, leaked := range []string{"alice", "hunter2"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q:\n%s", leaked, out)
		}
	}

	if c := strings.Count(out, redactedValue); c != 2 {
		t.Errorf("redacted %d values, want 2:\n%s", c, out)
	}
	// Everything else must survive byte-for-byte: namespaces, prefixes,
	// attributes, formatting, and non-secret content.
	for _, kept := range []string{
		`xmlns:wsse="http://docs.oasis-open.org`,
		`<wsse:Password Type="PasswordText">`,
		"<AccountId>1234</AccountId>",
		"  <soapenv:Header>",
	} {
		if !strings.Contains(out, kept) {
			t.Errorf("lost %q:\n%s", kept, out)
		}
	}

	if !xmlWellFormed([]byte(out)) {
		t.Errorf("output is not well-formed XML:\n%s", out)
	}
}

func TestRedactXMLSubtreeAndCDATA(t *testing.T) {
	red := rulesRedactor(RedactionRules{XMLElements: []string{"secret"}})
	in := `<r><Secret><inner>deep</inner>top</Secret><keep><![CDATA[safe]]></keep><Secret><![CDATA[raw&data]]></Secret></r>`

	out := string(red.redactXMLBody([]byte(in)))
	for _, leaked := range []string{"deep", "top", "raw&data"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q: %s", leaked, out)
		}
	}

	if !strings.Contains(out, "<![CDATA[safe]]>") {
		t.Errorf("non-secret CDATA altered: %s", out)
	}

	if strings.Contains(out, "<inner>") {
		t.Errorf("matched subtree structure was retained: %s", out)
	}
}

func TestRedactXMLInvalidInputUnchanged(t *testing.T) {
	red := rulesRedactor(RedactionRules{XMLElements: []string{"password"}})
	for _, in := range []string{"not xml at all", "<open><password>x</open>", ""} {
		// RawToken is lenient about tag mismatches; the guarantee under test
		// is only "never panic, always return usable bytes".
		out := red.redactXMLBody([]byte(in))
		if out == nil {
			t.Errorf("nil output for %q", in)
		}
	}

	if out := red.redactXMLBody([]byte("<a><b>keep</b></a>")); string(out) != "<a><b>keep</b></a>" {
		t.Errorf("document without matches was altered: %s", out)
	}
}

func TestRedactStructuredBodyDispatch(t *testing.T) {
	red := rulesRedactor(RedactionRules{
		JSONFields:  []string{"password"},
		XMLElements: []string{"password"},
	})
	if out := red.redactStructuredBody("application/json", []byte(`{"password":"x"}`)); strings.Contains(string(out), `"x"`) {
		t.Errorf("json not dispatched: %s", out)
	}

	for _, mt := range []string{"text/xml; charset=utf-8", "application/soap+xml", "application/xml"} {
		if out := red.redactStructuredBody(mt, []byte(`<password>x</password>`)); strings.Contains(string(out), ">x<") {
			t.Errorf("%s not dispatched: %s", mt, out)
		}
	}

	if out := red.redactStructuredBody("text/plain; charset=utf-8", []byte(`<?xml version="1.0"?><root><password>x</password></root>`)); strings.Contains(string(out), ">x<") {
		t.Errorf("mislabelled XML not sniffed: %s", out)
	}

	if out := red.redactStructuredBody("text/plain", []byte(`{"password":"x","keep":1}`)); strings.Contains(string(out), `"x"`) {
		t.Errorf("mislabelled JSON not sniffed: %s", out)
	}

	if out := red.redactStructuredBody("text/plain", []byte("password x")); string(out) != "password x" {
		t.Errorf("plain text must pass through: %s", out)
	}

	if out := red.redactStructuredBody("application/octet-stream", []byte("<not-closed>")); string(out) != "<not-closed>" {
		t.Errorf("malformed XML-like bytes must pass through: %s", out)
	}
}

func TestContentClassification(t *testing.T) {
	textual := []string{
		"text/plain", "text/html; charset=utf-8", "application/json",
		"application/xml", "application/x-www-form-urlencoded",
		"application/hal+json", "application/atom+xml", "image/svg+xml",
	}
	for _, mt := range textual {
		if !isTextualMime(mt) {
			t.Errorf("%s should be textual", mt)
		}
	}

	binary := []string{
		"application/octet-stream", "application/pdf", "image/png",
		"application/x-protobuf", "", "audio/mpeg",
	}
	for _, mt := range binary {
		if isTextualMime(mt) {
			t.Errorf("%s should be binary", mt)
		}
	}

	// Textual type + valid UTF-8: stored as plain text.
	if text, enc := contentText("text/plain", []byte("naïve café ünïcode")); enc != "" || text != "naïve café ünïcode" {
		t.Errorf("utf8 text: %q %q", text, enc)
	}
	// Textual type but invalid UTF-8 (e.g. Latin-1): must fall back to Base64.
	latin1 := []byte{'c', 'a', 'f', 0xe9}
	if _, enc := contentText("text/plain", latin1); enc != "base64" {
		t.Errorf("non-UTF-8 text stored without base64")
	}
	// Binary content: Base64 roundtrip.
	bin := []byte{0x00, 0xff, 0x10}

	text, enc := contentText("application/pdf", bin)
	if enc != "base64" {
		t.Fatalf("encoding = %q", enc)
	}

	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil || string(decoded) != string(bin) {
		t.Errorf("base64 roundtrip failed")
	}
}

func FuzzRedactXML(f *testing.F) {
	f.Add(soapEnvelope)
	f.Add("<a><password>x</password></a>")
	f.Add("<a/>")
	f.Add("not xml")
	f.Add("<a><![CDATA[x]]></a>")

	red := rulesRedactor(RedactionRules{XMLElements: []string{"password", "secret"}})

	f.Fuzz(func(t *testing.T, in string) {
		out := red.redactXMLBody([]byte(in))
		if xmlWellFormed([]byte(in)) && !xmlWellFormed(out) {
			t.Fatalf("well-formed input produced malformed output:\nin:  %q\nout: %q", in, out)
		}
	})
}

func FuzzRedactJSON(f *testing.F) {
	f.Add([]byte(`{"password":"x","a":[1,2,{"password":null}]}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`123`))

	red := rulesRedactor(RedactionRules{JSONFields: []string{"password", "secret"}})

	f.Fuzz(func(t *testing.T, data []byte) {
		out := red.redactJSONBody(data)
		if json.Valid(data) && !json.Valid(out) {
			t.Fatalf("valid input produced invalid output: %q -> %q", data, out)
		}
	})
}

func FuzzQueryPairs(f *testing.F) {
	f.Add("a=1&b=2")
	f.Add("%zz=broken&&=empty&flag")
	f.Add("")

	red := rulesRedactor(RedactionRules{QueryParameters: []string{"token"}})

	f.Fuzz(func(t *testing.T, raw string) {
		pairs := red.queryPairs(raw)
		for _, p := range pairs {
			if p.Name == "token" && p.Value != redactedValue {
				t.Fatalf("token leaked: %+v", p)
			}
		}
	})
}

func FuzzRedactURL(f *testing.F) {
	f.Add("https://u:p@h/p?token=s&x=1")
	f.Add("http://example.com")
	f.Add("//weird?token")

	red := rulesRedactor(RedactionRules{QueryParameters: []string{"token"}})

	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}

		_ = red.redactURL(u) // must not panic
	})
}

func FuzzContentClassification(f *testing.F) {
	f.Add("text/plain", []byte("hello"))
	f.Add("application/octet-stream", []byte{0, 1, 2})
	f.Add("", []byte(nil))
	f.Fuzz(func(t *testing.T, mimeType string, data []byte) {
		text, enc := contentText(mimeType, data)
		switch enc {
		case "base64":
			decoded, err := base64.StdEncoding.DecodeString(text)
			if err != nil || string(decoded) != string(data) {
				t.Fatalf("base64 roundtrip failed for %q", mimeType)
			}

		case "":
			if text != string(data) || !utf8.ValidString(text) {
				t.Fatalf("plain text mismatch for %q", mimeType)
			}

		default:
			t.Fatalf("unknown encoding %q", enc)
		}
	})
}

// TestSOAPRedactionEndToEnd drives a SOAP 1.1 POST through the transport:
// the server must receive the real credentials while the recorded entry
// carries only redacted ones, in both directions.
func TestSOAPRedactionEndToEnd(t *testing.T) {
	var serverGot []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverGot, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		testWriteString(w, `<Envelope><Body><Session><Token>resp-secret</Token></Session></Body></Envelope>`)
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts, WithRedaction(RedactionConfig{Common: RedactionRules{XMLElements: []string{"Username", "Password", "Token"}}}))

	resp, err := client.Post(ts.URL, "text/xml; charset=utf-8", strings.NewReader(soapEnvelope))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	mustReadAll(t, resp.Body)

	if !strings.Contains(string(serverGot), "hunter2") {
		t.Fatalf("redaction must not touch the real request; server saw:\n%s", serverGot)
	}

	e := singleEntry(t, rec)

	pd := e.Request.PostData
	if pd == nil {
		t.Fatal("postData missing")
	}

	for _, leaked := range []string{"alice", "hunter2"} {
		if strings.Contains(pd.Text, leaked) {
			t.Errorf("request record leaked %q", leaked)
		}
	}

	if !strings.Contains(pd.Text, "<AccountId>1234</AccountId>") {
		t.Errorf("non-secret request content lost:\n%s", pd.Text)
	}

	if strings.Contains(e.Response.Content.Text, "resp-secret") {
		t.Errorf("response record leaked token:\n%s", e.Response.Content.Text)
	}

	if !strings.Contains(e.Response.Content.Text, redactedValue) {
		t.Errorf("response token not redacted:\n%s", e.Response.Content.Text)
	}
	// The hash still covers the real (pre-redaction) bytes.
	if e.RequestBody.Hash != sha256Hex([]byte(soapEnvelope)) {
		t.Errorf("request hash must be computed over the wire bytes")
	}
}

func TestResponseBodyHashAndCountsMatchCallerBytesWithAndWithoutRedaction(t *testing.T) {
	const payload = `{"password":"response-secret","keep":"unchanged"}`

	tests := []struct {
		name     string
		redacted bool
		opts     []Option
	}{
		{name: "without redaction"},
		{name: "with redaction", redacted: true, opts: []Option{WithRedaction(RedactionConfig{Common: RedactionRules{JSONFields: []string{"password"}}})}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				testWriteString(w, payload)
			}))
			defer ts.Close()

			client, rec := newRecordedClient(ts, tc.opts...)

			resp, err := client.Get(ts.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}

			callerBody := mustReadAll(t, resp.Body)
			e := singleEntry(t, rec)

			info := e.ResponseBody
			if info == nil {
				t.Fatal("response body metadata missing")
			}

			if got, want := info.Hash, sha256Hex(callerBody); got != want {
				t.Fatalf("recorder hash = %q, caller body hash = %q", got, want)
			}

			if got, want := info.TotalBytes, int64(len(callerBody)); got != want {
				t.Fatalf("total bytes = %d, caller body bytes = %d", got, want)
			}

			if got, want := info.CapturedBytes, int64(len(callerBody)); got != want {
				t.Fatalf("captured bytes = %d, caller body bytes = %d", got, want)
			}

			if info.HashAlgorithm != "sha256" || !info.Complete || info.Truncated {
				t.Fatalf("response body metadata = %+v", info)
			}

			recorded := e.Response.Content.Text
			if tc.redacted {
				if strings.Contains(recorded, "response-secret") || !strings.Contains(recorded, redactedValue) {
					t.Fatalf("recorded body was not redacted: %q", recorded)
				}
			} else if recorded != string(callerBody) {
				t.Fatalf("recorded body = %q, caller body = %q", recorded, callerBody)
			}
		})
	}
}

func TestEncryptedResponseBodyDecryptsByteForByteToHTTPClientBody(t *testing.T) {
	payload := []byte("{\n  \"keep\" : 1.2300,\n  \"password\" : { \"nested\" : [true, null, \"x\\\\ny\"] },\n  \"tail\" : \"unchanged\"\n}")
	key := ProtectionKey{ID: "response-test", Key: bytes.Repeat([]byte{0x6a}, 32)}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts,
		WithRedaction(RedactionConfig{Common: RedactionRules{JSONFields: []string{"password"}}}),
		WithSensitiveValueProtection(SensitiveValueProtection{
			Mode: ProtectionEncrypt,
			KeyProvider: ProtectionKeyProviderFunc(func(mode ProtectionMode) (ProtectionKey, error) {
				if mode != ProtectionEncrypt {
					return ProtectionKey{}, errors.New("unexpected protection mode")
				}

				return key, nil
			}),
		}),
	)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	callerBody := mustReadAll(t, resp.Body)
	if !bytes.Equal(callerBody, payload) {
		t.Fatalf("HTTP client body changed:\n got: %q\nwant: %q", callerBody, payload)
	}

	e := singleEntry(t, rec)

	recorded := []byte(e.Response.Content.Text)
	if bytes.Equal(recorded, callerBody) || bytes.Contains(recorded, []byte(`"nested"`)) {
		t.Fatalf("recorded response was not encrypted: %q", recorded)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(recorded, &document); err != nil {
		t.Fatalf("parse recorded JSON: %v", err)
	}

	var token string
	if err := json.Unmarshal(document["password"], &token); err != nil {
		t.Fatalf("parse encrypted token: %v", err)
	}

	decryptedValue, err := DecryptProtectedValue(token, key)
	if err != nil {
		t.Fatalf("decrypt recorded value: %v", err)
	}

	reconstructed := bytes.Replace(recorded, document["password"], decryptedValue, 1)
	if !bytes.Equal(reconstructed, callerBody) {
		t.Fatalf("decrypted recording differs from HTTP client body:\n got: %q\nwant: %q", reconstructed, callerBody)
	}

	info := e.ResponseBody
	if info == nil || info.Hash != sha256Hex(callerBody) ||
		info.TotalBytes != int64(len(callerBody)) || info.CapturedBytes != int64(len(callerBody)) ||
		!info.Complete || info.Truncated {
		t.Fatalf("response body metadata = %+v", info)
	}

	if e.Redaction == nil || e.Redaction.Response == nil || e.Redaction.Response.Body == nil ||
		e.Redaction.Response.Body.Protection == nil || e.Redaction.Response.Body.Protection.Encrypted != 1 {
		t.Fatalf("protection audit = %+v", e.Redaction)
	}
}

func TestEncryptedXMLResponseHeadersAndCookiesDecryptToHTTPClientValues(t *testing.T) {
	payload := []byte("<?xml version=\"1.0\"?>\n<response>\n  <keep a=\"1\">unchanged</keep>\n  <password>secret<![CDATA[<raw>&value]]><nested x=\"y\"/></password>\n</response>\n")
	key := ProtectionKey{ID: "xml-response-test", Key: bytes.Repeat([]byte{0x7b}, 32)}

	const (
		secretHeader = "header-secret; formatting=preserved"
		secretCookie = "cookie-secret"
		setCookie    = "session=" + secretCookie + "; Path=/; HttpOnly"
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		w.Header().Set("X-Response-Secret", secretHeader)
		w.Header().Add("Set-Cookie", setCookie)
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts,
		WithRedaction(RedactionConfig{Common: RedactionRules{
			Headers:     []string{"X-Response-Secret"},
			Cookies:     []string{"session"},
			XMLElements: []string{"password"},
		}}),
		WithSensitiveValueProtection(SensitiveValueProtection{
			Mode: ProtectionEncrypt,
			KeyProvider: ProtectionKeyProviderFunc(func(mode ProtectionMode) (ProtectionKey, error) {
				if mode != ProtectionEncrypt {
					return ProtectionKey{}, errors.New("unexpected protection mode")
				}

				return key, nil
			}),
		}),
	)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	callerHeader := resp.Header.Get("X-Response-Secret")
	callerSetCookie := resp.Header.Get("Set-Cookie")
	callerCookies := resp.Cookies()

	callerBody := mustReadAll(t, resp.Body)
	if !bytes.Equal(callerBody, payload) {
		t.Fatalf("HTTP client XML body changed:\n got: %q\nwant: %q", callerBody, payload)
	}

	if callerHeader != secretHeader || callerSetCookie != setCookie || len(callerCookies) != 1 || callerCookies[0].Value != secretCookie {
		t.Fatalf("HTTP client values changed: header=%q set-cookie=%q cookies=%+v", callerHeader, callerSetCookie, callerCookies)
	}

	e := singleEntry(t, rec)
	recordedBody := []byte(e.Response.Content.Text)
	open := []byte("<password>")
	close := []byte("</password>")
	start := bytes.Index(recordedBody, open)

	end := bytes.Index(recordedBody, close)
	if start < 0 || end < 0 || end < start+len(open) {
		t.Fatalf("recorded XML does not contain protected element: %q", recordedBody)
	}

	tokenBytes := recordedBody[start+len(open) : end]

	decryptedXMLValue, err := DecryptProtectedValue(string(tokenBytes), key)
	if err != nil {
		t.Fatalf("decrypt XML value: %v", err)
	}

	reconstructed := append([]byte(nil), recordedBody[:start+len(open)]...)
	reconstructed = append(reconstructed, decryptedXMLValue...)

	reconstructed = append(reconstructed, recordedBody[end:]...)
	if !bytes.Equal(reconstructed, callerBody) {
		t.Fatalf("decrypted XML differs from HTTP client body:\n got: %q\nwant: %q", reconstructed, callerBody)
	}

	recordedHeader, ok := findHeader(e.Response.Headers, "X-Response-Secret")
	if !ok {
		t.Fatal("recorded response header missing")
	}

	decryptTestToken(t, recordedHeader, key, callerHeader)

	recordedSetCookie, ok := findHeader(e.Response.Headers, "Set-Cookie")
	if !ok {
		t.Fatal("recorded Set-Cookie header missing")
	}

	decryptTestToken(t, recordedSetCookie, key, callerSetCookie)

	if len(e.Response.Cookies) != 1 {
		t.Fatalf("recorded cookies = %+v", e.Response.Cookies)
	}

	decryptTestToken(t, e.Response.Cookies[0].Value, key, callerCookies[0].Value)

	info := e.ResponseBody
	if info == nil || info.Hash != sha256Hex(callerBody) ||
		info.TotalBytes != int64(len(callerBody)) || info.CapturedBytes != int64(len(callerBody)) ||
		!info.Complete || info.Truncated {
		t.Fatalf("response body metadata = %+v", info)
	}

	if e.Redaction == nil || e.Redaction.Response == nil || e.Redaction.Response.Body == nil ||
		e.Redaction.Response.Body.Protection == nil || e.Redaction.Response.Body.Protection.Encrypted != 1 ||
		e.Redaction.Response.Protection == nil || e.Redaction.Response.Protection.Encrypted != 3 {
		t.Fatalf("protection audit = %+v", e.Redaction)
	}
}

func TestEncryptionFailuresAreAggregatedThroughInternalErrorPolicy(t *testing.T) {
	const failures = 2002

	var payload strings.Builder
	payload.WriteByte('[')

	for i := 0; i < failures; i++ {
		if i > 0 {
			payload.WriteByte(',')
		}

		payload.WriteString(`{"password":"secret"}`)
	}

	payload.WriteByte(']')

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload.String())
	}))
	defer ts.Close()

	var (
		mu       sync.Mutex
		logs     []string
		internal []error
	)

	kmsErr := errors.New("test KMS unavailable")
	client, rec := newRecordedClient(ts,
		WithRedaction(RedactionConfig{Common: RedactionRules{
			Headers:    []string{"X-Request-Secret"},
			JSONFields: []string{"password"},
		}}),
		WithSensitiveValueProtection(SensitiveValueProtection{
			Mode: ProtectionEncrypt,
			KeyProvider: ProtectionKeyProviderFunc(func(ProtectionMode) (ProtectionKey, error) {
				return ProtectionKey{}, kmsErr
			}),
		}),
		WithInternalErrorMode(InternalErrorLog),
		WithLogf(func(format string, args ...any) {
			mu.Lock()

			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		}),
		WithOnInternalError(func(err error) {
			mu.Lock()

			internal = append(internal, err)
			mu.Unlock()
		}),
	)

	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("X-Request-Secret", "request-secret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	callerBody := mustReadAll(t, resp.Body)
	if !bytes.Equal(callerBody, []byte(payload.String())) {
		t.Fatal("HTTP client body changed after encryption failures")
	}

	mu.Lock()

	gotLogs := append([]string(nil), logs...)
	gotInternal := append([]error(nil), internal...)
	mu.Unlock()

	if len(gotLogs) != 2 || !containsProtectionFailure(gotLogs, "request", 1, kmsErr.Error()) ||
		!containsProtectionFailure(gotLogs, "response", failures, kmsErr.Error()) {
		t.Fatalf("aggregated logs = %q", gotLogs)
	}

	if len(gotInternal) != 2 || !errors.Is(gotInternal[0], kmsErr) || !errors.Is(gotInternal[1], kmsErr) ||
		!containsProtectionErrors(gotInternal, "request", 1) || !containsProtectionErrors(gotInternal, "response", failures) {
		t.Fatalf("aggregated internal errors = %v", gotInternal)
	}

	e := singleEntry(t, rec)

	requestAudit := e.Redaction.Request.Protection
	if requestAudit == nil || requestAudit.Redacted != 1 || requestAudit.Fallbacks["encryption_failed"] != 1 {
		t.Fatalf("request protection audit = %+v", requestAudit)
	}

	bodyAudit := e.Redaction.Response.Body.Protection
	if bodyAudit == nil || bodyAudit.Redacted != failures || bodyAudit.Encrypted != 0 ||
		bodyAudit.Fallbacks["encryption_failed"] != failures {
		t.Fatalf("body protection audit = %+v", bodyAudit)
	}
}

func containsProtectionFailure(logs []string, direction string, count int, cause string) bool {
	wantCount := fmt.Sprintf("%d value(s)", count)
	for _, entry := range logs {
		if strings.Contains(entry, direction) && strings.Contains(entry, wantCount) && strings.Contains(entry, cause) {
			return true
		}
	}

	return false
}

func containsProtectionErrors(errs []error, direction string, count int) bool {
	wantCount := fmt.Sprintf("%d value(s)", count)
	for _, err := range errs {
		if strings.Contains(err.Error(), direction) && strings.Contains(err.Error(), wantCount) {
			return true
		}
	}

	return false
}

func TestEncryptionValueLimitDoesNotReportInternalError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"password":"larger-than-limit"}`)
	}))
	defer ts.Close()

	var internal atomic.Int64

	client, _ := newRecordedClient(ts,
		WithRedaction(RedactionConfig{Common: RedactionRules{JSONFields: []string{"password"}}}),
		WithSensitiveValueProtection(SensitiveValueProtection{
			Mode: ProtectionEncrypt, MaxValueBytes: 1,
			KeyProvider: ProtectionKeyProviderFunc(func(ProtectionMode) (ProtectionKey, error) {
				return ProtectionKey{ID: "unused", Key: bytes.Repeat([]byte{1}, 32)}, nil
			}),
		}),
		WithInternalErrorMode(InternalErrorLog),
		WithLogf(func(string, ...any) { internal.Add(1) }),
		WithOnInternalError(func(error) { internal.Add(1) }),
	)

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	_ = mustReadAll(t, resp.Body)
	if internal.Load() != 0 {
		t.Fatalf("value_too_large reported as internal error %d time(s)", internal.Load())
	}
}

func TestSanitizeURLHeaders(t *testing.T) {
	red := rulesRedactor(RedactionRules{
		Headers:         []string{"Authorization"},
		QueryParameters: []string{"token"},
	})

	pairs := red.sanitizeURLHeaders([]NameValuePair{
		{Name: "Location", Value: "/next?token=s1&keep=1"},
		{Name: "Content-Location", Value: "https://h/doc?token=s2"},
		{Name: "referer", Value: "https://h/prev?token=s3&ok=2"}, // HTTP/2 lowercase
		{Name: "Authorization", Value: redactedValue},            // already redacted: untouched
		{Name: "Accept", Value: "text/plain?token=notaurlfield"}, // not URL-bearing: untouched
	})
	for _, p := range pairs[:3] {
		if strings.Contains(p.Value, "s1") || strings.Contains(p.Value, "s2") || strings.Contains(p.Value, "s3") {
			t.Errorf("%s leaked: %q", p.Name, p.Value)
		}
	}

	if !strings.Contains(pairs[0].Value, "keep=1") || !strings.Contains(pairs[2].Value, "ok=2") {
		t.Errorf("non-secret query dropped: %+v", pairs[:3])
	}

	if pairs[3].Value != redactedValue {
		t.Errorf("fully redacted value rewritten: %q", pairs[3].Value)
	}

	if pairs[4].Value != "text/plain?token=notaurlfield" {
		t.Errorf("non-URL header rewritten: %q", pairs[4].Value)
	}
}

// TestRefererQueryRedacted: on redirect hops Go's client forwards the
// previous URL — including its query — as the Referer header. Query
// redaction must reach it.
func TestRefererQueryRedacted(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b", http.StatusFound)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		testWrite(w, []byte("ok"))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	client, rec := newRecordedClient(ts, WithRedaction(RedactionConfig{Common: RedactionRules{QueryParameters: []string{"token"}}}))

	resp, err := client.Get(ts.URL + "/a?token=referer-secret&ok=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	mustReadAll(t, resp.Body)

	entries := rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}

	referer, ok := findHeader(entries[1].Request.Headers, "Referer")
	if !ok {
		t.Fatal("second hop has no Referer header (test setup)")
	}

	if strings.Contains(referer, "referer-secret") {
		t.Fatalf("Referer leaked the redacted query value: %q", referer)
	}

	if !strings.Contains(referer, "ok=1") {
		t.Errorf("non-secret query dropped from Referer: %q", referer)
	}
}

func TestRawTraceDetailsUseCentralErrorRedactor(t *testing.T) {
	red := newRedactor(&Options{RedactErrorMessage: func(s string) string {
		return strings.ReplaceAll(s, "trace-secret", redactedValue)
	}})
	original := []TraceEvent{{Name: "ConnectDone", Detail: "dial failed: trace-secret"}}

	got := red.traceEvents(original)
	if strings.Contains(got[0].Detail, "trace-secret") || !strings.Contains(got[0].Detail, redactedValue) {
		t.Fatalf("trace detail was not redacted: %+v", got)
	}

	if original[0].Detail != "dial failed: trace-secret" {
		t.Fatalf("trace redaction mutated collector snapshot: %+v", original)
	}
}

func TestRedactionAuditReportsChangesWithoutSensitiveRuleNames(t *testing.T) {
	const (
		requestSecret  = "request-secret"
		responseSecret = "response-secret"
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testCopy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/next?token=location-secret")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "cookie-secret", Path: "/"})
		testWriteString(w, `{"password":"`+responseSecret+`","keep":2}`)
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts,
		WithRedaction(RedactionConfig{Common: RedactionRules{
			QueryParameters: []string{"token"},
			Cookies:         []string{"session"},
			JSONFields:      []string{"password"},
		}}),
	)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/pay?token=query-secret", strings.NewReader(`{"password":"`+requestSecret+`","keep":1}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer header-secret")
	req.AddCookie(&http.Cookie{Name: "session", Value: "request-cookie-secret"})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	callerBody := mustReadAll(t, resp.Body)
	if !bytes.Contains(callerBody, []byte(responseSecret)) {
		t.Fatalf("caller body was redacted: %q", callerBody)
	}

	e := singleEntry(t, rec)

	audit := e.Redaction
	if audit == nil || audit.Request == nil || audit.Response == nil {
		t.Fatalf("redaction audit missing: %+v", audit)
	}

	if audit.Request.URL < 1 || audit.Request.QueryParameters < 1 || audit.Request.Headers < 1 || audit.Request.Cookies < 1 {
		t.Fatalf("request audit = %+v", audit.Request)
	}

	if audit.Response.URL < 1 || audit.Response.Headers < 1 || audit.Response.Cookies < 1 {
		t.Fatalf("response audit = %+v", audit.Response)
	}

	for direction, body := range map[string]*BodyRedactionInfo{
		"request":  audit.Request.Body,
		"response": audit.Response.Body,
	} {
		if body == nil || body.Kind != "builtin:json" || body.Outcome != "redacted" || body.Replacements == nil || *body.Replacements != 1 {
			t.Fatalf("%s body audit = %+v", direction, body)
		}
	}

	encoded, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}

	for _, sensitive := range []string{"password", "token", requestSecret, responseSecret, "header-secret", "cookie-secret"} {
		if bytes.Contains(encoded, []byte(sensitive)) {
			t.Fatalf("audit leaked %q: %s", sensitive, encoded)
		}
	}
}

func TestRedactionAuditSeparatesErrorsAndRawTrace(t *testing.T) {
	audit := &redactionAudit{}

	red := newRedactor(&Options{RedactErrorMessage: func(s string) string {
		return strings.ReplaceAll(s, "secret", redactedValue)
	}}).withAudit(audit, RequestBody)
	if got := red.redactError("error secret"); strings.Contains(got, "secret") {
		t.Fatalf("error not redacted: %q", got)
	}

	red.traceEvents([]TraceEvent{{Name: "event", Detail: "trace secret"}})

	info := audit.snapshot()
	if info == nil || info.Errors != 1 || info.RawTrace != 1 {
		t.Fatalf("audit = %+v", info)
	}
}

func TestRedactionAuditSnapshotIsImmutableAndConcurrentSafe(t *testing.T) {
	audit := &redactionAudit{}

	const workers = 64

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			audit.add(RequestBody, "headers", 1)
			audit.add(ResponseBody, "cookies", 1)
		}()
	}

	wg.Wait()

	first := audit.snapshot()
	if first == nil || first.Request == nil || first.Response == nil || first.Request.Headers != workers || first.Response.Cookies != workers {
		t.Fatalf("snapshot = %+v", first)
	}

	audit.add(RequestBody, "headers", 1)
	audit.setBody(ResponseBody, BodyRedactionInfo{Kind: "custom", Outcome: BodyRedactionUnchanged})

	if first.Request.Headers != workers || first.Response.Body != nil {
		t.Fatalf("previous snapshot mutated: %+v", first)
	}
}
