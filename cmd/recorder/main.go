// Command recorder provides bounded offline validation, summaries, conversion,
// evidence verification, fixture preparation and serving, local Inspector
// handoff, compatibility diagnostics, and explicit FileBodyStore
// reconciliation for recorder HAR and NDJSON captures.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
	"github.com/mgurevin/recorder/internal/buildinfo"
)

const (
	defaultInspectorURL = "https://mgurevin.github.io/recorder/"
	formatAuto          = "auto"
	formatHAR           = "har"
	formatNDJSON        = "ndjson"
)

var errUsage = errors.New("usage error")

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "recorder:", err)

		if errors.Is(err, errUsage) {
			os.Exit(2)
		}

		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		if err := printUsage(stderr); err != nil {
			return err
		}

		return errUsage
	}

	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)

	case "summarize":
		return runSummarize(args[1:], stdout, stderr)

	case "convert":
		return runConvert(args[1:], stdout, stderr)

	case "inspect":
		return runInspect(ctx, args[1:], stdout, stderr)

	case "verify":
		return runVerify(args[1:], stdout, stderr)

	case "fixture":
		return runFixture(args[1:], stdout, stderr)

	case "serve-fixture":
		return runServeFixture(ctx, args[1:], stdout, stderr)

	case "doctor":
		return runDoctor(args[1:], stdout, stderr)

	case "reconcile":
		return runReconcile(args[1:], stdout, stderr)

	case "version", "--version", "-version":
		_, err := fmt.Fprintln(stdout, commandVersion())

		return err

	case "help", "--help", "-h":
		return printUsage(stdout)

	default:
		if err := printUsage(stderr); err != nil {
			return err
		}

		return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

func printUsage(writer io.Writer) error {
	_, err := fmt.Fprintln(writer, `Usage: recorder <command> [options]

Commands:
  validate   Validate every entry in a HAR or NDJSON capture
  summarize  Print a bounded metadata-only capture summary
  convert    Convert a validated HAR to NDJSON, or NDJSON to HAR
  inspect    Serve a validated capture on loopback and open the Inspector
  verify     Verify body assets and checksums without modifying them
  fixture    Select exchanges into a deterministic HAR or NDJSON fixture
  serve-fixture
             Serve a network-free hartest fixture on loopback
  doctor     Diagnose CLI, capture schema, and body-store compatibility
  reconcile  Compare authoritative captures with a FileBodyStore
  version    Print the CLI version

Run "recorder <command> -h" for command options.`)

	return err
}

func runValidate(args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("validate", stderr)
	inputFormat := flags.String("format", formatAuto, "input format: auto, har, or ndjson")

	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	path, err := onePath(flags)
	if err != nil {
		return err
	}

	summary, err := scanCapture(path, *inputFormat)
	if err != nil {
		return err
	}

	if *jsonOutput {
		return writeJSON(stdout, struct {
			Valid   bool   `json:"valid"`
			Format  string `json:"format"`
			Entries int    `json:"entries"`
		}{Valid: true, Format: summary.Format, Entries: summary.Entries})
	}

	_, err = fmt.Fprintf(
		stdout,
		"valid %s capture: %d entries\n",
		strings.ToUpper(summary.Format),
		summary.Entries,
	)

	return err
}

func runSummarize(args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("summarize", stderr)
	inputFormat := flags.String("format", formatAuto, "input format: auto, har, or ndjson")

	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	path, err := onePath(flags)
	if err != nil {
		return err
	}

	summary, err := scanCapture(path, *inputFormat)
	if err != nil {
		return err
	}

	if *jsonOutput {
		return writeJSON(stdout, summary)
	}

	return writeSummary(stdout, summary)
}

