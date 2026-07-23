// Command csv-redactor contains a copy-oriented example of a streaming
// recorder.BodyRedactor for CSV request and response bodies. It is deliberately
// a main package so applications cannot depend on it as a library.
package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/mgurevin/recorder"
)

// Redactor replaces values in columns selected by their header names.
// A Redactor is safe for concurrent use; every call to Redact creates an
// independent parser and writer.
type Redactor struct {
	Columns []string
	Comma   rune
}

func main() {}

// Redact implements recorder.BodyRedactor. Recorder supplies protector, so
// this redactor never handles keys or constructs protected token formats.
func (r Redactor) Redact(dst io.Writer, _ string, protector recorder.BodyValueProtector) (io.WriteCloser, error) {
	if protector == nil {
		return nil, errors.New("csv redactor: nil value protector")
	}

	return r.open(dst, protector)
}

func (r Redactor) open(dst io.Writer, protector recorder.BodyValueProtector) (io.WriteCloser, error) {
	if dst == nil {
		return nil, errors.New("csv redactor: nil destination")
	}

	columns := make(map[string]struct{}, len(r.Columns))
	for _, column := range r.Columns {
		name := strings.ToLower(strings.TrimSpace(column))
		if name == "" {
			return nil, errors.New("csv redactor: column names must not be empty")
		}

		columns[name] = struct{}{}
	}

	if len(columns) == 0 {
		return nil, errors.New("csv redactor: at least one column is required")
	}

	comma := r.Comma
	if comma == 0 {
		comma = ','
	}

	pr, pw := io.Pipe()
	w := &writer{pw: pw, done: make(chan result, 1)}

	go w.process(pr, dst, columns, protector, comma)

	return w, nil
}

type result struct {
	err error
}

type writer struct {
	pw        *io.PipeWriter
	done      chan result
	closeOnce sync.Once
	closeErr  error
}

func (w *writer) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

func (w *writer) Close() error {
	w.closeOnce.Do(func() {
		closeErr := w.pw.Close()
		processed := <-w.done

		if processed.err != nil {
			w.closeErr = processed.err
		} else {
			w.closeErr = closeErr
		}
	})

	return w.closeErr
}

func (w *writer) process(pr *io.PipeReader, dst io.Writer, columns map[string]struct{}, protector recorder.BodyValueProtector, comma rune) {
	var processed result

	defer func() {
		_ = pr.CloseWithError(processed.err)
		w.done <- processed
	}()

	reader := csv.NewReader(pr)
	reader.Comma = comma
	reader.FieldsPerRecord = 0

	output := csv.NewWriter(dst)
	output.Comma = comma

	header, err := reader.Read()
	if err != nil {
		processed.err = fmt.Errorf("csv redactor: read header: %w", err)

		return
	}

	selected := make([]int, 0, len(columns))
	found := make(map[string]struct{}, len(columns))

	for index, field := range header {
		name := strings.ToLower(strings.TrimSpace(field))
		if _, ok := columns[name]; ok {
			selected = append(selected, index)
			found[name] = struct{}{}
		}
	}

	if len(found) != len(columns) {
		processed.err = errors.New("csv redactor: one or more configured columns are missing from the header")

		return
	}

	if err := output.Write(header); err != nil {
		processed.err = fmt.Errorf("csv redactor: write header: %w", err)

		return
	}

	for {
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}

		if readErr != nil {
			processed.err = fmt.Errorf("csv redactor: read record: %w", readErr)

			return
		}

		for _, index := range selected {
			if index >= len(record) {
				processed.err = errors.New("csv redactor: record has fewer fields than its header")

				return
			}

			value := protector.NewValue()
			if _, err := io.WriteString(value, record[index]); err != nil {
				processed.err = fmt.Errorf("csv redactor: buffer protected value: %w", err)

				return
			}

			var protected strings.Builder
			if err := value.FinishTo(&protected); err != nil {
				processed.err = fmt.Errorf("csv redactor: finish protected value: %w", err)

				return
			}

			record[index] = protected.String()
		}

		if err := output.Write(record); err != nil {
			processed.err = fmt.Errorf("csv redactor: write record: %w", err)

			return
		}
	}

	output.Flush()

	if err := output.Error(); err != nil {
		processed.err = fmt.Errorf("csv redactor: flush output: %w", err)
	}
}
