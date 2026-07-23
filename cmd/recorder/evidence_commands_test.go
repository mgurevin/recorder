package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
	"github.com/mgurevin/recorder/hartest"
)

func TestVerifyExternalAssetsAndChecksums(t *testing.T) {
	document := decodeTestHAR(t)
	reference := fileBodyReferencePrefix + "0123456789abcdef0123456789abcdef"
	orphan := fileBodyReferencePrefix + "fedcba9876543210fedcba9876543210"
	body := []byte("{}")
	sum := sha256.Sum256(body)

	document.Log.Entries[0].Recorder.ResponseBody.Store = reference
	document.Log.Entries[0].Recorder.ResponseBody.HashAlgorithm = "sha256"
	document.Log.Entries[0].Recorder.ResponseBody.Hash = hex.EncodeToString(sum[:])
	document.Log.Entries[0].Response.Content.Text = ""

	root := t.TempDir()

	assets := filepath.Join(root, "assets")
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}

	writeAsset(t, root, reference, body)
	writeAsset(t, root, orphan, []byte("unreferenced"))

	path := writeDocument(t, document)

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"verify", "--json", "--body-store", root, path},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	var report verificationReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if report.ChecksumsVerified != 1 || report.Orphans != 1 || report.Modified != 0 {
		t.Fatalf("report = %#v", report)
	}

	writeAsset(t, root, reference, []byte("[]"))
	output.Reset()

	err := run(
		context.Background(),
		[]string{"verify", "--json", "--body-store", root, path},
		&output,
		io.Discard,
	)
	if !errors.Is(err, errVerificationFailed) {
		t.Fatalf("verify modified error = %v", err)
	}

	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if report.Modified != 1 {
		t.Fatalf("modified report = %#v", report)
	}

	if len(report.Issues) == 0 || report.Issues[0].Entry == nil || *report.Issues[0].Entry != 0 {
		t.Fatalf("first entry index missing from report = %#v", report)
	}

	if strings.Contains(output.String(), root) {
		t.Fatalf("report exposes body-store path = %s", output.String())
	}

	if err := os.Remove(assetPath(t, root, reference)); err != nil {
		t.Fatal(err)
	}

	output.Reset()

	err = run(
		context.Background(),
		[]string{"verify", "--json", "--body-store", root, path},
		&output,
		io.Discard,
	)
	if !errors.Is(err, errVerificationFailed) {
		t.Fatalf("verify missing error = %v", err)
	}

	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if report.Missing != 1 {
		t.Fatalf("missing report = %#v", report)
	}
}

func TestVerifyRequiresStoreAndSkipsTransformedHashes(t *testing.T) {
	document := decodeTestHAR(t)
	reference := fileBodyReferencePrefix + "0123456789abcdef0123456789abcdef"
	document.Log.Entries[0].Recorder.ResponseBody.Store = reference

	path := writeDocument(t, document)
	if err := run(
		context.Background(),
		[]string{"verify", path},
		io.Discard,
		io.Discard,
	); !errors.Is(err, errUsage) {
		t.Fatalf("missing store error = %v", err)
	}

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}

	writeAsset(t, root, reference, []byte("[REDACTED]"))

	document.Log.Entries[0].Recorder.ResponseBody.CapturedBytes = int64(len("[REDACTED]"))
	document.Log.Entries[0].Recorder.ResponseBody.TotalBytes = int64(len("[REDACTED]"))
	document.Log.Entries[0].Recorder.ResponseBody.HashAlgorithm = "sha256"
	document.Log.Entries[0].Recorder.ResponseBody.Hash = strings.Repeat("0", 64)
	replacements := int64(1)
	document.Log.Entries[0].Recorder.Redaction = &recorder.RedactionInfo{
		Response: &recorder.RedactionScopeInfo{
			Body: &recorder.BodyRedactionInfo{
				Kind:         "json",
				Outcome:      recorder.BodyRedactionRedacted,
				Replacements: &replacements,
			},
		},
	}

	path = writeDocument(t, document)

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"verify", "--json", "--body-store", root, path},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	var report verificationReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if report.ChecksumsSkipped != 1 || report.Modified != 0 {
		t.Fatalf("redacted report = %#v", report)
	}
}