func runConvert(args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("convert", stderr)
	inputFormat := flags.String("from", formatAuto, "input format: auto, har, or ndjson")
	outputFormat := flags.String("to", "", "output format: har or ndjson")

	outputPath := flags.String("output", "-", `output path, or "-" for stdout`)
	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	inputPath, err := onePath(flags)
	if err != nil {
		return err
	}

	if *outputFormat != formatHAR && *outputFormat != formatNDJSON {
		return fmt.Errorf("%w: -to must be har or ndjson", errUsage)
	}

	reader, closeReader, detectedFormat, err := openCapture(inputPath, *inputFormat)
	if err != nil {
		return err
	}
	defer closeReader()

	if detectedFormat == *outputFormat {
		return fmt.Errorf("%w: input is already %s", errUsage, detectedFormat)
	}

	stream, err := newEntryStream(reader, detectedFormat)
	if err != nil {
		return fmt.Errorf("read %s: %w", inputPath, err)
	}

	output, err := newAtomicOutput(*outputPath, stdout)
	if err != nil {
		return err
	}
	defer output.abort()

	writer := newFixtureWriter(output.file, *outputFormat)
	if err := writer.begin(); err != nil {
		return err
	}

	for {
		entry, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return fmt.Errorf("read %s: %w", inputPath, nextErr)
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

func runInspect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("inspect", stderr)
	inputFormat := flags.String("format", formatAuto, "input format: auto, har, or ndjson")
	inspectorURL := flags.String("inspector-url", defaultInspectorURL, "trusted Inspector base URL")
	serverTLS := addServerTLSFlags(flags)

	noOpen := flags.Bool("no-open", false, "print the URL without opening a browser")
	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	path, err := onePath(flags)
	if err != nil {
		return err
	}

	if path == "-" {
		return fmt.Errorf("%w: inspect requires a file path", errUsage)
	}

	if _, err := scanCapture(path, *inputFormat); err != nil {
		return err
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve capture path: %w", err)
	}

	baseURL, origin, err := validateInspectorURL(*inspectorURL)
	if err != nil {
		return err
	}

	tlsConfig, serverScheme, err := serverTLS.load()
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	captureURL := serverScheme + "://" + listener.Addr().String() + "/capture"

	server := &http.Server{
		Handler:           captureHandler(absolutePath, origin),
		ReadHeaderTimeout: 5 * time.Second,
	}
	defer func() {
		_ = server.Close()
	}()

	go func() {
		serveErr := serveHTTP(server, listener, tlsConfig)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(stderr, "recorder: Inspector server: %v\n", serveErr)
		}
	}()

	query := baseURL.Query()
	query.Set("har", captureURL)

	baseURL.RawQuery = query.Encode()
	if _, err := fmt.Fprintf(stdout, "Inspector: %s\n", baseURL.String()); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(stdout, "Serving the capture on loopback; press Ctrl-C to stop."); err != nil {
		return err
	}

	if !*noOpen {
		if err := openBrowser(baseURL.String()); err != nil {
			_, _ = fmt.Fprintf(stderr, "recorder: open browser: %v\n", err)
		}
	}

	signalContext, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	<-signalContext.Done()

	return nil
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)

	return flags
}

// parseCommandFlags preserves the standard flag package's parsing and error
// behavior while allowing options to appear before or after positional paths.
func parseCommandFlags(flags *flag.FlagSet, args []string) error {
	options := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))

	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)

			break
		}

		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)

			continue
		}

		options = append(options, arg)

		name := strings.TrimLeft(arg, "-")
		if separator := strings.IndexByte(name, '='); separator >= 0 {
			name = name[:separator]
		}

		registered := flags.Lookup(name)
		if registered == nil || strings.Contains(arg, "=") || isBooleanFlag(registered) {
			continue
		}

		if index+1 < len(args) {
			index++
			options = append(options, args[index])
		}
	}

	return flags.Parse(append(options, positionals...))
}

func isBooleanFlag(value *flag.Flag) bool {
	boolean, ok := value.Value.(interface{ IsBoolFlag() bool })

	return ok && boolean.IsBoolFlag()
}

