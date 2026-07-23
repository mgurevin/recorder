package main

import (
	"crypto/md5"  //nolint:gosec // Verification must reproduce recorded legacy hashes.
	"crypto/sha1" //nolint:gosec // Verification must reproduce recorded legacy hashes.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mgurevin/recorder"
)

const fileBodyReferencePrefix = "filebody:v1:"

var errVerificationFailed = errors.New("verification failed")

type verificationIssue struct {
	Entry     *int   `json:"entry,omitempty"`
	Direction string `json:"direction,omitempty"`
	Kind      string `json:"kind"`
	Reference string `json:"reference,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type verificationReport struct {
	Format            string              `json:"format"`
	Entries           int                 `json:"entries"`
	BodyReferences    int                 `json:"bodyReferences"`
	ChecksumsVerified int                 `json:"checksumsVerified"`
	ChecksumsSkipped  int                 `json:"checksumsSkipped"`
	Missing           int                 `json:"missing"`
	Modified          int                 `json:"modified"`
	Orphans           int                 `json:"orphans"`
	Issues            []verificationIssue `json:"issues"`
}

func runVerify(args []string, stdout, stderr io.Writer) error {
	flags := newFlagSet("verify", stderr)
	inputFormat := flags.String("format", formatAuto, "input format: auto, har, or ndjson")
	bodyStore := flags.String("body-store", "", "FileBodyStore root containing assets/")
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")

	if err := parseCommandFlags(flags, args); err != nil {
		return usageError(err)
	}

	path, err := onePath(flags)
	if err != nil {
		return err
	}

	_, entries, format, err := readCapture(path, *inputFormat)
	if err != nil {
		return err
	}

	report, err := verifyCapture(entries, format, *bodyStore)
	if err != nil {
		return err
	}

	if *jsonOutput {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else if err := writeVerificationReport(stdout, report); err != nil {
		return err
	}

	if report.Missing > 0 || report.Modified > 0 {
		return errVerificationFailed
	}

	return nil
}

func verifyCapture(entries []*recorder.Entry, format, bodyStore string) (*verificationReport, error) {
	report := &verificationReport{Format: format, Entries: len(entries), Issues: []verificationIssue{}}
	liveReferences := make(map[string]struct{})

	if bodyStore == "" {
		for _, entry := range entries {
			for _, body := range entryBodies(entry) {
				if body.info != nil && body.info.Store != "" {
					return nil, fmt.Errorf("%w: -body-store is required for external body references", errUsage)
				}
			}
		}
	}

	for index, entry := range entries {
		for _, body := range entryBodies(entry) {
			if body.info == nil || !body.info.Present {
				continue
			}

			data, available, issue := bodyRepresentation(index, entry, body, bodyStore)
			if issue != nil {
				report.addIssue(*issue)

				continue
			}

			if body.info.Store != "" {
				report.BodyReferences++
				liveReferences[body.info.Store] = struct{}{}
			}

			if body.info.Hash == "" {
				continue
			}

			if !available || !bodyHashComparable(entry, body) {
				report.ChecksumsSkipped++

				continue
			}

			actual, hashErr := digest(body.info.HashAlgorithm, data)
			if hashErr != nil {
				report.addIssue(verificationIssue{
					Entry:     intPointer(index),
					Direction: body.direction,
					Kind:      "unsupported-hash",
					Detail:    hashErr.Error(),
				})

				continue
			}

			if !strings.EqualFold(actual, body.info.Hash) {
				report.addIssue(verificationIssue{
					Entry:     intPointer(index),
					Direction: body.direction,
					Kind:      "checksum-mismatch",
					Reference: body.info.Store,
				})

				continue
			}

			report.ChecksumsVerified++
		}
	}

	if bodyStore != "" {
		orphans, err := findOrphanAssets(bodyStore, liveReferences)
		if err != nil {
			return nil, err
		}

		for _, reference := range orphans {
			report.Orphans++
			report.Issues = append(report.Issues, verificationIssue{
				Kind:      "orphan",
				Reference: reference,
			})
		}
	}

	return report, nil
}

type entryBody struct {
	direction string
	info      *recorder.BodyInfo
}

func entryBodies(entry *recorder.Entry) []entryBody {
	if entry == nil || entry.Recorder == nil {
		return nil
	}

	return []entryBody{
		{direction: "request", info: entry.Recorder.RequestBody},
		{direction: "response", info: entry.Recorder.ResponseBody},
	}
}

func bodyRepresentation(
	entryIndex int,
	entry *recorder.Entry,
	body entryBody,
	bodyStore string,
) ([]byte, bool, *verificationIssue) {
	if body.info.Store != "" {
		data, err := readBodyAsset(bodyStore, body.info.Store)
		if err != nil {
			kind := "missing"
			if !errors.Is(err, os.ErrNotExist) {
				kind = "modified"
			}

			return nil, false, &verificationIssue{
				Entry:     intPointer(entryIndex),
				Direction: body.direction,
				Kind:      kind,
				Reference: body.info.Store,
				Detail:    safeAssetError(err),
			}
		}

		if int64(len(data)) != body.info.CapturedBytes {
			return nil, false, &verificationIssue{
				Entry:     intPointer(entryIndex),
				Direction: body.direction,
				Kind:      "size-mismatch",
				Reference: body.info.Store,
				Detail:    fmt.Sprintf("capturedBytes=%d assetBytes=%d", body.info.CapturedBytes, len(data)),
			}
		}

		return data, true, nil
	}

	data, ok := embeddedBody(entry, body.direction)

	return data, ok, nil
}

func readBodyAsset(root, reference string) ([]byte, error) {
	if root == "" {
		return nil, errors.New("body store is required for external reference")
	}

	id, err := parseFileBodyReference(reference)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(root, "assets", id+".body")

	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("body asset is not a regular file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func parseFileBodyReference(reference string) (string, error) {
	if !strings.HasPrefix(reference, fileBodyReferencePrefix) {
		return "", errors.New("unsupported body reference")
	}

	id := strings.TrimPrefix(reference, fileBodyReferencePrefix)
	if len(id) != 32 {
		return "", errors.New("invalid file body reference")
	}

	for _, char := range id {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return "", errors.New("invalid file body reference")
		}
	}

	return id, nil
}

func safeAssetError(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "asset does not exist"
	}

	return "asset could not be read safely"
}

func intPointer(value int) *int {
	return &value
}

func embeddedBody(entry *recorder.Entry, direction string) ([]byte, bool) {
	if direction == "request" {
		if entry.Request == nil || entry.Request.PostData == nil {
			return nil, false
		}

		text := entry.Request.PostData.Text
		if entry.Recorder != nil && entry.Recorder.RequestBodyEncoding == "base64" {
			data, err := base64.StdEncoding.DecodeString(text)

			return data, err == nil
		}

		return []byte(text), true
	}

	if entry.Response == nil || entry.Response.Content == nil {
		return nil, false
	}

	if entry.Response.Content.Encoding == "base64" {
		data, err := base64.StdEncoding.DecodeString(entry.Response.Content.Text)

		return data, err == nil
	}

	return []byte(entry.Response.Content.Text), true
}

func bodyHashComparable(entry *recorder.Entry, body entryBody) bool {
	if !body.info.Complete || body.info.Truncated || body.info.CapturedBytes != body.info.TotalBytes {
		return false
	}

	if body.direction == "response" && entry.Recorder != nil && entry.Recorder.ResponseBodyDecoded {
		return false
	}

	if entry.Recorder == nil || entry.Recorder.Redaction == nil {
		return true
	}

	scope := entry.Recorder.Redaction.Request
	if body.direction == "response" {
		scope = entry.Recorder.Redaction.Response
	}

	if scope == nil || scope.Body == nil {
		return true
	}

	return scope.Body.Outcome == recorder.BodyRedactionUnchanged
}

func digest(algorithm string, data []byte) (string, error) {
	var sum []byte

	switch strings.ToLower(algorithm) {
	case "md5":
		value := md5.Sum(data) //nolint:gosec // Reproduces recorded evidence.
		sum = value[:]

	case "sha1":
		value := sha1.Sum(data) //nolint:gosec // Reproduces recorded evidence.
		sum = value[:]

	case "sha256":
		value := sha256.Sum256(data)
		sum = value[:]

	default:
		return "", fmt.Errorf("unsupported hash algorithm %q", algorithm)
	}

	return hex.EncodeToString(sum), nil
}

func findOrphanAssets(root string, live map[string]struct{}) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "assets"))
	if err != nil {
		return nil, fmt.Errorf("scan body store assets: %w", err)
	}

	orphans := make([]string, 0)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".body") {
			continue
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("symlink found in body store assets")
		}

		id := strings.TrimSuffix(entry.Name(), ".body")

		reference := fileBodyReferencePrefix + id
		if _, ok := live[reference]; !ok {
			orphans = append(orphans, reference)
		}
	}

	sort.Strings(orphans)

	return orphans, nil
}

func (r *verificationReport) addIssue(issue verificationIssue) {
	r.Issues = append(r.Issues, issue)

	switch issue.Kind {
	case "missing":
		r.Missing++

	case "orphan":
		r.Orphans++

	default:
		r.Modified++
	}
}

func writeVerificationReport(writer io.Writer, report *verificationReport) error {
	var output strings.Builder

	fmt.Fprintf(&output, "Format: %s\n", strings.ToUpper(report.Format))
	fmt.Fprintf(&output, "Entries: %d\n", report.Entries)
	fmt.Fprintf(&output, "Body references: %d\n", report.BodyReferences)
	fmt.Fprintf(&output, "Checksums: %d verified, %d skipped\n", report.ChecksumsVerified, report.ChecksumsSkipped)
	fmt.Fprintf(&output, "Assets: %d missing, %d modified, %d orphaned\n", report.Missing, report.Modified, report.Orphans)

	for _, issue := range report.Issues {
		entry := "-"
		if issue.Entry != nil {
			entry = fmt.Sprintf("%d", *issue.Entry)
		}

		fmt.Fprintf(
			&output,
			"- %s entry=%s direction=%s reference=%s\n",
			issue.Kind,
			entry,
			issue.Direction,
			issue.Reference,
		)
	}

	_, err := io.WriteString(writer, output.String())

	return err
}
