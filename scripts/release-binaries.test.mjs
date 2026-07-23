import assert from "node:assert/strict";
import test from "node:test";

import {
  archiveName,
  binaries,
  checksumName,
  targets,
} from "./release-binaries.mjs";

test("release matrix covers supported desktop and server targets", () => {
  assert.deepEqual(
    targets.map(({ os, arch }) => `${os}/${arch}`),
    [
      "linux/amd64",
      "linux/arm64",
      "darwin/amd64",
      "darwin/arm64",
      "windows/amd64",
      "windows/arm64",
    ],
  );
});

test("asset names are stable and versioned", () => {
  assert.equal(binaries[0].name, "recorder");
  assert.equal(
    archiveName(binaries[0], "0.6.0", targets[0]),
    "recorder-0.6.0-linux-amd64.tar.gz",
  );
  assert.equal(
    archiveName(binaries[0], "0.6.0", targets[4]),
    "recorder-0.6.0-windows-amd64.zip",
  );
  assert.equal(checksumName("0.6.0"), "recorder-0.6.0-checksums.txt");
});