func onePath(flags *flag.FlagSet) (string, error) {
	if flags.NArg() != 1 {
		return "", fmt.Errorf("%w: exactly one input path is required", errUsage)
	}

	return flags.Arg(0), nil
}

func usageError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}

	return fmt.Errorf("%w: %v", errUsage, err)
}

type captureSummary struct {
	Format            string         `json:"format"`
	Entries           int            `json:"entries"`
	Methods           map[string]int `json:"methods"`
	StatusClasses     map[string]int `json:"statusClasses"`
	Hosts             map[string]int `json:"hosts"`
	Traces            int            `json:"traces"`
	Failures          int            `json:"failures"`
	TruncatedBodies   int            `json:"truncatedBodies"`
	IncompleteBodies  int            `json:"incompleteBodies"`
	ClosedEarlyBodies int            `json:"closedEarlyBodies"`
	CapturedBytes     int64          `json:"capturedBytes"`
	TotalDurationMS   float64        `json:"totalDurationMs"`
}

func newCaptureSummary(format string) *captureSummary {
	return &captureSummary{
		Format:        format,
		Methods:       make(map[string]int),
		StatusClasses: make(map[string]int),
		Hosts:         make(map[string]int),
	}
}

func (s *captureSummary) add(entry *recorder.Entry, traceIDs map[string]struct{}) {
	s.Entries++
	s.TotalDurationMS += entry.Time

	if entry.Request != nil {
		s.Methods[entry.Request.Method]++
		if parsed, err := url.Parse(entry.Request.URL); err == nil && parsed.Hostname() != "" {
			s.Hosts[parsed.Hostname()]++
		}
	}

	if entry.Response != nil {
		statusClass := "transport-error"
		if entry.Response.Status > 0 {
			statusClass = strconv.Itoa(entry.Response.Status/100) + "xx"
		}

		s.StatusClasses[statusClass]++
	}

	if entry.Recorder == nil {
		return
	}

	if entry.Recorder.TraceID != "" {
		traceIDs[entry.Recorder.TraceID] = struct{}{}
	}

	if entry.Recorder.Error != nil {
		s.Failures++
	}

	s.addBody(entry.Recorder.RequestBody)
	s.addBody(entry.Recorder.ResponseBody)
}

func (s *captureSummary) addBody(body *recorder.BodyInfo) {
	if body == nil || !body.Present {
		return
	}

	s.CapturedBytes += body.CapturedBytes
	if body.Truncated {
		s.TruncatedBodies++
	}

	if !body.Complete {
		s.IncompleteBodies++
	}

	if body.ClosedEarly {
		s.ClosedEarlyBodies++
	}
}

func scanCapture(path, requestedFormat string) (*captureSummary, error) {
	reader, closeReader, format, err := openCapture(path, requestedFormat)
	if err != nil {
		return nil, err
	}
	defer closeReader()

	stream, err := newEntryStream(reader, format)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	summary := newCaptureSummary(format)
	traceIDs := make(map[string]struct{})

	for {
		entry, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, nextErr)
		}

		summary.add(entry, traceIDs)
	}

	summary.Traces = len(traceIDs)

	return summary, nil
}

func readCapture(path, requestedFormat string) (*recorder.HAR, []*recorder.Entry, string, error) {
	reader, closeReader, format, err := openCapture(path, requestedFormat)
	if err != nil {
		return nil, nil, "", err
	}
	defer closeReader()

	switch format {
	case formatHAR:
		document, readErr := hario.ReadHAR(reader, hario.DefaultReadConfig())
		if readErr != nil {
			return nil, nil, "", fmt.Errorf("read %s: %w", path, readErr)
		}

		return document, document.Log.Entries, format, nil

	case formatNDJSON:
		entries, readErr := hario.ReadNDJSON(reader, hario.DefaultReadConfig())
		if readErr != nil {
			return nil, nil, "", fmt.Errorf("read %s: %w", path, readErr)
		}

		return nil, entries, format, nil
	}

	panic("unreachable capture format")
}