func TestFixtureStreamsFilteredOutputAndRefusesOverwrite(t *testing.T) {
	input := writeCapture(t, "capture.har", testHAR(t))
	output := filepath.Join(t.TempDir(), "server-errors.ndjson")

	if err := run(
		context.Background(),
		[]string{
			"fixture",
			"--method", "POST",
			"--host", "api.example.com",
			"--status-min", "200",
			"--status-max", "299",
			"--output", output,
			input,
		},
		io.Discard,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := hario.ReadNDJSON(file, hario.DefaultReadConfig())
	_ = file.Close()

	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Response.Status != http.StatusCreated {
		t.Fatalf("fixture entries = %#v", entries)
	}

	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}

	if err := run(
		context.Background(),
		[]string{"fixture", "--output", output, input},
		io.Discard,
		io.Discard,
	); err == nil {
		t.Fatal("fixture overwrote an existing output")
	}

	after, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(before, after) {
		t.Fatal("existing fixture changed")
	}

	harOutput := filepath.Join(t.TempDir(), "fixture.har")
	if err := run(
		context.Background(),
		[]string{"fixture", "--output", harOutput, input},
		io.Discard,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	harFile, err := os.Open(harOutput)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = harFile.Close()
	}()

	if _, err := hario.ReadHAR(harFile, hario.DefaultReadConfig()); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureDoesNotPublishInvalidInput(t *testing.T) {
	input := writeCapture(t, "broken.har", []byte(`{"log":{"version":"1.2","creator":{"name":"x"},"entries":[`))
	output := filepath.Join(t.TempDir(), "fixture.ndjson")

	if err := run(
		context.Background(),
		[]string{"fixture", "--output", output, input},
		io.Discard,
		io.Discard,
	); err == nil {
		t.Fatal("fixture accepted invalid input")
	}

	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output stat error = %v", err)
	}
}

