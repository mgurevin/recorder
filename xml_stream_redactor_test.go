package recorder

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func streamXML(t *testing.T, input string, chunk int) (string, error) {
	t.Helper()
	var out bytes.Buffer
	r := newXMLStreamRedactor(&out, lowerSet([]string{"password", "secret"}))
	for pos := 0; pos < len(input); {
		end := min(len(input), pos+chunk)
		if _, err := r.Write([]byte(input[pos:end])); err != nil {
			return out.String(), err
		}
		pos = end
	}
	return out.String(), r.Close()
}

func TestXMLStreamRedactorChunkBoundaries(t *testing.T) {
	in := `<?xml version="1.0"?><r xmlns:x="urn:x"><x:Password kind="text"><inner>deep</inner><![CDATA[raw&data]]></x:Password><!-- keep --><safe><![CDATA[ok]]></safe><Secret/></r>`
	want := `<?xml version="1.0"?><r xmlns:x="urn:x"><x:Password kind="text">[REDACTED]</x:Password><!-- keep --><safe><![CDATA[ok]]></safe><Secret/></r>`
	for chunk := 1; chunk <= 31; chunk++ {
		got, err := streamXML(t, in, chunk)
		if err != nil || got != want {
			t.Fatalf("chunk %d: got %q, %v; want %q", chunk, got, err, want)
		}
	}
}

func TestXMLStreamRedactorLargeSecretUsesBoundedState(t *testing.T) {
	in := `<r><password>` + strings.Repeat("s", 8<<20) + `</password><keep>yes</keep></r>`
	got, err := streamXML(t, in, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got != `<r><password>[REDACTED]</password><keep>yes</keep></r>` {
		t.Fatalf("unexpected output: %.200q", got)
	}
}

func TestXMLStreamRedactorIncompleteMatchedElementFailsClosed(t *testing.T) {
	got, err := streamXML(t, `<r><password>secret`, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "secret") || got != `<r><password>[REDACTED]` {
		t.Fatalf("incomplete element leaked: %q", got)
	}
}

func TestXMLStreamRedactorFailsClosedOnMarkupLimit(t *testing.T) {
	var out bytes.Buffer
	r := newXMLStreamRedactor(&out, lowerSet([]string{"password"}))
	input := `<password ` + strings.Repeat("a", maxXMLMarkupBytes+1)
	_, err := r.Write([]byte(input))
	if !errors.Is(err, errRedactionLimit) {
		t.Fatalf("error = %v, want redaction limit", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversized markup was emitted: %d bytes", out.Len())
	}
}

func TestXMLStreamRedactorFailsClosedOnSuppressedDepth(t *testing.T) {
	var out bytes.Buffer
	r := newXMLStreamRedactor(&out, lowerSet([]string{"password"}))
	input := `<password>` + strings.Repeat("<x>", maxXMLDepth+1) + `secret`
	_, err := r.Write([]byte(input))
	if !errors.Is(err, errRedactionLimit) {
		t.Fatalf("error = %v, want redaction limit", err)
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("limit failure leaked suppressed value: %q", out.String())
	}
}
