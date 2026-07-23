package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mgurevin/recorder"
)

type entryFilter struct {
	method    string
	host      string
	statusMin int
	statusMax int
}

func runFixture(args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("fixture", stderr)
	inputFormat := flags.String("format", formatAuto, "input format: auto, har, or ndjson")
	outputFormat := flags.String("to", formatAuto, "output format: auto, har, or ndjson")
	outputPath := flags.String("output", "-", `output path, or "-" for stdout`)
	method := flags.String("method", "", "select one HTTP method")
	host := flags.String("host", "", "select one request hostname")
	statusMin := flags.Int("status-min", 0, "select status codes at or above this value")
	statusMax := flags.Int("status-max", 0, "select status codes at or below this value")

	if err := flags.Parse(args); err != nil {
		return usageError(err)
	}

	inputPath, err := onePath(flags)
	if err != nil {
		return err
	}

	targetFormat, err := resolveOutputFormat(*outputPath, *outputFormat)
	if err != nil {
		return err
	}

	if *statusMin < 0 || *statusMax < 0 || (*statusMax > 0 && *statusMin > *statusMax) {
		return fmt.Errorf("%w: invalid status range", errUsage)
	}

	reader, closeReader, sourceFormat, err := openCapture(inputPath, *inputFormat)
	if err != nil {
		return err
	}
	defer closeReader()

	stream, err := newEntryStream(reader, sourceFormat)
	if err != nil {
		return fmt.Errorf("read %s: %w", inputPath, err)
	}

	output, err := newAtomicOutput(*outputPath, stdout)
	if err != nil {
		return err
	}
	defer output.abort()

	writer := newFixtureWriter(output.file, targetFormat)
	if err := writer.begin(); err != nil {
		return err
	}

	filter := entryFilter{
		method:    strings.ToUpper(strings.TrimSpace(*method)),
		host:      strings.ToLower(strings.TrimSpace(*host)),
		statusMin: *statusMin,
		statusMax: *statusMax,
	}

	for {
		entry, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return fmt.Errorf("read %s: %w", inputPath, nextErr)
		}

		if !filter.match(entry) {
			continue
		}

		if err := writer.write(entry); err != nil {
			return err
		}
	}

	if err := writer.end(); err != nil {
		return err
	}

	return output.commit()
}

func resolveOutputFormat(path, requested string) (string, error) {
	switch strings.ToLower(requested) {
	case formatHAR, formatNDJSON:
		return strings.ToLower(requested), nil

	case formatAuto:

	default:
		return "", fmt.Errorf("%w: output format must be auto, har, or ndjson", errUsage)
	}

	if path == "-" {
		return formatNDJSON, nil
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".har":
		return formatHAR, nil

	case ".ndjson", ".jsonl":
		return formatNDJSON, nil

	default:
		return "", fmt.Errorf("%w: cannot infer output format; use -to", errUsage)
	}
}

func (f entryFilter) match(entry *recorder.Entry) bool {
	if entry == nil || entry.Request == nil || entry.Response == nil {
		return false
	}

	if f.method != "" && !strings.EqualFold(entry.Request.Method, f.method) {
		return false
	}

	if f.host != "" {
		parsed, err := url.Parse(entry.Request.URL)
		if err != nil || !strings.EqualFold(parsed.Hostname(), f.host) {
			return false
		}
	}

	if f.statusMin > 0 && entry.Response.Status < f.statusMin {
		return false
	}

	if f.statusMax > 0 && entry.Response.Status > f.statusMax {
		return false
	}

	return true
}

type fixtureWriter struct {
	writer io.Writer
	format string
	count  int
}

func newFixtureWriter(writer io.Writer, format string) *fixtureWriter {
	return &fixtureWriter{writer: writer, format: format}
}

func (w *fixtureWriter) begin() error {
	if w.format != formatHAR {
		return nil
	}

	version := strings.TrimPrefix(commandVersion(), "recorder ")
	if version == "dev" {
		version = "devel"
	}

	prefix := `{"log":{"version":"1.2","creator":{"name":"github.com/mgurevin/recorder/cmd/recorder","version":` +
		strconv.Quote(version) + `},"entries":[`
	_, err := io.WriteString(w.writer, prefix)

	return err
}

func (w *fixtureWriter) write(entry *recorder.Entry) error {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode fixture entry: %w", err)
	}

	if w.format == formatHAR && w.count > 0 {
		if _, err := io.WriteString(w.writer, ","); err != nil {
			return err
		}
	}

	if _, err := w.writer.Write(encoded); err != nil {
		return err
	}

	if w.format == formatNDJSON {
		if _, err := io.WriteString(w.writer, "\n"); err != nil {
			return err
		}
	}

	w.count++

	return nil
}

func (w *fixtureWriter) end() error {
	if w.format != formatHAR {
		return nil
	}

	_, err := io.WriteString(w.writer, "]}}\n")

	return err
}

type atomicOutput struct {
	file      *os.File
	finalPath string
	stdout    io.Writer
	committed bool
	temporary string
}

func newAtomicOutput(path string, stdout io.Writer) (*atomicOutput, error) {
	directory := os.TempDir()
	mode := os.FileMode(0o600)

	if path != "-" {
		directory = filepath.Dir(path)
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("output %s already exists", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect output %s: %w", path, err)
		}
	}

	file, err := os.CreateTemp(directory, ".recorder-fixture-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary output: %w", err)
	}

	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())

		return nil, fmt.Errorf("set output permissions: %w", err)
	}

	return &atomicOutput{
		file:      file,
		finalPath: path,
		stdout:    stdout,
		temporary: file.Name(),
	}, nil
}

func (o *atomicOutput) commit() error {
	if err := o.file.Sync(); err != nil {
		return fmt.Errorf("sync temporary output: %w", err)
	}

	if _, err := o.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temporary output: %w", err)
	}

	if o.finalPath == "-" {
		if _, err := io.Copy(o.stdout, o.file); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}

		if err := o.file.Close(); err != nil {
			return fmt.Errorf("close temporary output: %w", err)
		}

		if err := os.Remove(o.temporary); err != nil {
			return fmt.Errorf("remove temporary output: %w", err)
		}
	} else {
		if err := o.file.Close(); err != nil {
			return fmt.Errorf("close temporary output: %w", err)
		}

		if err := os.Rename(o.temporary, o.finalPath); err != nil {
			return fmt.Errorf("publish %s: %w", o.finalPath, err)
		}
	}

	o.committed = true

	return nil
}

func (o *atomicOutput) abort() {
	if o == nil || o.committed {
		return
	}

	_ = o.file.Close()
	_ = os.Remove(o.temporary)
}
