package recorder

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func streamForm(t *testing.T, input string, chunk int) (string, error) {
	t.Helper()

	var out bytes.Buffer

	r := newFormStreamRedactor(&out, lowerSet([]string{"token", "api_key"}))

	for pos := 0; pos < len(input); {
		end := min(len(input), pos+chunk)
		if _, err := r.Write([]byte(input[pos:end])); err != nil {
			return out.String(), err
		}

		pos = end
	}

	err := r.Close()

	return out.String(), err
}

func TestFormStreamRedactorChunkBoundaries(t *testing.T) {
	in := `keep=a+b&token=s%20x&T%4fKEN=two%26x&flag&&empty=&api%5Fkey=last%3Dvalue`
	want := `keep=a+b&token=%5BREDACTED%5D&T%4fKEN=%5BREDACTED%5D&flag&&empty=&api%5Fkey=%5BREDACTED%5D`

	for chunk := 1; chunk <= 31; chunk++ {
		got, err := streamForm(t, in, chunk)
		if err != nil || got != want {
			t.Fatalf("chunk %d: got %q, %v; want %q", chunk, got, err, want)
		}
	}
}

func TestFormStreamRedactorPreservesUnmatchedBytes(t *testing.T) {
	for _, in := range []string{
		``, `&&`, `flag`, `a=1;b=2`, `bad%=value`, `a=%ZZ+still-raw`,
		`encoded%26key=value%26with%3Ddelimiters&empty=`,
	} {
		got, err := streamForm(t, in, 1)
		if err != nil || got != in {
			t.Fatalf("input %q became %q, %v", in, got, err)
		}
	}
}

func TestFormStreamRedactorLargeSecretUsesBoundedState(t *testing.T) {
	secret := strings.Repeat("s", 8<<20)

	got, err := streamForm(t, `token=`+secret+`&keep=yes`, 13)
	if err != nil {
		t.Fatal(err)
	}

	if got != `token=%5BREDACTED%5D&keep=yes` {
		t.Fatalf("unexpected output: %.200q", got)
	}
}

func TestFormStreamRedactorFailsClosedOnKeyLimit(t *testing.T) {
	var out bytes.Buffer

	r := newFormStreamRedactor(&out, lowerSet([]string{"token"}))

	_, err := r.Write([]byte(strings.Repeat("a", maxFormKeyBytes+1) + `=secret`))
	if !errors.Is(err, errRedactionLimit) {
		t.Fatalf("error = %v, want redaction limit", err)
	}

	if out.Len() != 0 {
		t.Fatalf("oversized undecided key was emitted: %d bytes", out.Len())
	}
}

func TestFormMIMEUsesQueryRulesOnly(t *testing.T) {
	var out bytes.Buffer

	red := newRedactor(&Options{Redaction: RedactionConfig{Common: RedactionRules{JSONFields: []string{"password"}}}})
	if r := newBodyStreamRedactor(&out, "application/x-www-form-urlencoded", red); r != nil {
		t.Fatal("form MIME incorrectly selected JSON/XML sniffer")
	}
}

func FuzzFormStreamRedactor(f *testing.F) {
	f.Add([]byte("value"), uint8(1))
	f.Add([]byte{0, 1, 2, 255}, uint8(17))
	f.Fuzz(func(t *testing.T, payload []byte, chunkByte uint8) {
		if len(payload) > 4096 {
			t.Skip()
		}

		secret := "form-secret-" + hex.EncodeToString(payload)
		in := `keep=a+b&token=` + secret + `&TOKEN=` + secret + `&tail=%26safe`
		chunk := int(chunkByte%64) + 1

		got, err := streamForm(t, in, chunk)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(got, secret) {
			t.Fatalf("stream leaked matched value: %q", got)
		}

		if strings.Count(got, formRedactedValue) != 2 {
			t.Fatalf("both values were not redacted: %q", got)
		}

		if !strings.Contains(got, `keep=a+b`) || !strings.Contains(got, `tail=%26safe`) {
			t.Fatalf("unmatched bytes changed: %q", got)
		}
	})
}
