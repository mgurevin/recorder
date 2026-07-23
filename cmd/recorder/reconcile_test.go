package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcileDryRunAndApplyUseAuthoritativeCaptures(t *testing.T) {
	root := newBodyStoreLayout(t)
	liveReference := fileBodyReferencePrefix + "0123456789abcdef0123456789abcdef"
	orphanReference := fileBodyReferencePrefix + "fedcba9876543210fedcba9876543210"

	writeAsset(t, root, liveReference, []byte("live"))
	writeAsset(t, root, orphanReference, []byte("orphan"))

	document := decodeTestHAR(t)
	document.Log.Entries[0].Recorder.ResponseBody.Store = liveReference
	path := writeDocument(t, document)

	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{"reconcile", "--json", "--body-store", root, "--grace", "0", path},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	var report reconcileReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if report.Applied || report.Candidates != 1 || report.LiveReferences != 1 {
		t.Fatalf("dry-run report = %#v", report)
	}

	if _, err := os.Stat(assetPath(t, root, orphanReference)); err != nil {
		t.Fatalf("dry-run removed orphan: %v", err)
	}

	output.Reset()

	if err := run(
		context.Background(),
		[]string{
			"reconcile",
			"--json",
			"--body-store", root,
			"--grace", "0",
			"--apply",
			"--authoritative",
			path,
		},
		&output,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if !report.Applied || report.Candidates != 1 || report.CandidateBytes != 6 {
		t.Fatalf("applied report = %#v", report)
	}

	if _, err := os.Stat(assetPath(t, root, orphanReference)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan stat error = %v", err)
	}

	if _, err := os.Stat(assetPath(t, root, liveReference)); err != nil {
		t.Fatalf("live asset removed: %v", err)
	}
}

func TestReconcileCombinesCapturesBeforeMutation(t *testing.T) {
	root := newBodyStoreLayout(t)
	firstReference := fileBodyReferencePrefix + "11111111111111111111111111111111"
	secondReference := fileBodyReferencePrefix + "22222222222222222222222222222222"

	writeAsset(t, root, firstReference, []byte("first"))
	writeAsset(t, root, secondReference, []byte("second"))

	first := decodeTestHAR(t)
	first.Log.Entries[0].Recorder.ResponseBody.Store = firstReference

	second := decodeTestHAR(t)
	second.Log.Entries[0].Recorder.ResponseBody.Store = secondReference

	firstPath := writeDocument(t, first)
	secondPath := filepath.Join(t.TempDir(), "second.har")

	data, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(secondPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run(
		context.Background(),
		[]string{
			"reconcile",
			"--body-store", root,
			"--grace", "0",
			"--apply",
			"--authoritative",
			firstPath,
			secondPath,
		},
		io.Discard,
		io.Discard,
	); err != nil {
		t.Fatal(err)
	}

	for _, reference := range []string{firstReference, secondReference} {
		if _, err := os.Stat(assetPath(t, root, reference)); err != nil {
			t.Fatalf("live asset %s removed: %v", reference, err)
		}
	}
}

func TestReconcileRejectsUnsafeApplyAndInvalidInputs(t *testing.T) {
	root := newBodyStoreLayout(t)
	orphan := fileBodyReferencePrefix + "33333333333333333333333333333333"
	writeAsset(t, root, orphan, []byte("orphan"))

	path := writeDocument(t, decodeTestHAR(t))

	for _, args := range [][]string{
		{"reconcile", path},
		{"reconcile", "--body-store", root},
		{"reconcile", "--body-store", root, "--grace", "-1s", path},
		{"reconcile", "--body-store", root, "--apply", path},
		{"reconcile", "--body-store", root, "-"},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("run(%q) = %v", args, err)
		}
	}

	invalid := filepath.Join(t.TempDir(), "invalid.har")
	if err := os.WriteFile(invalid, []byte(`{"log":`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run(
		context.Background(),
		[]string{
			"reconcile",
			"--body-store", root,
			"--grace", "0",
			"--apply",
			"--authoritative",
			path,
			invalid,
		},
		io.Discard,
		io.Discard,
	)
	if err == nil {
		t.Fatal("invalid capture reconciliation succeeded")
	}

	if _, err := os.Stat(assetPath(t, root, orphan)); err != nil {
		t.Fatalf("asset removed before every capture validated: %v", err)
	}
}

func TestReconcileRefusesApplyWhilePartialsExist(t *testing.T) {
	root := newBodyStoreLayout(t)

	partialPath := filepath.Join(root, "partial", "44444444444444444444444444444444.partial")
	if err := os.WriteFile(partialPath, []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := writeDocument(t, decodeTestHAR(t))

	err := run(
		context.Background(),
		[]string{
			"reconcile",
			"--body-store", root,
			"--grace", "0",
			"--apply",
			"--authoritative",
			path,
		},
		io.Discard,
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "partial body files") {
		t.Fatalf("reconcile error = %v", err)
	}

	if _, err := os.Stat(partialPath); err != nil {
		t.Fatalf("maintenance open removed partial: %v", err)
	}
}

func newBodyStoreLayout(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	for _, name := range []string{"partial", "assets"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	return root
}
