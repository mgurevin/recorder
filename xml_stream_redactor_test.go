package recorder

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
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

func TestXMLStreamRedactorMismatchedEndTagDoesNotEndSuppression(t *testing.T) {
	in := `<r><password>first</wrong>ret-tail</password><keep>yes</keep></r>`
	want := `<r><password>[REDACTED]</password><keep>yes</keep></r>`

	for chunk := 1; chunk <= 31; chunk++ {
		got, err := streamXML(t, in, chunk)
		if err != nil || got != want {
			t.Fatalf("chunk %d: got %q, %v; want %q", chunk, got, err, want)
		}
	}
}

func TestXMLStreamRedactorUnclosedNestedTagFailsClosed(t *testing.T) {
	got, err := streamXML(t, `<r><password><nested>secret</password>tail</r>`, 3)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got, "secret") || strings.Contains(got, "tail") {
		t.Fatalf("mismatched nested markup leaked suppressed bytes: %q", got)
	}
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

func TestXMLStreamRedactorUnicodeNameMatching(t *testing.T) {
	tests := []struct {
		name    string
		element string
		input   string
		want    string
	}{
		{
			name:    "latin diaeresis",
			element: "pässword",
			input:   `<r><PÄSSWORD>secret</PÄSSWORD><keep>yes</keep></r>`,
			want:    `<r><PÄSSWORD>[REDACTED]</PÄSSWORD><keep>yes</keep></r>`,
		},
		{
			name:    "Turkish dotted capital I",
			element: "şifre",
			input:   `<r><ŞİFRE>gizli123</ŞİFRE><kalan>evet</kalan></r>`,
			want:    `<r><ŞİFRE>[REDACTED]</ŞİFRE><kalan>evet</kalan></r>`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for chunk := 1; chunk <= len(tc.input); chunk++ {
				var out bytes.Buffer

				r := newXMLStreamRedactor(&out, lowerSet([]string{tc.element}))
				for start := 0; start < len(tc.input); start += chunk {
					end := min(start+chunk, len(tc.input))
					if _, err := r.Write([]byte(tc.input[start:end])); err != nil {
						t.Fatalf("chunk %d: write: %v", chunk, err)
					}
				}

				if err := r.Close(); err != nil {
					t.Fatalf("chunk %d: close: %v", chunk, err)
				}

				if got := out.String(); got != tc.want {
					t.Fatalf("chunk %d: got %q, want %q", chunk, got, tc.want)
				}
			}
		})
	}
}

func TestXMLStreamRedactorAllocationBudget(t *testing.T) {
	payload := []byte(`<root>` + strings.Repeat(`<item><id>1</id><password>secret</password></item>`, 256) + `</root>`)
	elements := lowerSet([]string{"password"})

	allocations := testing.AllocsPerRun(100, func() {
		r := newXMLStreamRedactor(io.Discard, elements)
		if _, err := r.Write(payload); err != nil {
			panic(err)
		}

		if err := r.Close(); err != nil {
			panic(err)
		}
	})

	if allocations > 32 {
		t.Fatalf("allocations = %.0f, want <= 32", allocations)
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

func FuzzXMLStreamRedactor(f *testing.F) {
	f.Add([]byte("value"), uint8(1), false)
	f.Add([]byte{0, 1, 2, 255}, uint8(11), true)
	f.Fuzz(func(t *testing.T, payload []byte, chunkByte uint8, mismatched bool) {
		if len(payload) > 4096 {
			t.Skip()
		}

		secret := "stream-secret-" + hex.EncodeToString(payload)

		inner := secret
		if mismatched {
			inner = `<nested>` + secret + `</wrong>tail</nested>`
		}

		in := `<r><password>` + inner + `</password><keep>yes</keep></r>`
		chunk := int(chunkByte%64) + 1

		got, err := streamXML(t, in, chunk)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(got, secret) || (mismatched && strings.Contains(got, "tail")) {
			t.Fatalf("stream leaked matched subtree: %q", got)
		}
	})
}