func openCapture(path, requestedFormat string) (io.Reader, func(), string, error) {
	format, err := resolveFormat(path, requestedFormat)
	if err != nil {
		return nil, nil, "", err
	}

	if path == "-" {
		return os.Stdin, func() {}, format, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open %s: %w", path, err)
	}

	return file, func() { _ = file.Close() }, format, nil
}

func resolveFormat(path, requested string) (string, error) {
	switch strings.ToLower(requested) {
	case formatHAR, formatNDJSON:
		return strings.ToLower(requested), nil

	case formatAuto:

	default:
		return "", fmt.Errorf("%w: format must be auto, har, or ndjson", errUsage)
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".har":
		return formatHAR, nil

	case ".ndjson", ".jsonl":
		return formatNDJSON, nil

	default:
		return "", fmt.Errorf("%w: cannot infer input format; use -format or -from", errUsage)
	}
}

func newEntryStream(reader io.Reader, format string) (*hario.EntryStream, error) {
	switch format {
	case formatHAR:
		return hario.NewHARStream(reader, hario.DefaultReadConfig())

	case formatNDJSON:
		return hario.NewNDJSONStream(reader, hario.DefaultReadConfig())

	default:
		return nil, fmt.Errorf("%w: unsupported format %q", errUsage, format)
	}
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")

	return encoder.Encode(value)
}

func writeSummary(writer io.Writer, summary *captureSummary) error {
	var output strings.Builder

	fmt.Fprintf(&output, "Format: %s\n", strings.ToUpper(summary.Format))
	fmt.Fprintf(&output, "Entries: %d\n", summary.Entries)
	fmt.Fprintf(&output, "Traces: %d\n", summary.Traces)
	fmt.Fprintf(&output, "Failures: %d\n", summary.Failures)
	fmt.Fprintf(&output, "Duration: %.3f ms\n", summary.TotalDurationMS)
	fmt.Fprintf(&output, "Captured body bytes: %d\n", summary.CapturedBytes)
	fmt.Fprintf(
		&output,
		"Bodies: %d truncated, %d incomplete, %d closed early\n",
		summary.TruncatedBodies,
		summary.IncompleteBodies,
		summary.ClosedEarlyBodies,
	)
	writeCounts(&output, "Methods", summary.Methods)
	writeCounts(&output, "Status classes", summary.StatusClasses)
	writeCounts(&output, "Hosts", summary.Hosts)

	_, err := io.WriteString(writer, output.String())

	return err
}

func writeCounts(writer *strings.Builder, label string, counts map[string]int) {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	fmt.Fprintf(writer, "%s:", label)

	for _, key := range keys {
		fmt.Fprintf(writer, " %s=%d", key, counts[key])
	}

	fmt.Fprintln(writer)
}

func validateInspectorURL(value string) (*url.URL, string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid Inspector URL: %v", errUsage, err)
	}

	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname())) {
		return nil, "", fmt.Errorf("%w: Inspector URL must use HTTPS or loopback HTTP", errUsage)
	}

	if parsed.Host == "" || parsed.User != nil {
		return nil, "", fmt.Errorf("%w: Inspector URL must have a host and no credentials", errUsage)
	}

	origin := parsed.Scheme + "://" + parsed.Host

	return parsed, origin, nil
}

func captureHandler(path, allowedOrigin string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/capture" {
			http.NotFound(writer, request)

			return
		}

		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		origin := request.Header.Get("Origin")
		if origin != "" && origin != allowedOrigin {
			http.Error(writer, "origin forbidden", http.StatusForbidden)

			return
		}

		if origin == allowedOrigin {
			writer.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			writer.Header().Set("Vary", "Origin")
		}

		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(writer, request, path)
	})
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	address := net.ParseIP(host)

	return address != nil && address.IsLoopback()
}

func openBrowser(target string) error {
	var command *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)

	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)

	default:
		command = exec.Command("xdg-open", target)
	}

	return command.Start()
}

func commandVersion() string {
	return "recorder " + buildinfo.Version()
}
