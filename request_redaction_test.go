package recorder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestWithRequestRedactionMergesAndCopiesRules(t *testing.T) {
	firstRedactor := &markerBodyRedactor{marker: "first"}
	lastRedactor := &markerBodyRedactor{marker: "last"}
	first := RedactionConfig{
		Request: RedactionRules{
			Headers:       []string{"X-First"},
			BodyRedactors: map[string]BodyRedactor{"TEXT/CSV; charset=utf-8": firstRedactor},
		},
	}
	ctx := WithRequestRedaction(context.Background(), first)

	first.Request.Headers[0] = "mutated"
	first.Request.BodyRedactors["text/csv"] = lastRedactor

	ctx = WithRequestRedaction(ctx, RedactionConfig{
		Request: RedactionRules{
			Headers:       []string{"X-Second"},
			BodyRedactors: map[string]BodyRedactor{"text/csv": lastRedactor},
		},
		Response: RedactionRules{JSONFields: []string{"responseSecret"}},
	})

	got := requestRedactionFromContext(ctx)
	if strings.Join(got.Request.Headers, ",") != "X-First,X-Second" {
		t.Fatalf("request headers = %v", got.Request.Headers)
	}

	if got.Request.BodyRedactors["text/csv"] != lastRedactor {
		t.Fatal("most recent normalized MIME redactor did not win")
	}

	if len(got.Response.JSONFields) != 1 || got.Response.JSONFields[0] != "responseSecret" {
		t.Fatalf("response rules = %+v", got.Response)
	}
}

func TestWithRedactionUsesCommonAndDirectionalRules(t *testing.T) {
	options := DefaultConfig()
	withRedaction(RedactionConfig{
		Common:   RedactionRules{Headers: []string{"X-Common"}},
		Request:  RedactionRules{Headers: []string{"X-Request"}},
		Response: RedactionRules{Headers: []string{"X-Response"}},
	})(&options)

	request := newRedactor(&options)
	response := newRedactorWithRules(&options,
		effectiveRedactionRules(options.Redaction.Common, options.Redaction.Response))

	for _, name := range []string{"authorization", "x-common", "x-request"} {
		if !request.headerRedacted(name) {
			t.Fatalf("request redactor missing %q", name)
		}
	}

	if request.headerRedacted("x-response") {
		t.Fatal("response-only rule applied to request")
	}

	for _, name := range []string{"authorization", "x-common", "x-response"} {
		if !response.headerRedacted(name) {
			t.Fatalf("response redactor missing %q", name)
		}
	}

	if response.headerRedacted("x-request") {
		t.Fatal("request-only rule applied to response")
	}
}

func TestRequestWithRedactionClonesRequest(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	clone := RequestWithRedaction(req, RedactionConfig{
		Request: RedactionRules{Headers: []string{"X-Secret"}},
	})
	if clone == req {
		t.Fatal("request was not cloned")
	}

	if got := requestRedactionFromContext(req.Context()); len(got.Request.Headers) != 0 {
		t.Fatalf("original request context changed: %+v", got)
	}

	if got := requestRedactionFromContext(clone.Context()); len(got.Request.Headers) != 1 {
		t.Fatalf("clone rules = %+v", got)
	}

	if RequestWithRedaction(nil, RedactionConfig{}) != nil {
		t.Fatal("nil request did not remain nil")
	}
}

