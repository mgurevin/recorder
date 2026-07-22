package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/mgurevin/recorder"
)

func TestRedactorStreamsSelectedColumns(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	redactor := Redactor{Columns: []string{"email", "card_number"}}

	w, err := redactor.Redact(&output, "text/csv; charset=utf-8", fixedProtector("[REDACTED]"))
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	input := "id,email,card_number,note\r\n1,alice@example.com,4111111111111111,\"hello, world\"\r\n"
	for _, chunk := range []string{input[:7], input[7:31], input[31:]} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const want = "id,email,card_number,note\n1,[REDACTED],[REDACTED],\"hello, world\"\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestRedactorUsesLibraryProtector(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	w, err := (Redactor{Columns: []string{"secret"}}).Redact(&output, "text/csv", prefixProtector("protected:"))
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if _, err := io.WriteString(w, "id,secret\n1,a\n2,b\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got, want := output.String(), "id,secret\n1,protected:a\n2,protected:b\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

type prefixProtector string

func (p prefixProtector) NewValue() recorder.BodyValue {
	return &testValue{finish: func(value string) string { return string(p) + value }}
}

type fixedProtector string

func (p fixedProtector) NewValue() recorder.BodyValue {
	return &testValue{finish: func(string) string { return string(p) }}
}

type testValue struct {
	strings.Builder
	finish func(string) string
}

func (v *testValue) Finish() string { return v.finish(v.String()) }

func TestRedactorRejectsMissingColumnWithoutWritingRows(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	w, err := (Redactor{Columns: []string{"missing"}}).Redact(&output, "text/csv", prefixProtector(""))
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	_, writeErr := io.WriteString(w, "id,public\n1,value\n")

	closeErr := w.Close()
	if writeErr == nil && closeErr == nil {
		t.Fatal("missing-column input unexpectedly succeeded")
	}

	if strings.Contains(output.String(), "value") {
		t.Fatalf("unredacted row was written: %q", output.String())
	}
}

func TestRedactorRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		redactor  Redactor
		dst       io.Writer
		protector recorder.BodyValueProtector
	}{
		{name: "nil protector", redactor: Redactor{Columns: []string{"secret"}}, dst: io.Discard},
		{name: "nil destination", redactor: Redactor{Columns: []string{"secret"}}, protector: prefixProtector("")},
		{name: "no columns", redactor: Redactor{}, dst: io.Discard, protector: prefixProtector("")},
		{name: "empty column", redactor: Redactor{Columns: []string{" "}}, dst: io.Discard, protector: prefixProtector("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := tt.redactor.Redact(tt.dst, "text/csv", tt.protector); err == nil {
				t.Fatal("Redact unexpectedly succeeded")
			}
		})
	}
}

func TestRedactorRejectsMalformedCSV(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	w, err := (Redactor{Columns: []string{"secret"}}).Redact(&output, "text/csv", prefixProtector(""))
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if _, err := io.WriteString(w, "id,secret\n1,\"unterminated\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Close(); err == nil {
		t.Fatal("Close unexpectedly succeeded")
	}
}
