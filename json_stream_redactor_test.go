package recorder

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func streamJSON(t *testing.T, input string, chunk int) (string, error) {
	t.Helper()
	var out bytes.Buffer
	r := newJSONStreamRedactor(&out, lowerSet([]string{"password", "secret"}))
	for pos := 0; pos < len(input); {
		end := min(len(input), pos+chunk)
		if _, err := r.Write([]byte(input[pos:end])); err != nil {
			return out.String(), err
		}
		pos = end
	}
	return out.String(), r.Close()
}

func TestJSONStreamRedactorChunkBoundaries(t *testing.T) {
	in := " \n{\"keep\":1.2300e+04,\"pass\\u0077ord\" : {\"nested\":[1,{\"x\":\"}\\\"\"}]},\"items\":[{\"SECRET\":false},{\"ok\":3}]}\t"
	want := " \n{\"keep\":1.2300e+04,\"pass\\u0077ord\" : \"[REDACTED]\",\"items\":[{\"SECRET\":\"[REDACTED]\"},{\"ok\":3}]}\t"
	for chunk := 1; chunk <= 31; chunk++ {
		got, err := streamJSON(t, in, chunk)
		if err != nil || got != want {
			t.Fatalf("chunk %d: got %q, %v; want %q", chunk, got, err, want)
		}
	}
}

func TestJSONStreamRedactorLargeSecretUsesBoundedState(t *testing.T) {
	secret := strings.Repeat("s", 8<<20)
	in := `{"password":"` + secret + `","keep":true}`
	got, err := streamJSON(t, in, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"password":"[REDACTED]","keep":true}` {
		t.Fatalf("unexpected output: %.200q", got)
	}
}

func TestJSONStreamRedactorMalformedTailStaysRedacted(t *testing.T) {
	got, err := streamJSON(t, `{"password":"secret","later":`, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "secret") || !strings.Contains(got, redactedValue) {
		t.Fatalf("malformed stream leaked secret: %q", got)
	}
}

func TestJSONStreamRedactorFailsClosedOnKeyBufferLimit(t *testing.T) {
	var out bytes.Buffer
	r := newJSONStreamRedactor(&out, lowerSet([]string{"password"}))
	input := `{"` + strings.Repeat("a", maxJSONKeyBytes+1)
	_, err := r.Write([]byte(input))
	if !errors.Is(err, errRedactionLimit) {
		t.Fatalf("error = %v, want redaction limit", err)
	}
	if out.Len() > maxJSONKeyBytes+2 {
		t.Fatalf("output/state grew past key limit: %d", out.Len())
	}
}

func TestJSONStreamRedactorFailsClosedOnSuppressedDepth(t *testing.T) {
	var out bytes.Buffer
	r := newJSONStreamRedactor(&out, lowerSet([]string{"password"}))
	input := `{"password":` + strings.Repeat("[", maxJSONDepth+1) + `"secret"`
	_, err := r.Write([]byte(input))
	if !errors.Is(err, errRedactionLimit) {
		t.Fatalf("error = %v, want redaction limit", err)
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("limit failure leaked suppressed value: %q", out.String())
	}
}

func TestBodySnifferFailsClosedOnPrefixLimit(t *testing.T) {
	var out bytes.Buffer
	red := newRedactor(&Options{RedactJSONFields: []string{"password"}})
	r := newBodyStreamRedactor(&out, "text/plain", red)
	_, err := r.Write([]byte(strings.Repeat(" ", maxBodySniffBytes+1) + `{"password":"secret"}`))
	if !errors.Is(err, errRedactionLimit) {
		t.Fatalf("error = %v, want redaction limit", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversized undecided prefix was emitted: %d bytes", out.Len())
	}
}
