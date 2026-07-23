package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mgurevin/recorder"
)

func TestDoctorReportsRuntimeWithoutCapture(t *testing.T) {
	var output bytes.Buffer

	if err := run(context.Background(), []string{"doctor", "--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}

	var report doctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if !report.Healthy || report.Go != runtime.Version() {
		t.Fatalf("report = %#v", report)
	}

	if report.SchemaVersion != recorder.RecorderExtensionVersion || len(report.Checks) != 2 {
		t.Fatalf("report contract = %#v", report)
	}
}

func TestDoctorDiagnosesCaptureAndBodyStore(t *testing.T) {
	document := decodeTestHAR(t)
	body := []byte(`{"ok":true}`)
	reference := fileBodyReferencePrefix + "0123456789abcdef0123456789abcdef"
	info := document.Log.Entries[0].Recorder.ResponseBody
	info.Store = reference
	info.Present = true
	info.Complete = true
	info.CapturedBytes = int64(len(body))
	info.TotalBytes = int64(len(body))
	info.HashAlgorithm = "sha256"
	info.Hash, _ = digest("sha256", body)

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}

	writeAsset(t, root, reference, body)
	path := writeDocument(t, document)

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"doctor", "--json", "--body-store", root, path},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	var report doctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if !report.Healthy || len(report.Checks) != 5 {
		t.Fatalf("report = %#v", report)
	}

	if report.Checks[4].Status != doctorPass || !strings.Contains(report.Checks[4].Detail, "1 verified") {
		t.Fatalf("asset check = %#v", report.Checks[4])
	}

	output.Reset()

	if err := run(context.Background(), []string{"doctor", path}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), "[WARN] body-assets") {
		t.Fatalf("text report = %s", output.String())
	}
}

func TestDoctorFailsForInvalidCaptureAndStore(t *testing.T) {
	invalidCapture := filepath.Join(t.TempDir(), "invalid.har")
	if err := os.WriteFile(invalidCapture, []byte(`{"log":`), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer

	err := run(
		context.Background(),
		[]string{"doctor", "--json", "--body-store", t.TempDir(), invalidCapture},
		&output,
		io.Discard,
	)
	if err == nil {
		t.Fatal("doctor succeeded")
	}

	var report doctorReport
	if unmarshalErr := json.Unmarshal(output.Bytes(), &report); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}

	if report.Healthy {
		t.Fatalf("report = %#v", report)
	}

	for _, check := range report.Checks {
		if strings.Contains(check.Detail, invalidCapture) {
			t.Fatalf("report exposes input path = %#v", report)
		}
	}
}

func TestDoctorRejectsUnsafeArgumentsAndBodyStoreShapes(t *testing.T) {
	for _, args := range [][]string{
		{"doctor", "-"},
		{"doctor", "one.har", "two.har"},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("run(%q) = %v", args, err)
		}
	}

	root := t.TempDir()

	assets := filepath.Join(root, "assets")
	if err := os.WriteFile(assets, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	check := checkBodyStore(root)
	if check.Status != doctorFail {
		t.Fatalf("file assets check = %#v", check)
	}

	if runtime.GOOS != "windows" {
		linkRoot := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(linkRoot, "assets")); err != nil {
			t.Fatal(err)
		}

		check = checkBodyStore(linkRoot)
		if check.Status != doctorFail {
			t.Fatalf("symlink assets check = %#v", check)
		}
	}
}
