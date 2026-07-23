import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  normalizeVersion,
  prepareRelease,
  releaseNotes,
  retireExceptions,
  verifyRelease,
} from "./release.mjs";

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "recorder-release-"));
  fs.mkdirSync(path.join(root, "inspector"), { recursive: true });
  fs.mkdirSync(path.join(root, "api"), { recursive: true });
  fs.writeFileSync(
    path.join(root, "CHANGELOG.md"),
    [
      "# Changelog",
      "",
      "## [Unreleased]",
      "",
      "### Added",
      "",
      "- New evidence.",
      "",
      "## [0.4.2] - 2026-07-22",
      "",
      "[Unreleased]: https://github.com/mgurevin/recorder/compare/v0.4.2...HEAD",
      "[0.4.2]: https://example.invalid",
      "",
    ].join("\n"),
  );
  fs.writeFileSync(path.join(root, "har.go"), '\tcreatorVersion = "0.4.2"\n');
  fs.writeFileSync(
    path.join(root, "integration_test.go"),
    [
      'if got := creator["name"]; got != "github.com/mgurevin/recorder" {}',
      'if got, ok := creator["version"].(string); !ok || got != "0.4.2" {',
      '\tfatal(creator["version"], "0.4.2")',
      "}",
      "",
    ].join("\n"),
  );
  fs.writeFileSync(
    path.join(root, "inspector/package.json"),
    '{"name":"recorder-inspector","version":"0.4.2"}\n',
  );
  fs.writeFileSync(
    path.join(root, "inspector/package-lock.json"),
    '{"version":"0.4.2","packages":{"":{"version":"0.4.2"}}}\n',
  );
  fs.writeFileSync(
    path.join(root, "api/compatibility-exceptions.txt"),
    [
      "# Reviewed exceptions.",
      "[github.com/mgurevin/recorder]",
      "- RootChange: removed",
      "",
      "[github.com/mgurevin/recorder/otelrecorder]",
      "- OTelChange: removed",
      "",
    ].join("\n"),
  );
  return root;
}

test("normalizeVersion accepts a stable semantic version", () => {
  assert.equal(normalizeVersion("v1.2.3"), "1.2.3");
  assert.throws(() => normalizeVersion("1.2"), /VERSION=X.Y.Z/);
});

test("prepareRelease updates every release-owned version and notes", () => {
  const root = fixture();
  prepareRelease(root, "0.5.0", "2026-07-23");
  verifyRelease(root, "0.5.0");

  assert.match(releaseNotes(root, "0.5.0"), /New evidence/);
  assert.match(
    fs.readFileSync(path.join(root, "CHANGELOG.md"), "utf8"),
    /\[0\.5\.0\]: .*v0\.4\.2\.\.\.v0\.5\.0/,
  );
  assert.match(
    fs.readFileSync(path.join(root, "integration_test.go"), "utf8"),
    /got != "github\.com\/mgurevin\/recorder"/,
  );
});

test("prepareRelease rejects an empty Unreleased section", () => {
  const root = fixture();
  fs.writeFileSync(
    path.join(root, "CHANGELOG.md"),
    "## [Unreleased]\n\n## [0.4.2]\n\n[Unreleased]: https://github.com/mgurevin/recorder/compare/v0.4.2...HEAD\n",
  );
  assert.throws(() => prepareRelease(root, "0.5.0", "2026-07-23"), /section is empty/);
});

test("retireExceptions removes only the selected module section", () => {
  const root = fixture();
  retireExceptions(root, "github.com/mgurevin/recorder");
  const result = fs.readFileSync(path.join(root, "api/compatibility-exceptions.txt"), "utf8");

  assert.doesNotMatch(result, /RootChange/);
  assert.match(result, /OTelChange/);
});