func TestRequestScopedRedactionIsAdditiveAndDirectional(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()

		w.Header().Set("X-Global-Response", "global-response-header")
		w.Header().Set("X-Local-Response", "local-response-header")
		http.SetCookie(w, &http.Cookie{Name: "response_session", Value: "response-cookie"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"global":"global-response","shared":"shared-response","responseOnly":"local-response","keep":"visible"}`)
	}))
	defer server.Close()

	record := NewMemoryRecorder()
	transport := NewTransport(http.DefaultTransport, record, Config{
		CaptureRequestBody:  true,
		CaptureResponseBody: true,
		EmbedBodies:         true,
		CaptureHeaders:      true,
		CaptureCookies:      true,
		Redaction: RedactionConfig{Common: RedactionRules{
			Headers:         []string{"Cookie", "Set-Cookie", "X-Global-Request", "X-Global-Response"},
			QueryParameters: []string{"globalQuery"},
			Cookies:         []string{"global_session"},
			JSONFields:      []string{"global"},
		}},
		MaxRequestBodyBytes:  1 << 20,
		MaxResponseBodyBytes: 1 << 20,
	})
	client := &http.Client{Transport: transport}

	req, err := http.NewRequest(http.MethodPost,
		server.URL+"?globalQuery=global-query&localQuery=local-query&keep=visible",
		strings.NewReader(`{"global":"global-request","shared":"shared-request","requestOnly":"local-request","keep":"visible"}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Global-Request", "global-request-header")
	req.Header.Set("X-Local-Request", "local-request-header")
	req.AddCookie(&http.Cookie{Name: "global_session", Value: "global-cookie"})
	req.AddCookie(&http.Cookie{Name: "local_session", Value: "local-cookie"})
	req = RequestWithRedaction(req, RedactionConfig{
		Common: RedactionRules{JSONFields: []string{"shared"}},
		Request: RedactionRules{
			Headers:         []string{"X-Local-Request"},
			QueryParameters: []string{"localQuery"},
			Cookies:         []string{"local_session"},
			JSONFields:      []string{"requestOnly"},
		},
		Response: RedactionRules{
			Headers:    []string{"X-Local-Response"},
			Cookies:    []string{"response_session"},
			JSONFields: []string{"responseOnly"},
		},
	})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}

	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	entry := singleEntry(t, record)
	assertAbsent(t, entry.Request.URL,
		"global-query", "local-query")
	assertAbsent(t, entry.Request.PostData.Text,
		"global-request", "shared-request", "local-request")
	assertAbsent(t, entry.Response.Content.Text,
		"global-response", "shared-response", "local-response")
	assertPairsAbsent(t, entry.Request.Headers,
		"global-request-header", "local-request-header", "global-cookie", "local-cookie")
	assertCookiesAbsent(t, entry.Request.Cookies, "global-cookie", "local-cookie")
	assertPairsAbsent(t, entry.Response.Headers,
		"global-response-header", "local-response-header", "response-cookie")
	assertCookiesAbsent(t, entry.Response.Cookies, "response-cookie")

	for label, text := range map[string]string{
		"request URL":   entry.Request.URL,
		"request body":  entry.Request.PostData.Text,
		"response body": entry.Response.Content.Text,
	} {
		if !strings.Contains(text, "visible") {
			t.Fatalf("%s lost unselected content: %q", label, text)
		}
	}
}

func TestRequestScopedRedactionFollowsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final?hopSecret=redirect-value", http.StatusFound)

			return
		}

		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	record := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, record, DefaultConfig())}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}

	req = RequestWithRedaction(req, RedactionConfig{
		Request:  RedactionRules{QueryParameters: []string{"hopSecret"}},
		Response: RedactionRules{QueryParameters: []string{"hopSecret"}},
	})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	entries := record.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	for _, entry := range entries {
		assertAbsent(t, entry.Request.URL, "redirect-value")
		assertAbsent(t, entry.Response.RedirectURL, "redirect-value")
	}
}

