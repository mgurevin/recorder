package recorder

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHeaderRedactionCaseInsensitive(t *testing.T) {
	red := newRedactor(&Options{RedactHeaders: []string{"authorization", "X-API-KEY"}})
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
	red := newRedactor(&Options{RedactQueryParameters: []string{"TOKEN"}})
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
	red := newRedactor(&Options{RedactQueryParameters: []string{"token"}})
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
	red := newRedactor(&Options{RedactQueryParameters: []string{"token"}})
	got := red.redactURLString("/callback?token=secret&keep=1")
	if strings.Contains(got, "secret") || !strings.Contains(got, "keep=1") {
		t.Fatalf("relative URL was not safely redacted: %q", got)
	}
}

func TestRedactJSONNested(t *testing.T) {
	red := newRedactor(&Options{RedactJSONFields: []string{"password"}})
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

func TestRedactJSONInvalidInputUnchanged(t *testing.T) {
	red := newRedactor(&Options{RedactJSONFields: []string{"password"}})
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
	red := newRedactor(&Options{RedactHeaders: []string{"Cookie"}})
	if !red.cookieRedacted("session", "cookie") {
		t.Errorf("cookie not redacted when Cookie header is redacted")
	}
	red = newRedactor(&Options{RedactCookies: []string{"SESSION"}})
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
	red := newRedactor(&Options{RedactXMLElements: []string{"username", "PASSWORD"}})
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
	red := newRedactor(&Options{RedactXMLElements: []string{"secret"}})
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
	if !strings.Contains(out, "<inner>") {
		t.Errorf("subtree structure removed instead of its text: %s", out)
	}
}

func TestRedactXMLInvalidInputUnchanged(t *testing.T) {
	red := newRedactor(&Options{RedactXMLElements: []string{"password"}})
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
	red := newRedactor(&Options{
		RedactJSONFields:  []string{"password"},
		RedactXMLElements: []string{"password"},
	})
	if out := red.redactStructuredBody("application/json", []byte(`{"password":"x"}`)); strings.Contains(string(out), `"x"`) {
		t.Errorf("json not dispatched: %s", out)
	}
	for _, mt := range []string{"text/xml; charset=utf-8", "application/soap+xml", "application/xml"} {
		if out := red.redactStructuredBody(mt, []byte(`<password>x</password>`)); strings.Contains(string(out), ">x<") {
			t.Errorf("%s not dispatched: %s", mt, out)
		}
	}
	if out := red.redactStructuredBody("text/plain", []byte("password x")); string(out) != "password x" {
		t.Errorf("plain text must pass through: %s", out)
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
	red := newRedactor(&Options{RedactXMLElements: []string{"password", "secret"}})
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
	red := newRedactor(&Options{RedactJSONFields: []string{"password", "secret"}})
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
	red := newRedactor(&Options{RedactQueryParameters: []string{"token"}})
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
	red := newRedactor(&Options{RedactQueryParameters: []string{"token"}})
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
		io.WriteString(w, `<Envelope><Body><Session><Token>resp-secret</Token></Session></Body></Envelope>`)
	}))
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithRedactXMLElements("Username", "Password", "Token"))

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

func TestSanitizeURLHeaders(t *testing.T) {
	red := newRedactor(&Options{
		RedactHeaders:         []string{"Authorization"},
		RedactQueryParameters: []string{"token"},
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
		w.Write([]byte("ok"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	client, rec := newRecordedClient(ts, WithRedactQueryParameters("token"))

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