func TestFixtureWritesValidatedStdout(t *testing.T) {
	input := writeCapture(t, "capture.har", testHAR(t))

	var output bytes.Buffer

	if err := run(
		context.Background(),
		[]string{"fixture", "--status-min", "500", input},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if output.Len() != 0 {
		t.Fatalf("empty selection output = %q", output.String())
	}

	if _, err := resolveOutputFormat("fixture.unknown", formatAuto); !errors.Is(err, errUsage) {
		t.Fatalf("unknown output format error = %v", err)
	}

	if _, err := resolveOutputFormat("-", "yaml"); !errors.Is(err, errUsage) {
		t.Fatalf("invalid output format error = %v", err)
	}
}

func TestEntryFilterCoversEverySelector(t *testing.T) {
	entry := decodeTestHAR(t).Log.Entries[0]

	for _, filter := range []entryFilter{
		{method: "GET"},
		{host: "other.example.com"},
		{statusMin: 300},
		{statusMax: 199},
	} {
		if filter.match(entry) {
			t.Fatalf("filter %#v matched", filter)
		}
	}

	if !(entryFilter{
		method:    "POST",
		host:      "api.example.com",
		statusMin: 200,
		statusMax: 299,
	}).match(entry) {
		t.Fatal("complete filter did not match")
	}

	if (entryFilter{}).match(nil) {
		t.Fatal("nil entry matched")
	}
}

func TestFixtureHandlerMapsLoopbackRequestToRecordedOrigin(t *testing.T) {
	document := decodeTestHAR(t)
	entry := document.Log.Entries[0]
	entry.Request.Method = http.MethodGet
	entry.Request.URL = "https://api.example.com/orders"
	entry.Request.BodySize = 0
	entry.Request.PostData = nil

	fixture, err := hartest.NewTransport([]*recorder.Entry{entry}, hartest.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	origin, err := fixtureOrigin([]*recorder.Entry{entry}, "")
	if err != nil {
		t.Fatal(err)
	}

	var diagnostics bytes.Buffer

	handler := fixtureHandler(fixture, origin, &diagnostics)

	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/orders", nil)
	if err != nil {
		t.Fatal(err)
	}

	response := newResponseRecorder()
	handler.ServeHTTP(response, request)

	if response.status != http.StatusCreated || response.body.String() != "{}" {
		t.Fatalf("fixture response = %d %q; diagnostics=%q", response.status, response.body.String(), diagnostics.String())
	}

	if err := fixture.Verify(); err != nil {
		t.Fatal(err)
	}

	unmatched := newResponseRecorder()
	handler.ServeHTTP(unmatched, request)

	if unmatched.status != http.StatusBadGateway || !strings.Contains(unmatched.body.String(), "did not match") {
		t.Fatalf("unmatched response = %d %q", unmatched.status, unmatched.body.String())
	}
}

func TestFixtureOriginAndListenValidation(t *testing.T) {
	document := decodeTestHAR(t)
	document.Log.Entries[1].Request.URL = "https://other.example.com/orders"

	if _, err := fixtureOrigin(document.Log.Entries, ""); !errors.Is(err, errUsage) {
		t.Fatalf("multiple origin error = %v", err)
	}

	if _, err := fixtureOrigin(document.Log.Entries, "https://api.example.com"); err != nil {
		t.Fatal(err)
	}

	if _, err := fixtureOrigin(document.Log.Entries, "file:///tmp"); !errors.Is(err, errUsage) {
		t.Fatalf("invalid origin error = %v", err)
	}

	if _, err := listenLoopback("0.0.0.0:8080"); !errors.Is(err, errUsage) {
		t.Fatalf("non-loopback error = %v", err)
	}
}

func TestInspectAndServeFixtureStopWithContext(t *testing.T) {
	path := writeCapture(t, "capture.har", testHAR(t))

	inspectContext, cancelInspect := context.WithCancel(context.Background())
	cancelInspect()

	var inspectOutput bytes.Buffer
	if err := run(
		inspectContext,
		[]string{
			"inspect",
			"--no-open",
			"--inspector-url", "http://localhost:5173/",
			path,
		},
		&inspectOutput,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(inspectOutput.String(), "http%3A%2F%2F127.0.0.1") {
		t.Fatalf("inspect output = %q", inspectOutput.String())
	}

	serveContext, cancelServe := context.WithCancel(context.Background())
	cancelServe()

	var serveOutput bytes.Buffer
	if err := run(
		serveContext,
		[]string{
			"serve-fixture",
			"--listen", "127.0.0.1:0",
			"--allow-unused",
			path,
		},
		&serveOutput,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(serveOutput.String(), "mapped to https://api.example.com") {
		t.Fatalf("serve output = %q", serveOutput.String())
	}
}

func TestVerificationHashAndEmbeddedBodyVariants(t *testing.T) {
	for _, test := range []struct {
		algorithm string
		length    int
	}{
		{algorithm: "md5", length: 32},
		{algorithm: "sha1", length: 40},
		{algorithm: "sha256", length: 64},
	} {
		value, err := digest(test.algorithm, []byte("evidence"))
		if err != nil {
			t.Fatal(err)
		}

		if len(value) != test.length {
			t.Fatalf("%s digest length = %d", test.algorithm, len(value))
		}
	}

	if _, err := digest("unknown", nil); err == nil {
		t.Fatal("unknown digest succeeded")
	}

	document := decodeTestHAR(t)

	entry := document.Log.Entries[0]
	if data, ok := embeddedBody(entry, "response"); !ok || string(data) != "{}" {
		t.Fatalf("response embedded body = %q, %v", data, ok)
	}

	entry.Request.PostData = &recorder.PostData{Text: "cGF5bG9hZA=="}

	entry.Recorder.RequestBodyEncoding = "base64"
	if data, ok := embeddedBody(entry, "request"); !ok || string(data) != "payload" {
		t.Fatalf("request embedded body = %q, %v", data, ok)
	}

	var output bytes.Buffer

	report := &verificationReport{
		Format:            formatHAR,
		Entries:           1,
		ChecksumsVerified: 2,
		Issues: []verificationIssue{
			{Entry: intPointer(1), Direction: "response", Kind: "orphan", Reference: "filebody:v1:x"},
		},
	}
	if err := writeVerificationReport(&output, report); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), "Checksums: 2 verified") {
		t.Fatalf("verification output = %q", output.String())
	}

	sum := sha256.Sum256([]byte("{}"))
	entry.Recorder.ResponseBody.HashAlgorithm = "sha256"
	entry.Recorder.ResponseBody.Hash = hex.EncodeToString(sum[:])
	entry.Recorder.RequestBody = nil
	path := writeDocument(t, document)

	output.Reset()

	if err := run(
		context.Background(),
		[]string{"verify", path},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), "Checksums: 1 verified") {
		t.Fatalf("embedded verification output = %q", output.String())
	}

	entry.Request.PostData.Text = "plain"

	entry.Recorder.RequestBodyEncoding = ""
	if data, ok := embeddedBody(entry, "request"); !ok || string(data) != "plain" {
		t.Fatalf("plain request body = %q, %v", data, ok)
	}

	entry.Request.PostData = nil
	if _, ok := embeddedBody(entry, "request"); ok {
		t.Fatal("missing request body was available")
	}

	entry.Response.Content.Encoding = "base64"

	entry.Response.Content.Text = "e30="
	if data, ok := embeddedBody(entry, "response"); !ok || string(data) != "{}" {
		t.Fatalf("base64 response body = %q, %v", data, ok)
	}

	entry.Response.Content.Text = "!"
	if _, ok := embeddedBody(entry, "response"); ok {
		t.Fatal("invalid base64 response body was available")
	}

	for _, reference := range []string{
		"memory:v1:value",
		fileBodyReferencePrefix + "short",
		fileBodyReferencePrefix + "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	} {
		if _, err := parseFileBodyReference(reference); err == nil {
			t.Fatalf("parseFileBodyReference(%q) succeeded", reference)
		}
	}
}

func TestSummaryBodyStatesAndUsageHelp(t *testing.T) {
	summary := newCaptureSummary(formatHAR)
	summary.addBody(&recorder.BodyInfo{
		Present:       true,
		Complete:      false,
		ClosedEarly:   true,
		Truncated:     true,
		CapturedBytes: 3,
	})

	if summary.CapturedBytes != 3 ||
		summary.IncompleteBodies != 1 ||
		summary.ClosedEarlyBodies != 1 ||
		summary.TruncatedBodies != 1 {
		t.Fatalf("body summary = %#v", summary)
	}

	summary.addBody(nil)

	if err := usageError(flag.ErrHelp); err != nil {
		t.Fatalf("help error = %v", err)
	}

	if err := usageError(errors.New("broken flag")); !errors.Is(err, errUsage) {
		t.Fatalf("usage error = %v", err)
	}
}

func TestFixtureHeaderCopyAndDiskBodyOpener(t *testing.T) {
	source := http.Header{
		"Content-Type":      {"application/json"},
		"Connection":        {"close"},
		"Transfer-Encoding": {"chunked"},
	}
	destination := make(http.Header)
	copyFixtureHeaders(destination, source)

	if destination.Get("Content-Type") != "application/json" {
		t.Fatalf("destination headers = %#v", destination)
	}

	if destination.Get("Connection") != "" || destination.Get("Transfer-Encoding") != "" {
		t.Fatalf("hop-by-hop headers copied: %#v", destination)
	}

	if !isHopByHopHeader("Upgrade") || isHopByHopHeader("X-Test") {
		t.Fatal("hop-by-hop classification is incorrect")
	}

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}

	reference := fileBodyReferencePrefix + "0123456789abcdef0123456789abcdef"
	writeAsset(t, root, reference, []byte("asset"))

	reader, err := (diskBodyOpener{root: root}).Open(reference)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	if string(data) != "asset" {
		t.Fatalf("asset = %q", data)
	}
}