func TestRequestScopedRedactionIsIsolatedAcrossConcurrentRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"alpha":"alpha-value","beta":"beta-value"}`)
	}))
	defer server.Close()

	record := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, record, configWith(withCaptureResponseBody(true),
		withEmbedBodies(true)),
	)}

	const requests = 32

	var wg sync.WaitGroup

	errs := make(chan error, requests)
	for index := range requests {
		wg.Add(1)

		go func() {
			defer wg.Done()

			field := "alpha"
			if index%2 == 1 {
				field = "beta"
			}

			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/%s", server.URL, field), nil)
			if err != nil {
				errs <- err

				return
			}

			req = RequestWithRedaction(req, RedactionConfig{
				Response: RedactionRules{JSONFields: []string{field}},
			})

			resp, err := client.Do(req)
			if err != nil {
				errs <- err

				return
			}

			_, readErr := io.Copy(io.Discard, resp.Body)

			closeErr := resp.Body.Close()
			if readErr != nil {
				errs <- readErr
			} else if closeErr != nil {
				errs <- closeErr
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	entries := record.Entries()
	if len(entries) != requests {
		t.Fatalf("entries = %d, want %d", len(entries), requests)
	}

	for _, entry := range entries {
		text := entry.Response.Content.Text
		if strings.HasSuffix(entry.Request.URL, "/alpha") {
			if strings.Contains(text, "alpha-value") || !strings.Contains(text, "beta-value") {
				t.Fatalf("alpha-scoped response = %q", text)
			}
		} else if strings.Contains(text, "beta-value") || !strings.Contains(text, "alpha-value") {
			t.Fatalf("beta-scoped response = %q", text)
		}
	}
}

func TestRequestScopedBodyRedactorOverridesGlobalRegistration(t *testing.T) {
	global := &markerBodyRedactor{marker: "global"}
	scoped := &markerBodyRedactor{marker: "scoped"}
	base := newRedactor(&Config{Redaction: RedactionConfig{Common: RedactionRules{
		BodyRedactors: map[string]BodyRedactor{"text/csv": global},
	}}})
	effective := base.withRules(RedactionRules{
		BodyRedactors: map[string]BodyRedactor{"TEXT/CSV; charset=utf-8": scoped},
	})

	selected, kind := selectBodyRedactor("text/csv", effective)
	if selected != scoped || kind != "custom" {
		t.Fatalf("selected = %T %p, kind = %q", selected, selected, kind)
	}
}

func TestBodyCapturePolicyOverridesRequestScopedBodyRedactor(t *testing.T) {
	scoped := &markerBodyRedactor{marker: "scoped"}
	policy := &markerBodyRedactor{marker: "policy"}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = io.WriteString(w, "secret")
	}))
	defer server.Close()

	record := NewMemoryRecorder()
	client := &http.Client{Transport: NewTransport(http.DefaultTransport, record, configWith(withCaptureResponseBody(true),
		withEmbedBodies(true),
		withBodyCapturePolicy(BodyCapturePolicy(func(_ context.Context, meta BodyCaptureMeta, decision BodyCaptureDecision) (BodyCaptureDecision, error) {
			if meta.Direction == ResponseBody {
				decision.RedactorOverride = policy
			}

			return decision, nil
		}))),
	)}

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	req = RequestWithRedaction(req, RedactionConfig{
		Response: RedactionRules{BodyRedactors: map[string]BodyRedactor{"text/csv": scoped}},
	})

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	entry := singleEntry(t, record)
	if got := entry.Response.Content.Text; got != "policy" {
		t.Fatalf("response content = %q, want policy override", got)
	}

	if scoped.opens.Load() != 0 || policy.opens.Load() != 1 {
		t.Fatalf("redactor opens: scoped=%d policy=%d", scoped.opens.Load(), policy.opens.Load())
	}
}

func assertAbsent(t *testing.T, text string, forbidden ...string) {
	t.Helper()

	for _, value := range forbidden {
		if strings.Contains(text, value) {
			t.Fatalf("%q leaked in %q", value, text)
		}
	}
}

func assertPairsAbsent(t *testing.T, pairs []NameValuePair, forbidden ...string) {
	t.Helper()

	var joined bytes.Buffer
	for _, pair := range pairs {
		joined.WriteString(pair.Value)
		joined.WriteByte('\n')
	}

	assertAbsent(t, joined.String(), forbidden...)
}

func assertCookiesAbsent(t *testing.T, cookies []Cookie, forbidden ...string) {
	t.Helper()

	var joined bytes.Buffer
	for _, cookie := range cookies {
		joined.WriteString(cookie.Value)
		joined.WriteByte('\n')
	}

	assertAbsent(t, joined.String(), forbidden...)
}
