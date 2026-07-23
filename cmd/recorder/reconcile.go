package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mgurevin/recorder"
)

const defaultReconcileGrace = 24 * time.Hour

type reconcileReport struct {
	Applied        bool     `json:"applied"`
	Captures       int      `json:"captures"`
	Entries        int      `json:"entries"`
	LiveReferences int      `json:"liveReferences"`
	Scanned        int      `json:"scanned"`
	Candidates     int      `json:"candidates"`
	CandidateBytes int64    `json:"candidateBytes"`
	Grace          string   `json:"grace"`
	Warnings       []string `json:"warnings"`
}

func runReconcile(args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("reconcile", stderr)
	inputFormat := flags.String("format", formatAuto, "input format for every capture: auto, har, or ndjson")
	bodyStore := flags.String("body-store", "", "FileBodyStore root containing assets/")
	grace := flags.Duration("grace", defaultReconcileGrace, "minimum orphan age eligible for cleanup")
	apply := flags.Bool("apply", false, "delete eligible unreferenced committed assets")
	authoritative := flags.Bool(
		"authoritative",
		false,
		"confirm supplied captures are the complete live-reference set",
	)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")

	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	if *bodyStore == "" {
		return fmt.Errorf("%w: -body-store is required", errUsage)
	}

	if flags.NArg() == 0 {
		return fmt.Errorf("%w: at least one authoritative capture path is required", errUsage)
	}

	if *grace < 0 {
		return fmt.Errorf("%w: -grace must not be negative", errUsage)
	}

	if *apply && !*authoritative {
		return fmt.Errorf("%w: -apply requires -authoritative", errUsage)
	}

	live, entries, err := collectLiveBodyReferences(flags.Args(), *inputFormat)
	if err != nil {
		return err
	}

	config := recorder.DefaultFileBodyStoreConfig()
	config.MaxBytes = math.MaxInt64
	config.MaxFiles = int(^uint(0) >> 1)
	config.MaintenanceMode = true

	store, err := recorder.NewFileBodyStore(*bodyStore, config)
	if err != nil {
		return fmt.Errorf("open body store for maintenance: %w", err)
	}

	if *apply && store.Stats().PartialFiles > 0 {
		return errors.New("refusing cleanup while partial body files exist; stop writers and resolve partials first")
	}

	result, err := store.Reconcile(live, *grace, !*apply)
	if err != nil {
		return fmt.Errorf("reconcile body store: %w", err)
	}

	report := reconcileReport{
		Applied:        *apply,
		Captures:       flags.NArg(),
		Entries:        entries,
		LiveReferences: len(live),
		Scanned:        result.Scanned,
		Candidates:     result.Released,
		CandidateBytes: result.ReleasedBytes,
		Grace:          grace.String(),
		Warnings:       []string{},
	}

	if !*apply {
		report.Warnings = append(
			report.Warnings,
			"dry-run only; no committed body assets were deleted",
		)
	}

	if *jsonOutput {
		return writeJSON(stdout, report)
	}

	return writeReconcileReport(stdout, report)
}

func collectLiveBodyReferences(paths []string, format string) ([]string, int, error) {
	live := make(map[string]struct{})
	entries := 0

	for _, path := range paths {
		if path == "-" {
			return nil, 0, fmt.Errorf("%w: reconcile requires file paths, not stdin", errUsage)
		}

		reader, closeReader, detectedFormat, err := openCapture(path, format)
		if err != nil {
			return nil, 0, err
		}

		stream, err := newEntryStream(reader, detectedFormat)
		if err != nil {
			closeReader()

			return nil, 0, fmt.Errorf("read %s: %w", path, err)
		}

		for {
			entry, nextErr := stream.Next()
			if errors.Is(nextErr, io.EOF) {
				break
			}

			if nextErr != nil {
				closeReader()

				return nil, 0, fmt.Errorf("read %s: %w", path, nextErr)
			}

			entries++

			for _, body := range entryBodies(entry) {
				if body.info != nil && body.info.Store != "" {
					live[body.info.Store] = struct{}{}
				}
			}
		}

		closeReader()
	}

	references := make([]string, 0, len(live))
	for reference := range live {
		references = append(references, reference)
	}

	sort.Strings(references)

	return references, entries, nil
}

func writeReconcileReport(writer io.Writer, report reconcileReport) error {
	var output strings.Builder

	mode := "DRY-RUN"
	if report.Applied {
		mode = "APPLIED"
	}

	fmt.Fprintf(&output, "Mode: %s\n", mode)
	fmt.Fprintf(&output, "Captures: %d\n", report.Captures)
	fmt.Fprintf(&output, "Entries: %d\n", report.Entries)
	fmt.Fprintf(&output, "Live body references: %d\n", report.LiveReferences)
	fmt.Fprintf(&output, "Committed assets scanned: %d\n", report.Scanned)
	fmt.Fprintf(&output, "Eligible unreferenced assets: %d (%d bytes)\n", report.Candidates, report.CandidateBytes)
	fmt.Fprintf(&output, "Grace: %s\n", report.Grace)

	for _, warning := range report.Warnings {
		fmt.Fprintf(&output, "Warning: %s\n", warning)
	}

	_, err := io.WriteString(writer, output.String())

	return err
}
