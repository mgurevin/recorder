package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mgurevin/recorder"
	"github.com/mgurevin/recorder/hario"
)

const (
	doctorPass = "pass"
	doctorWarn = "warn"
	doctorFail = "fail"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	Healthy       bool          `json:"healthy"`
	CLI           string        `json:"cli"`
	Go            string        `json:"go"`
	Platform      string        `json:"platform"`
	SchemaVersion string        `json:"schemaVersion"`
	Capture       string        `json:"capture,omitempty"`
	Checks        []doctorCheck `json:"checks"`
}

func runDoctor(args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("doctor", stderr)
	inputFormat := flags.String("format", formatAuto, "input format: auto, har, or ndjson")
	bodyStore := flags.String("body-store", "", "optional FileBodyStore root containing assets/")
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")

	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	if flags.NArg() > 1 {
		return fmt.Errorf("%w: doctor accepts at most one capture path", errUsage)
	}

	report := newDoctorReport()

	if *bodyStore != "" {
		report.add(checkBodyStore(*bodyStore))
	}

	if flags.NArg() == 1 {
		path := flags.Arg(0)
		if path == "-" {
			return fmt.Errorf("%w: doctor requires a file path, not stdin", errUsage)
		}

		report.Capture = filepath.Base(path)

		_, entries, format, err := readCapture(path, *inputFormat)
		if err != nil {
			report.add(doctorCheck{
				Name:   "capture",
				Status: doctorFail,
				Detail: safeCaptureError(err),
			})
		} else {
			report.add(doctorCheck{
				Name:   "capture",
				Status: doctorPass,
				Detail: fmt.Sprintf("%s, %d entries", strings.ToUpper(format), len(entries)),
			})

			references := countBodyReferences(entries)
			switch {
			case references == 0:
				report.add(doctorCheck{
					Name:   "body-assets",
					Status: doctorPass,
					Detail: "capture has no external body references",
				})

			case *bodyStore == "":
				report.add(doctorCheck{
					Name:   "body-assets",
					Status: doctorWarn,
					Detail: fmt.Sprintf("%d external references not checked; provide -body-store", references),
				})

			default:
				verification, verifyErr := verifyCapture(entries, format, *bodyStore)
				if verifyErr != nil {
					report.add(doctorCheck{
						Name:   "body-assets",
						Status: doctorFail,
						Detail: safeDoctorError(verifyErr),
					})
				} else if verification.Missing > 0 || verification.Modified > 0 {
					report.add(doctorCheck{
						Name:   "body-assets",
						Status: doctorFail,
						Detail: fmt.Sprintf(
							"%d verified checksums, %d missing, %d modified",
							verification.ChecksumsVerified,
							verification.Missing,
							verification.Modified,
						),
					})
				} else {
					report.add(doctorCheck{
						Name:   "body-assets",
						Status: doctorPass,
						Detail: fmt.Sprintf(
							"%d references, %d verified checksums, %d skipped checksums",
							verification.BodyReferences,
							verification.ChecksumsVerified,
							verification.ChecksumsSkipped,
						),
					})
				}
			}
		}
	}

	if *jsonOutput {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else if err := writeDoctorReport(stdout, report); err != nil {
		return err
	}

	if !report.Healthy {
		return fmt.Errorf("doctor found incompatible or invalid state")
	}

	return nil
}

func newDoctorReport() *doctorReport {
	report := &doctorReport{
		Healthy:       true,
		CLI:           commandVersion(),
		Go:            runtime.Version(),
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		SchemaVersion: recorder.RecorderExtensionVersion,
		Checks:        make([]doctorCheck, 0, 4),
	}

	report.add(doctorCheck{
		Name:   "runtime",
		Status: doctorPass,
		Detail: report.Go + " " + report.Platform,
	})
	report.add(doctorCheck{
		Name:   "schema",
		Status: doctorPass,
		Detail: "supports recorder extension version " + report.SchemaVersion,
	})

	return report
}

func (r *doctorReport) add(check doctorCheck) {
	r.Checks = append(r.Checks, check)
	if check.Status == doctorFail {
		r.Healthy = false
	}
}

func checkBodyStore(root string) doctorCheck {
	assetsPath := filepath.Join(root, "assets")

	info, err := os.Lstat(assetsPath)
	if err != nil {
		return doctorCheck{
			Name:   "body-store",
			Status: doctorFail,
			Detail: "assets directory is unavailable",
		}
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return doctorCheck{
			Name:   "body-store",
			Status: doctorFail,
			Detail: "assets path must not be a symbolic link",
		}
	}

	if !info.IsDir() {
		return doctorCheck{
			Name:   "body-store",
			Status: doctorFail,
			Detail: "assets path is not a directory",
		}
	}

	directory, err := os.Open(assetsPath)
	if err != nil {
		return doctorCheck{
			Name:   "body-store",
			Status: doctorFail,
			Detail: "assets directory is not readable",
		}
	}

	closeErr := directory.Close()
	if closeErr != nil {
		return doctorCheck{
			Name:   "body-store",
			Status: doctorFail,
			Detail: "assets directory could not be closed safely",
		}
	}

	return doctorCheck{
		Name:   "body-store",
		Status: doctorPass,
		Detail: "assets directory is readable",
	}
}

func countBodyReferences(entries []*recorder.Entry) int {
	count := 0

	for _, entry := range entries {
		for _, body := range entryBodies(entry) {
			if body.info != nil && body.info.Store != "" {
				count++
			}
		}
	}

	return count
}

func safeDoctorError(_ error) string {
	return "body assets could not be verified safely"
}

func safeCaptureError(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "capture file is unavailable"

	case errors.Is(err, hario.ErrLimitExceeded):
		return "capture exceeds configured safety limits"

	case errors.Is(err, hario.ErrInvalidHAR):
		return "capture is structurally invalid or uses an unsupported schema"

	default:
		return "capture could not be read safely"
	}
}

func writeDoctorReport(writer io.Writer, report *doctorReport) error {
	var output strings.Builder

	fmt.Fprintf(&output, "CLI: %s\n", report.CLI)
	fmt.Fprintf(&output, "Runtime: %s %s\n", report.Go, report.Platform)
	fmt.Fprintf(&output, "Recorder schema: %s\n", report.SchemaVersion)

	if report.Capture != "" {
		fmt.Fprintf(&output, "Capture: %s\n", report.Capture)
	}

	for _, check := range report.Checks {
		fmt.Fprintf(&output, "[%s] %s: %s\n", strings.ToUpper(check.Status), check.Name, check.Detail)
	}

	_, err := io.WriteString(writer, output.String())

	return err
}