func TestServeFixtureRejectsUnusedFixture(t *testing.T) {
	path := writeCapture(t, "capture.har", testHAR(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := run(
		ctx,
		[]string{"serve-fixture", "--listen", "127.0.0.1:0", path},
		io.Discard,
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "not consumed") {
		t.Fatalf("unused fixture error = %v", err)
	}
}

func TestServeFixtureAndInspectRejectUnsafeOptions(t *testing.T) {
	path := writeCapture(t, "capture.har", testHAR(t))

	for _, args := range [][]string{
		{"serve-fixture", "--listen", "0.0.0.0:8080", path},
		{"serve-fixture", "--origin", "file:///tmp", path},
		{"inspect", "--inspector-url", "http://example.com", path},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("run(%q) error = %v", args, err)
		}
	}
}

func decodeTestHAR(t *testing.T) *recorder.HAR {
	t.Helper()

	var document recorder.HAR
	if err := json.Unmarshal(testHAR(t), &document); err != nil {
		t.Fatal(err)
	}

	return &document
}

func writeDocument(t *testing.T, document *recorder.HAR) string {
	t.Helper()

	var output bytes.Buffer
	if err := document.Write(&output); err != nil {
		t.Fatal(err)
	}

	return writeCapture(t, "capture.har", output.Bytes())
}

func writeAsset(t *testing.T, root, reference string, data []byte) {
	t.Helper()

	if err := os.WriteFile(assetPath(t, root, reference), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assetPath(t *testing.T, root, reference string) string {
	t.Helper()

	id, err := parseFileBodyReference(reference)
	if err != nil {
		t.Fatal(err)
	}

	return filepath.Join(root, "assets", id+".body")
}
